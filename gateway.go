package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// listenPrivateUnix keeps the management endpoint inside the application's
// private persistent directory. Only the FRP backend joins the host network.
func listenPrivateUnix(socketPath, dataDir string) (net.Listener, error) {
	socketPath, err := filepath.Abs(socketPath)
	if err != nil {
		return nil, err
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(socketPath) != dataDir {
		return nil, errors.New("Unix Socket 必须直接位于本应用的数据目录内")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("Socket 路径已存在且不是 Socket，拒绝覆盖")
		}
		conn, dialErr := net.DialTimeout("unix", socketPath, time.Second)
		if dialErr == nil {
			conn.Close()
			return nil, errors.New("该管理 Socket 已有进程监听")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("无法确认 Socket 是否可清理：%w", dialErr)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

func unixGateway(socketPath string) (*httputil.ReverseProxy, *http.Transport) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "localhost"})
	proxy.Transport = transport
	// Keep the original Host, Origin, cookies and ingress identity headers. The
	// backend retains all existing authentication and same-origin CSRF checks.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "后台服务尚未就绪，请稍后重试", http.StatusServiceUnavailable)
	}
	return proxy, transport
}

func runGateway(listenAddr, socketPath string) error {
	loopback, err := isLoopbackListen(listenAddr)
	if err != nil {
		return err
	}
	if !loopback {
		return errors.New("管理界面转发器只能监听回环地址")
	}
	if !filepath.IsAbs(socketPath) {
		return errors.New("管理 Socket 必须使用绝对路径")
	}
	proxy, transport := unixGateway(socketPath)
	defer transport.CloseIdleConnections()
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := newHTTPServer(&httpServer{Handler: proxy})
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		return server.Shutdown(shutdown)
	}
}

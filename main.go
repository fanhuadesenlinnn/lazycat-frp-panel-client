// Command lzc-frpp-client is the lightweight management backend for a
// frp-panel client running inside a lazycat (懒猫微服) application container.
//
// It serves a small embedded Chinese web UI, keeps the four connection
// settings (id / secret / API URL / RPC URL) in a persistent directory, and
// supervises the upstream frp-panel client as a child process.
//
// It is written against the Go standard library only so that the resulting
// binary can be built and audited without any third-party dependency.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/config"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/logbuf"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/redact"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/web"
)

// version is injected at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败："+err.Error())
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("lzc-frpp-client", flag.ExitOnError)
	var (
		listenAddr    = fs.String("listen", envOr("LZCFRPP_LISTEN", "127.0.0.1:18080"), "管理界面监听地址")
		dataDir       = fs.String("data-dir", envOr("LZCFRPP_DATA_DIR", defaultDataDir()), "持久化数据目录")
		clientBin     = fs.String("client-bin", "", "frp-panel 客户端可执行文件路径（默认自动查找）")
		unsafeNoAuth  = fs.Bool("insecure-no-auth", false, "关闭鉴权（仅限本机调试，必须绑定回环地址）")
		requireAdmin  = fs.Bool("require-admin", envBool("LZCFRPP_REQUIRE_ADMIN", true), "要求懒猫账号为管理员")
		trustIngress  = fs.Bool("trust-ingress", envBool("LZCFRPP_TRUST_INGRESS", false), "信任懒猫 ingress 注入的 X-HC-User-ID / X-HC-User-Role 请求头（仅限经懒猫入口访问、且只监听回环地址的部署）")
		allowNonLocal = fs.Bool("allow-non-loopback-listen", false, "允许监听非回环地址（不推荐）")
		probeInterval = fs.Duration("probe-interval", 30*time.Second, "Master 可达性探测间隔")
		extraOrigins  = fs.String("extra-origins", envOr("LZCFRPP_EXTRA_ORIGINS", ""), "额外允许的请求来源，逗号分隔")
		showVersion   = fs.Bool("version", false, "打印版本并退出")
		logCapacity   = fs.Int("log-capacity", 800, "内存中保留的日志行数")
	)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Printf("lzc-frpp-client %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	}

	loopback, err := isLoopbackListen(*listenAddr)
	if err != nil {
		return err
	}
	if !loopback && !*allowNonLocal {
		return fmt.Errorf("拒绝监听 %s：管理接口不允许暴露到回环地址之外，"+
			"如需非常规部署请显式添加 -allow-non-loopback-listen 并自行确保访问控制", *listenAddr)
	}
	if *unsafeNoAuth && !loopback {
		return errors.New("-insecure-no-auth 只能与回环地址一起使用")
	}
	// 只有“用户访问必然经过懒猫 ingress”时才能信任 X-HC-* 请求头。监听
	// 非回环地址时任何人都能直接构造这两个头冒充管理员，因此拒绝启动。
	if *trustIngress && !loopback {
		return errors.New("-trust-ingress 只能与回环地址一起使用：" +
			"监听非回环地址时，任何客户端都能伪造 X-HC-User-ID / X-HC-User-Role 冒充管理员")
	}
	if *trustIngress && *unsafeNoAuth {
		return errors.New("-trust-ingress 与 -insecure-no-auth 不能同时使用")
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return fmt.Errorf("无法创建数据目录 %s：%w", *dataDir, err)
	}
	store, err := config.Open(*dataDir)
	if err != nil {
		return err
	}
	if preset := strings.TrimSpace(os.Getenv("LZCFRPP_ADMIN_TOKEN")); preset != "" {
		if len(preset) < 8 {
			return errors.New("LZCFRPP_ADMIN_TOKEN 至少需要 8 个字符")
		}
		if _, err := store.Update(func(c *config.Config) error {
			if c.AdminToken == "" {
				c.AdminToken = preset
			}
			return nil
		}); err != nil {
			return fmt.Errorf("无法写入 LZCFRPP_ADMIN_TOKEN：%w", err)
		}
	}
	token, err := store.EnsureAdminToken()
	if err != nil {
		return fmt.Errorf("无法生成访问令牌：%w", err)
	}

	red := redact.New(store.Get().ClientSecret, token)
	ring := logbuf.New(*logCapacity, 8192, red.Redact)

	binPath, err := resolveClientBinary(*clientBin)
	if err != nil {
		ring.Write([]byte("WARN 未找到 frp-panel 客户端可执行文件：" + err.Error() + "\n"))
	}

	if binPath != "" {
		binPath, err = filepath.Abs(binPath)
		if err != nil {
			return err
		}
	}

	srv, err := web.New(store, nil, ring, red, web.Options{
		Version:      version,
		DataDir:      *dataDir,
		ClientBinary: binPath,
		StartedAt:    time.Now(),
		RequireAdmin: *requireAdmin,
		TrustIngress: *trustIngress,
		ExtraOrigins: splitList(*extraOrigins),
		DisableAuth:  *unsafeNoAuth,
		LogCapacity:  *logCapacity,
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		return fmt.Errorf("无法监听 %s：%w", *listenAddr, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.RunProbes(ctx, *probeInterval)

	httpSrv := &httpServer{Handler: srv.Handler()}
	server := newHTTPServer(httpSrv)

	ring.Write([]byte(fmt.Sprintf("INFO lzc-frpp-client %s 已启动，监听 %s，数据目录 %s\n",
		version, *listenAddr, *dataDir)))
	ring.Write([]byte(fmt.Sprintf("INFO 客户端可执行文件：%s\n", orNone(binPath))))
	if *unsafeNoAuth {
		ring.Write([]byte("WARN 已通过 -insecure-no-auth 关闭鉴权，仅可用于本机调试\n"))
	}
	if *trustIngress {
		ring.Write([]byte("INFO 已信任懒猫 ingress 注入的登录身份（X-HC-User-ID / X-HC-User-Role）\n"))
	}
	if !loopback {
		ring.Write([]byte("WARN 管理接口监听在非回环地址，请自行确保网络隔离\n"))
	}

	errCh := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, errServerClosed) {
			errCh <- err
		}
	}()

	srv.AutoStart()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		_ = srv.Close(context.Background())
		return err
	case sig := <-sigCh:
		ring.Write([]byte(fmt.Sprintf("INFO 收到信号 %s，开始优雅退出\n", sig)))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	if err := srv.Close(shutdownCtx); err != nil {
		return err
	}
	return nil
}

func defaultDataDir() string {
	if st, err := os.Stat("/lzcapp/var"); err == nil && st.IsDir() {
		return "/lzcapp/var/frp-panel-client"
	}
	return "./data"
}

func isLoopbackListen(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("监听地址 %q 无效：%w", addr, err)
	}
	switch strings.ToLower(host) {
	case "localhost":
		return true, nil
	case "":
		return false, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false, nil
	}
	return ip.IsLoopback(), nil
}

// resolveClientBinary finds the bundled upstream client binary. The program
// path is never taken from user input; only these fixed candidates (plus an
// explicit operator override) are considered.
func resolveClientBinary(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := strings.TrimSpace(os.Getenv("LZCFRPP_CLIENT_BIN")); env != "" {
		return env, nil
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	var names []string
	if runtime.GOOS == "linux" {
		names = append(names,
			"frp-panel-client-linux-"+runtime.GOARCH,
			"frp-panel-linux-"+runtime.GOARCH,
		)
	}
	names = append(names,
		"frp-panel-client-"+runtime.GOOS+"-"+runtime.GOARCH,
		"frp-panel-client",
		"frp-panel",
		"frppc",
		"frpp",
	)
	dirs := []string{"."}
	if exe, err := os.Executable(); err == nil {
		dirs = append([]string{filepath.Dir(exe)}, dirs...)
	}
	for _, dir := range dirs {
		for _, name := range names {
			candidate := filepath.Join(dir, name+suffix)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			}
		}
	}
	for _, name := range []string{"frp-panel-client", "frp-panel", "frppc", "frpp"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("在可执行文件同目录与 PATH 中都没有找到 frp-panel 客户端，" +
		"请使用 -client-bin 或环境变量 LZCFRPP_CLIENT_BIN 指定")
}

func orNone(s string) string {
	if s == "" {
		return "(未找到)"
	}
	return s
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

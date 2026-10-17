package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/config"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/logbuf"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/supervisor"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/upstream"
)

type connectionRuntime struct {
	sup   *supervisor.Manager
	logs  *logbuf.Ring
	probe *Probe
}

func (s *Server) newManager(id string, logs *logbuf.Ring) *supervisor.Manager {
	return supervisor.New(func() (supervisor.Spec, error) {
		cfg, ok := s.store.Connection(id)
		if !ok {
			return supervisor.Spec{}, errors.New("连接不存在")
		}
		if !cfg.Configured() {
			return supervisor.Spec{}, errors.New("尚未完成配置：请填写客户端 ID、密钥、API URL 与 RPC URL")
		}
		if err := cfg.Validate(); err != nil {
			return supervisor.Spec{}, err
		}
		dir := s.opts.DataDir
		if id != config.DefaultConnection {
			dir = filepath.Join(dir, "connections", id)
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return supervisor.Spec{}, err
		}
		if err := os.MkdirAll(absDir, 0o700); err != nil {
			return supervisor.Spec{}, err
		}
		spec := upstream.LaunchSpec{ClientID: cfg.ClientID, Secret: cfg.ClientSecret,
			APIURL: cfg.APIURL, RPCURL: cfg.RPCURL, LogLevel: cfg.LogLevel,
			EnableRemoteShell: cfg.EnableRemoteShell, EnableFunctions: cfg.EnableFunctions}
		env := append(spec.Env(), "CLIENT_WORKER_WORKERD_WORK_DIR="+filepath.Join(absDir, "workerd"), "DEBUG_PROFILER_ENABLED=false", "FRPP_RUNTIME_STATUS_FILE="+filepath.Join(absDir, "frpp-runtime.json"))
		return supervisor.Spec{Path: s.opts.ClientBinary, Args: spec.Argv(), Env: env, Dir: absDir}, nil
	}, supervisor.Options{Sink: logs})
}

func (s *Server) connection(id string) (*connectionRuntime, config.Config, error) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if s.closed {
		return nil, config.Config{}, errors.New("管理器已关闭")
	}
	cfg, ok := s.store.Connection(id)
	if !ok {
		return nil, config.Config{}, errors.New("连接不存在")
	}
	rt := s.runtimes[id]
	if rt == nil {
		logs := logbuf.New(s.opts.LogCapacity, 8192, s.red.Redact)
		rt = &connectionRuntime{logs: logs, probe: NewProbe(cfg.APIURL)}
		rt.sup = s.newManager(id, logs)
		s.runtimes[id] = rt
	}
	return rt, cfg, nil
}

func connectionID(r *http.Request) string {
	id := r.URL.Query().Get("connection")
	if id == "" {
		return config.DefaultConnection
	}
	return id
}

func (s *Server) requestConnection(w http.ResponseWriter, r *http.Request) (*connectionRuntime, config.Config, bool) {
	rt, cfg, err := s.connection(connectionID(r))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return nil, config.Config{}, false
	}
	return rt, cfg, true
}

func (s *Server) apiAddConnection(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireMutation(w, r); !ok {
		return
	}
	s.connectionOps.Lock()
	defer s.connectionOps.Unlock()
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	id, err := s.store.AddConnection(req.Name)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	_, cfg, err := s.connection(id)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": id, "config": cfg.Public()})
}

func (s *Server) apiDeleteConnection(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireMutation(w, r); !ok {
		return
	}
	s.connectionOps.Lock()
	defer s.connectionOps.Unlock()
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if req.ID == config.DefaultConnection {
		fail(w, http.StatusBadRequest, errors.New("默认连接不能删除，可停止或修改配置"))
		return
	}
	rt, _, err := s.connection(req.ID)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	// Stop first, but only permanently close after persistence succeeds. A failed
	// disk write must leave a usable manager for the still-existing connection.
	if err := rt.sup.Stop(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.runtimeMu.Lock()
	err = s.store.DeleteConnection(req.ID)
	if err == nil {
		delete(s.runtimes, req.ID)
	}
	s.runtimeMu.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	_ = rt.sup.Close(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RunProbes probes every connection concurrently, including connections added
// after startup. The next round waits for the preceding round to finish.
func (s *Server) RunProbes(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		var wg sync.WaitGroup
		for _, id := range s.store.ConnectionIDs() {
			rt, _, err := s.connection(id)
			if err != nil {
				continue
			}
			wg.Add(1)
			go func() { defer wg.Done(); rt.probe.Run(ctx) }()
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) AutoStart() {
	for _, id := range s.store.ConnectionIDs() {
		rt, cfg, err := s.connection(id)
		if err != nil || !cfg.AutoStart {
			continue
		}
		if err := rt.sup.Start(); err != nil {
			_, _ = rt.logs.Write([]byte("ERROR 自动启动失败：" + s.red.Redact(err.Error()) + "\n"))
		} else {
			_, _ = rt.logs.Write([]byte("INFO 已按配置自动启动客户端\n"))
		}
	}
}

// Close prevents new runtimes and stops all children concurrently.
func (s *Server) Close(ctx context.Context) error {
	s.connectionOps.Lock()
	defer s.connectionOps.Unlock()
	s.runtimeMu.Lock()
	s.closed = true
	runtimes := make([]*connectionRuntime, 0, len(s.runtimes))
	for _, rt := range s.runtimes {
		runtimes = append(runtimes, rt)
	}
	s.runtimeMu.Unlock()
	var wg sync.WaitGroup
	errs := make(chan error, len(runtimes))
	for _, rt := range runtimes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rt.sup.Close(ctx); err != nil {
				errs <- fmt.Errorf("停止连接失败：%w", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	var all []error
	for err := range errs {
		all = append(all, err)
	}
	return errors.Join(all...)
}

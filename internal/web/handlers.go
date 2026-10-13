package web

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/config"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/supervisor"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/upstream"
)

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r); !ok {
		return
	}
	rt, cfg, ok := s.requestConnection(w, r)
	if !ok {
		return
	}
	snap := rt.sup.Status()
	now := time.Now()

	process := map[string]any{
		"state":                     snap.State,
		"pid":                       snap.PID,
		"started_at":                snap.StartedAt,
		"uptime_seconds":            uptimeSeconds(snap.StartedAt, snap.State, now),
		"restarts":                  snap.Restarts,
		"consecutive_fast_failures": snap.ConsecutiveFastFailures,
		"unstable":                  snap.Unstable,
		"next_restart_at":           snap.NextRestartAt,
		"last_exit":                 snap.LastExit,
		"last_error":                snap.LastError,
	}
	remote := map[string]any{
		"probe":        rt.probe.Last(),
		"probe_target": rt.probe.Target(),
		"claim":        "unknown",
		"note": "本应用无法从本机确认客户端是否已在 Master 上注册成功。" +
			"下方探针只表示本容器能否建立到 Master 的网络连接，不能作为“在线”的凭据。",
	}
	connections := []map[string]any{}
	for _, id := range s.store.ConnectionIDs() {
		conn, settings, err := s.connection(id)
		if err != nil {
			continue
		}
		connections = append(connections, map[string]any{"id": id, "config": settings.Public(), "process": conn.sup.Status()})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connection_id": connectionID(r),
		"connections":   connections,
		"ok":            true,
		"version":       s.opts.Version,
		"now":           now,
		"process":       process,
		"remote":        remote,
		"config":        cfg.Public(),
		"runtime": map[string]any{
			"data_dir":      s.opts.DataDir,
			"client_binary": s.opts.ClientBinary,
			"server_start":  s.opts.StartedAt,
			"require_admin": s.opts.RequireAdmin,
		},
	})
}

func uptimeSeconds(started time.Time, state supervisor.State, now time.Time) int64 {
	if started.IsZero() || state != supervisor.StateRunning {
		return 0
	}
	return int64(now.Sub(started).Seconds())
}

func (s *Server) apiGetConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r); !ok {
		return
	}
	_, cfg, ok := s.requestConnection(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": cfg.Public()})
}

// apiLogin exchanges the local access token for an HttpOnly session cookie.
//
// The access token is only ever sent in a JSON body over the same origin; it
// never has to stay in the address bar, and the resulting session cookie is
// not readable from JavaScript.
func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireCSRF(w, r) {
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	want := s.store.Get().AdminToken
	if want == "" {
		fail(w, http.StatusConflict, errors.New("本实例没有配置访问令牌"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Token)), []byte(want)) != 1 {
		fail(w, http.StatusUnauthorized, errors.New("访问令牌不正确"))
		return
	}
	id, expires, err := s.newSession("token")
	if err != nil {
		fail(w, http.StatusInternalServerError, errors.New("无法创建会话"))
		return
	}
	s.setSessionCookie(w, r, id, expires)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": expires})
}

// apiLogout drops the current session cookie.
func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireMutation(w, r); !ok {
		return
	}
	s.dropSession(r)
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type configRequest struct {
	Name              *string `json:"name"`
	ClientID          *string `json:"client_id"`
	ClientSecret      *string `json:"client_secret"`
	ClearSecret       *bool   `json:"clear_secret"`
	APIURL            *string `json:"api_url"`
	RPCURL            *string `json:"rpc_url"`
	AutoStart         *bool   `json:"auto_start"`
	EnableRemoteShell *bool   `json:"enable_remote_shell"`
	EnableFunctions   *bool   `json:"enable_functions"`
	LogLevel          *string `json:"log_level"`
}

func (s *Server) apiSetConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireMutation(w, r); !ok {
		return
	}
	s.connectionOps.Lock()
	defer s.connectionOps.Unlock()
	rt, before, ok := s.requestConnection(w, r)
	if !ok {
		return
	}
	var req configRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	next, err := s.store.UpdateConnection(connectionID(r), func(c *config.Config) error {
		if req.Name != nil {
			c.Name = *req.Name
		}
		if req.ClientID != nil {
			c.ClientID = *req.ClientID
		}
		if req.ClientSecret != nil && *req.ClientSecret != "" {
			c.ClientSecret = *req.ClientSecret
		}
		if req.ClearSecret != nil && *req.ClearSecret {
			c.ClientSecret = ""
		}
		if req.APIURL != nil {
			c.APIURL = *req.APIURL
		}
		if req.RPCURL != nil {
			c.RPCURL = *req.RPCURL
		}
		if req.AutoStart != nil {
			c.AutoStart = *req.AutoStart
		}
		if req.EnableRemoteShell != nil {
			c.EnableRemoteShell = *req.EnableRemoteShell
		}
		if req.EnableFunctions != nil {
			c.EnableFunctions = *req.EnableFunctions
		}
		if req.LogLevel != nil {
			c.LogLevel = *req.LogLevel
		}
		return nil
	})
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.red.AddLiterals(next.ClientSecret)
	if before.APIURL != next.APIURL {
		rt.probe.SetTarget(next.APIURL)
	}

	restarted := false
	if restartNeeded(before, next) {
		if snap := rt.sup.Status(); snap.State == supervisor.StateRunning || snap.State == supervisor.StateBackoff {
			if err := rt.sup.Restart(r.Context()); err != nil {
				fail(w, http.StatusInternalServerError, fmt.Errorf("配置已保存，但重启客户端失败：%w", err))
				return
			}
			restarted = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "config": next.Public(), "restarted": restarted,
	})
}

func restartNeeded(a, b config.Config) bool {
	return a.ClientID != b.ClientID ||
		a.ClientSecret != b.ClientSecret ||
		a.APIURL != b.APIURL ||
		a.RPCURL != b.RPCURL ||
		a.LogLevel != b.LogLevel ||
		a.EnableRemoteShell != b.EnableRemoteShell ||
		a.EnableFunctions != b.EnableFunctions
}

func (s *Server) apiPreviewCommand(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireMutation(w, r); !ok {
		return
	}
	var req struct {
		Command string `json:"command"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	res, err := upstream.ParseCommand(req.Command)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// upstream.Result never marshals the secret, so it is safe to return as is.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"preview":         res,
		"secret_detected": res.SecretDetected(),
	})
}

func (s *Server) apiApplyCommand(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireMutation(w, r); !ok {
		return
	}
	s.connectionOps.Lock()
	defer s.connectionOps.Unlock()
	rt, before, ok := s.requestConnection(w, r)
	if !ok {
		return
	}
	var req struct {
		Command   string  `json:"command"`
		Name      *string `json:"name"`
		AutoStart *bool   `json:"auto_start"`
		Start     bool    `json:"start"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	res, err := upstream.ParseCommand(req.Command)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(res.Missing) > 0 {
		fail(w, http.StatusBadRequest, errors.New("命令中缺少："+strings.Join(res.Missing, "、")))
		return
	}

	next, err := s.store.UpdateConnection(connectionID(r), func(c *config.Config) error {
		if req.Name != nil {
			c.Name = *req.Name
		}
		c.ClientID = res.ClientID
		c.APIURL = res.APIURL
		c.RPCURL = res.RPCURL
		if res.Secret != "" {
			c.ClientSecret = res.Secret
		}
		if req.AutoStart != nil {
			c.AutoStart = *req.AutoStart
		}
		return nil
	})
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.red.AddLiterals(next.ClientSecret)
	if before.APIURL != next.APIURL {
		rt.probe.SetTarget(next.APIURL)
	}

	started := false
	if req.Start {
		if snap := rt.sup.Status(); snap.State == supervisor.StateRunning {
			if err := rt.sup.Restart(r.Context()); err != nil {
				fail(w, http.StatusInternalServerError, fmt.Errorf("配置已保存，但重启客户端失败：%w", err))
				return
			}
		} else if err := rt.sup.Start(); err != nil {
			fail(w, http.StatusInternalServerError, fmt.Errorf("配置已保存，但启动客户端失败：%w", err))
			return
		}
		started = true
	} else if restartNeeded(before, next) {
		if snap := rt.sup.Status(); snap.State == supervisor.StateRunning || snap.State == supervisor.StateBackoff {
			if err := rt.sup.Restart(r.Context()); err != nil {
				fail(w, http.StatusInternalServerError, fmt.Errorf("配置已保存，但重启客户端失败：%w", err))
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"config":   next.Public(),
		"warnings": res.Warnings,
		"started":  started,
	})
}

func (s *Server) apiControl(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireMutation(w, r); !ok {
		return
	}
	s.connectionOps.Lock()
	defer s.connectionOps.Unlock()
	rt, _, ok := s.requestConnection(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	switch req.Action {
	case "start":
		if err := rt.sup.Start(); err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
	case "stop":
		if err := rt.sup.Stop(r.Context()); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
	case "restart":
		if err := rt.sup.Restart(r.Context()); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
	default:
		fail(w, http.StatusBadRequest, errors.New("action 只能是 start/stop/restart"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": rt.sup.Status()})
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r); !ok {
		return
	}
	rt, _, ok := s.requestConnection(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if raw := q.Get("since"); raw != "" {
		since, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			fail(w, http.StatusBadRequest, errors.New("since 必须是数字"))
			return
		}
		lines := rt.logs.Since(since)
		dropped := false
		if len(lines) > 0 && lines[0].Seq > since+1 {
			dropped = true
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "lines": lines, "next": rt.logs.LastSeq(), "dropped": dropped,
		})
		return
	}
	tail := 200
	if raw := q.Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 2000 {
			fail(w, http.StatusBadRequest, errors.New("tail 必须是 0-2000 之间的数字"))
			return
		}
		tail = n
	}
	lines := rt.logs.Tail(tail)
	next := rt.logs.LastSeq()
	if len(lines) > 0 {
		next = lines[len(lines)-1].Seq
	}
	if tail == 0 {
		next = rt.logs.LastSeq()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lines": lines, "next": next})
}

func (s *Server) apiClearLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireMutation(w, r); !ok {
		return
	}
	rt, _, ok := s.requestConnection(w, r)
	if !ok {
		return
	}
	rt.logs.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

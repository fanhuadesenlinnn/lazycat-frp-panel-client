// Package web serves the embedded Chinese management UI and its JSON API.
//
// Security model:
//   - the listener is bound to loopback and is only reachable through the
//     lazycat ingress, which already authenticates the user;
//   - the primary identity source is the X-HC-User-ID header injected by
//     lzc-ingress (it is cleared for unauthenticated requests);
//   - a locally generated access token is accepted as a fallback for direct
//     (Docker / curl) usage;
//   - every state changing request must carry a same-origin Origin/Referer and
//     a custom X-Requested-With header, and must be JSON, which removes the
//     classic cross-site form POST.
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/config"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/logbuf"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/supervisor"
)

//go:embed assets/*
var assets embed.FS

// RequestHeaderMark is required on every mutating request. A browser can not
// send a custom header cross-origin without a CORS preflight, so this blocks
// CSRF even if the Origin check were bypassed.
const RequestHeaderMark = "X-Requested-With"

// RequestHeaderMarkValue is the expected value of RequestHeaderMark.
const RequestHeaderMarkValue = "lzc-frpp-client"

// sessionCookieName carries the session created by an access token login.
const sessionCookieName = "lzc-frpp-session"

// Options configures the HTTP server.
type Options struct {
	Version      string
	DataDir      string
	ClientBinary string
	StartedAt    time.Time

	// RequireAdmin rejects logins whose X-HC-User-Role is not ADMIN.
	RequireAdmin bool
	// TrustIngress enables the X-HC-* identity headers. It must only be set
	// when the process is reachable exclusively through the lazycat ingress
	// (loopback in the app container, or a private Unix Socket reached through
	// the app gateway). Standalone
	// deployments leave it off so that a forged header can not impersonate an
	// administrator.
	TrustIngress bool
	// ExtraOrigins are additional allowed Origin values (scheme://host[:port]).
	ExtraOrigins []string
	// DisableAuth is only honoured for loopback listeners (checked by main).
	DisableAuth bool
	// LogCapacity is the number of log lines kept in memory.
	LogCapacity int
	// SessionTTL is how long an access-token login stays valid.
	SessionTTL time.Duration
}

type session struct {
	user    string
	expires time.Time
}

// Server is the HTTP front end.
type Server struct {
	connectionOps sync.Mutex
	runtimeMu     sync.Mutex
	runtimes      map[string]*connectionRuntime
	closed        bool
	mu            sync.Mutex
	session       map[string]session

	store *config.Store
	red   Redactor
	logs  *logbuf.Ring
	sup   *supervisor.Manager
	probe *Probe
	opts  Options
	html  http.Handler
}

// Redactor is the subset of redact.Redactor used here.
type Redactor interface {
	Redact(string) string
	AddLiterals(...string)
}

// New wires the server together.
func New(store *config.Store, sup *supervisor.Manager, ring *logbuf.Ring, red Redactor, opts Options) (*Server, error) {
	if opts.LogCapacity <= 0 {
		opts.LogCapacity = 800
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = time.Now()
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 12 * time.Hour
	}
	if ring == nil {
		ring = logbuf.New(opts.LogCapacity, 8192, red.Redact)
	}
	s := &Server{
		session: make(map[string]session),
		store:   store,
		red:     red,
		logs:    ring,
		sup:     sup,
		opts:    opts,
	}
	s.probe = NewProbe(store.Get().APIURL)
	s.runtimes = make(map[string]*connectionRuntime)
	if sup == nil {
		sup = s.newManager(config.DefaultConnection, ring)
		s.sup = sup
	}
	s.runtimes[config.DefaultConnection] = &connectionRuntime{sup: sup, logs: ring, probe: s.probe}
	for _, id := range store.ConnectionIDs() {
		cfg, _ := store.Connection(id)
		red.AddLiterals(cfg.ClientSecret)
	}
	assetsFS, err := fs.Sub(assets, "assets")
	if err != nil {
		return nil, err
	}
	s.html = http.FileServer(http.FS(assetsFS))
	return s, nil
}

// LogWriter returns the sink that child process output must be written to.
func (s *Server) LogWriter() *logbuf.Ring { return s.logs }

// Probe exposes the reachability prober (started by main).
func (s *Server) Probe() *Probe { return s.probe }

// Handler builds the route table and middleware chain.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", s.html))

	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("POST /api/connections", s.apiAddConnection)
	mux.HandleFunc("POST /api/connections/delete", s.apiDeleteConnection)
	mux.HandleFunc("GET /api/config", s.apiGetConfig)
	mux.HandleFunc("POST /api/config", s.apiSetConfig)
	mux.HandleFunc("POST /api/login", s.apiLogin)
	mux.HandleFunc("POST /api/logout", s.apiLogout)
	mux.HandleFunc("POST /api/command/preview", s.apiPreviewCommand)
	mux.HandleFunc("POST /api/command/apply", s.apiApplyCommand)
	mux.HandleFunc("POST /api/control", s.apiControl)
	mux.HandleFunc("GET /api/logs", s.apiLogs)
	mux.HandleFunc("POST /api/logs/clear", s.apiClearLogs)

	return s.recoverMW(s.securityHeadersMW(mux))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// The shell page is served without authentication: it contains no data and
	// the platform health check needs a 200 here. Every API call behind it is
	// authenticated, and the page itself renders a login panel when the caller
	// is not authenticated yet.
	s.serveAsset(w, r, "index.html")
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	data, err := assets.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeadersMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// Identity describes who is calling.
type Identity struct {
	User string
	Role string
	Via  string
}

func (s *Server) identify(r *http.Request) (Identity, error) {
	if s.opts.DisableAuth {
		return Identity{User: "local", Role: "ADMIN", Via: "disabled"}, nil
	}
	if user := s.sessionUser(r); user != "" {
		return Identity{User: user, Role: "ADMIN", Via: "session"}, nil
	}
	token := s.store.Get().AdminToken
	if token != "" {
		candidate := r.Header.Get("X-LZCFRPP-Token")
		if candidate == "" {
			candidate = bearerToken(r.Header.Get("Authorization"))
		}
		if candidate != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			return Identity{User: "token", Role: "ADMIN", Via: "token"}, nil
		}
	}
	if !s.opts.TrustIngress {
		return Identity{}, errors.New("未认证：请使用访问令牌登录，或通过懒猫微服入口打开本应用")
	}
	user := strings.TrimSpace(r.Header.Get("X-HC-User-ID"))
	if user != "" {
		role := strings.TrimSpace(r.Header.Get("X-HC-User-Role"))
		if s.opts.RequireAdmin && !strings.EqualFold(role, "ADMIN") {
			return Identity{}, errors.New("当前账号不是管理员，无法管理 frp-panel 客户端")
		}
		return Identity{User: user, Role: role, Via: "lzc-ingress"}, nil
	}
	return Identity{}, errors.New("未检测到登录身份：请从懒猫微服的应用入口打开本页面")
}

func (s *Server) sessionUser(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.session {
		if now.After(sess.expires) {
			delete(s.session, id)
		}
	}
	sess, ok := s.session[cookie.Value]
	if !ok || now.After(sess.expires) {
		return ""
	}
	return sess.user
}

func (s *Server) newSession(user string) (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	id := hex.EncodeToString(buf)
	expires := time.Now().Add(s.opts.SessionTTL)
	s.mu.Lock()
	s.session[id] = session{user: user, expires: expires}
	s.mu.Unlock()
	return id, expires, nil
}

func (s *Server) dropSession(r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	s.mu.Lock()
	delete(s.session, cookie.Value)
	s.mu.Unlock()
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, id string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isSecureRequest(r),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isSecureRequest(r),
	})
}

// isSecureRequest reports whether the browser is talking to us over HTTPS.
// lzc-ingress always sets X-Forwarded-Proto: https.
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func bearerToken(h string) string {
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// requireAuth authenticates and returns the caller identity.
func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	id, err := s.identify(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": err.Error()})
		return Identity{}, false
	}
	return id, true
}

// requireMutation applies the CSRF defences and the authentication check.
func (s *Server) requireMutation(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	id, ok := s.requireAuth(w, r)
	if !ok {
		return Identity{}, false
	}
	if !s.requireCSRF(w, r) {
		return Identity{}, false
	}
	return id, true
}

// requireCSRF applies the cross-site request forgery defences without
// requiring an authenticated caller (used by the token login endpoint).
func (s *Server) requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	if err := s.checkCSRF(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": err.Error()})
		return false
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
			"ok": false, "error": "请求体必须是 application/json",
		})
		return false
	}
	return true
}

func (s *Server) checkCSRF(r *http.Request) error {
	if r.Header.Get(RequestHeaderMark) != RequestHeaderMarkValue {
		return fmt.Errorf("缺少 %s 请求头，已拒绝该请求", RequestHeaderMark)
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		ref := strings.TrimSpace(r.Header.Get("Referer"))
		if ref == "" {
			return errors.New("缺少 Origin/Referer，已拒绝该请求")
		}
		u, err := url.Parse(ref)
		if err != nil {
			return errors.New("Referer 无法解析，已拒绝该请求")
		}
		origin = u.Scheme + "://" + u.Host
	}
	if origin == "null" {
		return errors.New("Origin 为 null，已拒绝该请求")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return errors.New("Origin 无法解析，已拒绝该请求")
	}
	if s.originAllowed(u.Host, r) {
		return nil
	}
	return fmt.Errorf("请求来源 %s 不在允许列表中", u.Host)
}

func (s *Server) originAllowed(originHost string, r *http.Request) bool {
	hosts := []string{r.Host}
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		if i := strings.IndexByte(fh, ','); i >= 0 {
			fh = fh[:i]
		}
		hosts = append(hosts, strings.TrimSpace(fh))
	}
	for _, h := range hosts {
		if h != "" && strings.EqualFold(h, originHost) {
			return true
		}
	}
	for _, allowed := range s.opts.ExtraOrigins {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}
		if strings.EqualFold(allowed, originHost) {
			return true
		}
		if u, err := url.Parse(allowed); err == nil && strings.EqualFold(u.Host, originHost) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(body)
}

// fail writes a JSON error response.
func fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("请求体不是合法的 JSON：" + err.Error())
	}
	return nil
}

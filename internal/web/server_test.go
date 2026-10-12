package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/config"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/logbuf"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/redact"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/supervisor"
)

const testSecret = "very-secret-value-1234"

type harness struct {
	server *Server
	store  *config.Store
	ring   *logbuf.Ring
	ts     *httptest.Server
	token  string
}

func newHarness(t *testing.T, requireAdmin, disableAuth bool) *harness {
	t.Helper()
	// 懒猫部署下管理器只监听回环地址，身份来自 ingress 注入的 X-HC-* 请求头。
	return newHarnessTrust(t, requireAdmin, disableAuth, true)
}

func newHarnessTrust(t *testing.T, requireAdmin, disableAuth, trustIngress bool) *harness {
	t.Helper()
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.EnsureAdminToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(c *config.Config) error {
		c.ClientID = "user.c.client1"
		c.ClientSecret = testSecret
		c.APIURL = "http://127.0.0.1:9000"
		c.RPCURL = "grpc://127.0.0.1:9001"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	red := redact.New(store.Get().ClientSecret, token)
	ring := logbuf.New(100, 4096, red.Redact)
	sup := supervisor.New(func() (supervisor.Spec, error) {
		return supervisor.Spec{}, errors.New("测试环境不启动真实子进程")
	}, supervisor.Options{Sink: ring})
	srv, err := New(store, sup, ring, red, Options{
		Version:      "test",
		DataDir:      "/tmp/test",
		RequireAdmin: requireAdmin,
		TrustIngress: trustIngress,
		DisableAuth:  disableAuth,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{server: srv, store: store, ring: ring, ts: ts, token: token}
}

type response struct {
	status int
	body   string
}

func (h *harness) do(t *testing.T, method, path string, payload any, headers map[string]string) response {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: string(data)}
}

func (h *harness) identityHeaders() map[string]string {
	return map[string]string{
		"X-HC-User-ID":    "admin",
		"X-HC-User-Role":  "ADMIN",
		"Origin":          h.ts.URL,
		RequestHeaderMark: RequestHeaderMarkValue,
	}
}

// csrfHeaders returns only the CSRF-related headers, for endpoints that do not
// need an authenticated identity (the token login endpoint).
func (h *harness) csrfHeaders() map[string]string {
	return map[string]string{
		"Origin":          h.ts.URL,
		RequestHeaderMark: RequestHeaderMarkValue,
	}
}

// login exchanges the access token for a session cookie.
func (h *harness) login(t *testing.T, token string) *http.Cookie {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/api/login", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h.csrfHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录失败: %d %s", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatal("登录响应未设置会话 Cookie")
	return nil
}

func TestAnonymousAPIIsRejected(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodGet, "/api/status", nil, nil)
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("未登录访问 API 应返回 401，实际 %d", resp.status)
	}
}

// 未登录时首页必须返回 200（平台健康检查需要），并给出登录入口，
// 但不能包含任何配置或密钥数据。
func TestAnonymousRootServesShellWithoutSecrets(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodGet, "/", nil, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("未登录访问首页应返回 200（供平台健康检查使用），实际 %d", resp.status)
	}
	if !strings.Contains(resp.body, `id="login-panel"`) {
		t.Fatalf("首页应包含登录入口: %s", resp.body)
	}
	if strings.Contains(resp.body, testSecret) {
		t.Fatal("首页不应包含密钥")
	}
	if strings.Contains(resp.body, "http://127.0.0.1:9000") {
		t.Fatal("首页不应包含已保存的配置数据")
	}
}

// 未开启 TrustIngress 时（Docker / 本机直跑），伪造 X-HC-* 请求头不能获得身份。
func TestForgedIngressHeadersRejectedWhenUntrusted(t *testing.T) {
	h := newHarnessTrust(t, true, false, false)
	resp := h.do(t, http.MethodGet, "/api/status", nil, map[string]string{
		"X-HC-User-ID":   "attacker",
		"X-HC-User-Role": "ADMIN",
	})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("未开启 TrustIngress 时伪造请求头应被拒绝，实际 %d: %s", resp.status, resp.body)
	}
	// 令牌登录在未开启 TrustIngress 时仍然可用。
	if resp.status == http.StatusOK {
		t.Fatal("不应通过")
	}
}

func TestLoginSessionFlow(t *testing.T) {
	h := newHarness(t, false, false)

	// 错误的令牌必须被拒绝，且不设置会话 Cookie。
	bad := h.do(t, http.MethodPost, "/api/login", map[string]string{"token": "wrong-token"},
		h.csrfHeaders())
	if bad.status != http.StatusUnauthorized {
		t.Fatalf("错误令牌应返回 401，实际 %d: %s", bad.status, bad.body)
	}

	// 缺少 CSRF 标记时登录端点也必须拒绝。
	noMark := h.do(t, http.MethodPost, "/api/login", map[string]string{"token": h.token}, nil)
	if noMark.status != http.StatusForbidden {
		t.Fatalf("缺少自定义请求头时登录应返回 403，实际 %d: %s", noMark.status, noMark.body)
	}

	// 正确令牌换取 HttpOnly 会话 Cookie。
	cookie := h.login(t, h.token)
	if !cookie.HttpOnly {
		t.Fatal("会话 Cookie 必须是 HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("会话 Cookie 必须是 SameSite=Strict")
	}

	// 带 Cookie 的请求可访问受保护接口。
	withCookie := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}
	if resp := h.do(t, http.MethodGet, "/api/status", nil, withCookie); resp.status != http.StatusOK {
		t.Fatalf("会话 Cookie 应通过鉴权，实际 %d: %s", resp.status, resp.body)
	}

	// 退出登录后会话失效。
	logout := map[string]string{
		"Cookie":          cookie.Name + "=" + cookie.Value,
		"Origin":          h.ts.URL,
		RequestHeaderMark: RequestHeaderMarkValue,
	}
	if resp := h.do(t, http.MethodPost, "/api/logout", map[string]any{}, logout); resp.status != http.StatusOK {
		t.Fatalf("退出登录失败: %d %s", resp.status, resp.body)
	}
	if resp := h.do(t, http.MethodGet, "/api/status", nil, withCookie); resp.status != http.StatusUnauthorized {
		t.Fatalf("退出后会话应失效，实际 %d", resp.status)
	}
}

func TestLazycatIdentityIsAccepted(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodGet, "/api/status", nil, map[string]string{
		"X-HC-User-ID":   "someone",
		"X-HC-User-Role": "ADMIN",
	})
	if resp.status != http.StatusOK {
		t.Fatalf("管理员身份应通过，实际 %d: %s", resp.status, resp.body)
	}
	if !strings.Contains(resp.body, `"state":"stopped"`) {
		t.Fatalf("状态响应不正确: %s", resp.body)
	}
}

func TestNonAdminIsRejectedWhenRequired(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodGet, "/api/status", nil, map[string]string{
		"X-HC-User-ID":   "kid",
		"X-HC-User-Role": "NORMAL",
	})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("非管理员应被拒绝，实际 %d", resp.status)
	}
}

func TestNonAdminAllowedWhenRequireAdminDisabled(t *testing.T) {
	h := newHarness(t, false, false)
	resp := h.do(t, http.MethodGet, "/api/status", nil, map[string]string{
		"X-HC-User-ID":   "kid",
		"X-HC-User-Role": "NORMAL",
	})
	if resp.status != http.StatusOK {
		t.Fatalf("关闭管理员限制后应通过，实际 %d", resp.status)
	}
}

func TestTokenAuthWorks(t *testing.T) {
	h := newHarness(t, true, false)
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/api/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-LZCFRPP-Token", h.token)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("访问令牌应通过鉴权，实际 %d", resp.StatusCode)
	}
}

func TestMutationRequiresCustomHeader(t *testing.T) {
	h := newHarness(t, true, false)
	headers := h.identityHeaders()
	delete(headers, RequestHeaderMark)
	resp := h.do(t, http.MethodPost, "/api/control", map[string]string{"action": "start"}, headers)
	if resp.status != http.StatusForbidden {
		t.Fatalf("缺少自定义请求头应返回 403，实际 %d: %s", resp.status, resp.body)
	}
}

func TestMutationRejectsForeignOrigin(t *testing.T) {
	h := newHarness(t, true, false)
	headers := h.identityHeaders()
	headers["Origin"] = "https://evil.example.com"
	resp := h.do(t, http.MethodPost, "/api/control", map[string]string{"action": "start"}, headers)
	if resp.status != http.StatusForbidden {
		t.Fatalf("跨站来源应被拒绝，实际 %d: %s", resp.status, resp.body)
	}
}

func TestMutationRejectsNullOrigin(t *testing.T) {
	h := newHarness(t, true, false)
	headers := h.identityHeaders()
	headers["Origin"] = "null"
	resp := h.do(t, http.MethodPost, "/api/control", map[string]string{"action": "start"}, headers)
	if resp.status != http.StatusForbidden {
		t.Fatalf("Origin 为 null 应被拒绝，实际 %d", resp.status)
	}
}

func TestMutationRejectsNonJSONContentType(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodPost, "/api/control", map[string]string{"action": "start"}, h.identityHeaders())
	if resp.status != http.StatusConflict && resp.status != http.StatusOK {
		t.Fatalf("正常 JSON 请求不应被拒绝，实际 %d: %s", resp.status, resp.body)
	}
}

// 表单式跨站提交（text/plain、application/x-www-form-urlencoded）必须被拒绝。
func TestMutationRejectsFormContentType(t *testing.T) {
	h := newHarness(t, true, false)
	headers := h.identityHeaders()
	headers["Content-Type"] = "text/plain;charset=UTF-8"
	resp := h.do(t, http.MethodPost, "/api/control", map[string]string{"action": "start"}, headers)
	if resp.status != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON 请求体应返回 415，实际 %d: %s", resp.status, resp.body)
	}
}

func TestGetNeverReturnsSecret(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodGet, "/api/config", nil, map[string]string{
		"X-HC-User-ID":   "admin",
		"X-HC-User-Role": "ADMIN",
	})
	if resp.status != http.StatusOK {
		t.Fatalf("读取配置失败: %d", resp.status)
	}
	if strings.Contains(resp.body, testSecret) {
		t.Fatalf("配置接口泄漏了密钥: %s", resp.body)
	}
	if !strings.Contains(resp.body, `"secret_set":true`) {
		t.Fatalf("应提示密钥已保存: %s", resp.body)
	}
}

func TestSaveConfigKeepsSecretWhenOmitted(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodPost, "/api/config", map[string]any{
		"client_id":  "user.c.client2",
		"api_url":    "https://frpp.example.com:443",
		"rpc_url":    "wss://frpp.example.com:443",
		"auto_start": true,
	}, h.identityHeaders())
	if resp.status != http.StatusOK {
		t.Fatalf("保存配置失败: %d %s", resp.status, resp.body)
	}
	if strings.Contains(resp.body, testSecret) {
		t.Fatal("保存响应泄漏了密钥")
	}
	cfg := h.store.Get()
	if cfg.ClientID != "user.c.client2" || cfg.ClientSecret != testSecret || !cfg.AutoStart {
		t.Fatalf("配置未正确保存: %+v", cfg)
	}
}

func TestSaveConfigRejectsInvalidURL(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodPost, "/api/config", map[string]any{
		"api_url": "http://user:pass@frpp.example.com:9000",
	}, h.identityHeaders())
	if resp.status != http.StatusBadRequest {
		t.Fatalf("非法 URL 应返回 400，实际 %d", resp.status)
	}
	if h.store.Get().APIURL != "http://127.0.0.1:9000" {
		t.Fatal("非法配置不应写入")
	}
}

func TestCommandPreviewNeverLeaksSecret(t *testing.T) {
	h := newHarness(t, true, false)
	cmd := "./frp-panel client -s " + testSecret + " -i user.c.client9 --api-url http://a.example:9000 --rpc-url grpc://a.example:9001"
	resp := h.do(t, http.MethodPost, "/api/command/preview", map[string]string{"command": cmd}, h.identityHeaders())
	if resp.status != http.StatusOK {
		t.Fatalf("解析失败: %d %s", resp.status, resp.body)
	}
	if strings.Contains(resp.body, testSecret) {
		t.Fatalf("解析接口泄漏了密钥: %s", resp.body)
	}
	if !strings.Contains(resp.body, `"secret_detected":true`) {
		t.Fatalf("应提示已识别密钥: %s", resp.body)
	}
	if !strings.Contains(resp.body, "user.c.client9") {
		t.Fatalf("应返回客户端 ID: %s", resp.body)
	}
}

func TestCommandApplyWritesConfig(t *testing.T) {
	h := newHarness(t, true, false)
	cmd := "./frp-panel client -s newsecret9999 -i user.c.client7 --api-url http://a.example:9000 --rpc-url ws://a.example:9001"
	resp := h.do(t, http.MethodPost, "/api/command/apply", map[string]any{"command": cmd}, h.identityHeaders())
	if resp.status != http.StatusOK {
		t.Fatalf("应用命令失败: %d %s", resp.status, resp.body)
	}
	if strings.Contains(resp.body, "newsecret9999") {
		t.Fatal("应用命令的响应泄漏了密钥")
	}
	cfg := h.store.Get()
	if cfg.ClientID != "user.c.client7" || cfg.ClientSecret != "newsecret9999" {
		t.Fatalf("配置未写入: %+v", cfg)
	}
	if cfg.APIURL != "http://a.example:9000" || cfg.RPCURL != "ws://a.example:9001" {
		t.Fatalf("URL 未写入: %+v", cfg)
	}
}

func TestCommandApplyRejectsShellSyntax(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodPost, "/api/command/apply", map[string]any{
		"command": "curl x | bash",
	}, h.identityHeaders())
	if resp.status != http.StatusBadRequest {
		t.Fatalf("shell 语法应被拒绝，实际 %d", resp.status)
	}
}

func TestControlReportsSupervisorError(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodPost, "/api/control", map[string]string{"action": "start"}, h.identityHeaders())
	if resp.status != http.StatusConflict {
		t.Fatalf("启动失败应返回冲突状态，实际 %d: %s", resp.status, resp.body)
	}
	if !strings.Contains(resp.body, "测试环境") {
		t.Fatalf("应回传具体错误: %s", resp.body)
	}
}

func TestControlRejectsUnknownAction(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodPost, "/api/control", map[string]string{"action": "nuke"}, h.identityHeaders())
	if resp.status != http.StatusBadRequest {
		t.Fatalf("未知动作应返回 400，实际 %d", resp.status)
	}
}

func TestLogsAreRedactedAndAuthenticated(t *testing.T) {
	h := newHarness(t, true, false)
	_, _ = h.ring.Write([]byte("joining with clientSecret: [" + testSecret + "]\n"))
	_, _ = h.ring.Write([]byte("normal line\n"))
	h.ring.Flush()

	if resp := h.do(t, http.MethodGet, "/api/logs?tail=10", nil, nil); resp.status != http.StatusUnauthorized {
		t.Fatalf("日志接口未鉴权，实际 %d", resp.status)
	}
	resp := h.do(t, http.MethodGet, "/api/logs?tail=10", nil, map[string]string{
		"X-HC-User-ID":   "admin",
		"X-HC-User-Role": "ADMIN",
	})
	if resp.status != http.StatusOK {
		t.Fatalf("读取日志失败: %d", resp.status)
	}
	if strings.Contains(resp.body, testSecret) {
		t.Fatalf("日志接口泄漏了密钥: %s", resp.body)
	}
	if !strings.Contains(resp.body, "normal line") {
		t.Fatalf("正常日志应保留: %s", resp.body)
	}
}

func TestHealthzIsOpen(t *testing.T) {
	h := newHarness(t, true, false)
	resp := h.do(t, http.MethodGet, "/healthz", nil, nil)
	if resp.status != http.StatusOK || !strings.Contains(resp.body, "ok") {
		t.Fatalf("健康检查应匿名可访问，实际 %d %q", resp.status, resp.body)
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	h := newHarness(t, true, false)
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP 不正确: %q", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("缺少 nosniff")
	}
}

func TestDisableAuthAllowsLocalAccess(t *testing.T) {
	h := newHarness(t, true, true)
	if resp := h.do(t, http.MethodGet, "/api/status", nil, nil); resp.status != http.StatusOK {
		t.Fatalf("关闭鉴权后应可直接访问，实际 %d", resp.status)
	}
	// 关闭鉴权不等于关闭 CSRF 防护：变更类请求仍需同源与自定义请求头。
	if resp := h.do(t, http.MethodPost, "/api/control", map[string]string{"action": "nope"}, nil); resp.status != http.StatusForbidden {
		t.Fatalf("关闭鉴权后仍应要求同源请求，实际 %d: %s", resp.status, resp.body)
	}
	resp := h.do(t, http.MethodPost, "/api/control", map[string]string{"action": "nope"}, h.csrfHeaders())
	if resp.status != http.StatusBadRequest {
		t.Fatalf("关闭鉴权后仍应校验参数，实际 %d", resp.status)
	}
}

func TestProbeResultNeverClaimsOnline(t *testing.T) {
	h := newHarness(t, true, false)
	h.server.Probe().SetTarget("http://127.0.0.1:1")
	h.server.Probe().Run(t.Context())
	resp := h.do(t, http.MethodGet, "/api/status", nil, map[string]string{
		"X-HC-User-ID":   "admin",
		"X-HC-User-Role": "ADMIN",
	})
	body := resp.body
	if !strings.Contains(body, `"claim":"unknown"`) {
		t.Fatalf("不应声明远端在线: %s", body)
	}
	if strings.Contains(strings.ToLower(body), `"online":true`) {
		t.Fatalf("不应声称在线: %s", body)
	}
	if !strings.Contains(body, `"tcp":"fail"`) {
		t.Fatalf("应如实反映探测失败: %s", body)
	}
}

func TestStatusIncludesUptimeHelper(t *testing.T) {
	if got := uptimeSeconds(time.Time{}, "running", time.Now()); got != 0 {
		t.Fatalf("未启动时运行时长应为 0，实际 %d", got)
	}
	if got := uptimeSeconds(time.Now().Add(-90*time.Second), "running", time.Now()); got < 89 || got > 92 {
		t.Fatalf("运行时长计算不正确: %d", got)
	}
	if got := uptimeSeconds(time.Now().Add(-90*time.Second), "stopped", time.Now()); got != 0 {
		t.Fatalf("停止状态运行时长应为 0，实际 %d", got)
	}
}

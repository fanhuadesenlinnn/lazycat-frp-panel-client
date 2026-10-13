package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/config"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/logbuf"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/redact"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/supervisor"
)

// Act as a small upstream client when the manager launches this test executable.
// It emits its identity and secret so the test covers real process output routing
// and redaction, and records the worker directory used by each process.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "client" {
		fmt.Printf("client=%s secret=%s worker=%s\n", os.Getenv("CLIENT_ID"), os.Getenv("CLIENT_SECRET"), os.Getenv("CLIENT_WORKER_WORKERD_WORK_DIR"))
		for {
			time.Sleep(time.Second)
		}
	}
	os.Exit(m.Run())
}

func newProcessHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureAdminToken(); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	red := redact.New()
	ring := logbuf.New(100, 4096, red.Redact)
	srv, err := New(store, nil, ring, red, Options{DataDir: dir, ClientBinary: exe, DisableAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); _ = srv.Close(context.Background()) })
	return &harness{server: srv, store: store, ring: ring, ts: ts}
}

func requireOK(t *testing.T, res response) {
	t.Helper()
	if res.status < 200 || res.status >= 300 {
		t.Fatalf("request failed: %d %s", res.status, res.body)
	}
}

func addTestConnection(t *testing.T, h *harness, name string) string {
	t.Helper()
	res := h.do(t, "POST", "/api/connections", map[string]any{"name": name}, h.csrfHeaders())
	requireOK(t, res)
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.body), &data); err != nil {
		t.Fatal(err)
	}
	return data.ID
}

func TestMultipleConnectionsRunAndRemainIsolated(t *testing.T) {
	h := newProcessHarness(t)
	masterA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer masterA.Close()
	masterB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer masterB.Close()
	id := addTestConnection(t, h, "面板 B")
	for _, c := range []struct{ id, name, secret, api string }{
		{"default", "client-a", "secret-a-123456", masterA.URL},
		{id, "client-b", "secret-b-123456", masterB.URL},
	} {
		requireOK(t, h.do(t, "POST", "/api/config?connection="+c.id, map[string]any{
			"client_id": c.name, "client_secret": c.secret, "api_url": c.api, "rpc_url": "grpc://127.0.0.1:9001", "auto_start": true,
		}, h.csrfHeaders()))
	}
	h.server.AutoStart()
	a, _, _ := h.server.connection("default")
	b, _, _ := h.server.connection(id)
	pidA, pidB := a.sup.Status().PID, b.sup.Status().PID
	if pidA == 0 || pidB == 0 || pidA == pidB {
		t.Fatal("two independent processes were not started")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(a.logs.Tail(10)) == 0 || len(b.logs.Tail(10)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("child output not captured")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, c := range []struct{ id, own, other string }{{"default", "client-a", "client-b"}, {id, "client-b", "client-a"}} {
		res := h.do(t, "GET", "/api/logs?connection="+c.id, nil, nil)
		requireOK(t, res)
		if !strings.Contains(res.body, c.own) || strings.Contains(res.body, c.other) || strings.Contains(res.body, "secret-a-123456") || strings.Contains(res.body, "secret-b-123456") {
			t.Fatalf("logs leaked across connections or exposed a secret: %s", res.body)
		}
	}
	if got := b.logs.Tail(1)[0].Text; !strings.Contains(got, filepath.Join("connections", id, "workerd")) {
		t.Fatalf("worker directory not isolated: %s", got)
	}
	a.probe.Run(context.Background())
	b.probe.Run(context.Background())
	if a.probe.Last().HTTPStatus != 200 || b.probe.Last().HTTPStatus != 204 {
		t.Fatal("probe targets are not independent")
	}
	status := h.do(t, "GET", "/api/status?connection="+id, nil, nil)
	requireOK(t, status)
	if strings.Contains(status.body, "secret-a-123456") || strings.Contains(status.body, "secret-b-123456") {
		t.Fatal("status exposed secrets")
	}
	// Updating A must restart A alone; a blank secret must retain A's secret.
	requireOK(t, h.do(t, "POST", "/api/config", map[string]any{"log_level": "debug", "client_secret": ""}, h.csrfHeaders()))
	if a.sup.Status().PID == pidA || b.sup.Status().PID != pidB {
		t.Fatal("config update restarted the wrong processes")
	}
	if c, _ := h.store.Connection("default"); c.ClientSecret != "secret-a-123456" {
		t.Fatal("blank secret lost the saved value")
	}
	requireOK(t, h.do(t, "POST", "/api/logs/clear", map[string]any{}, h.csrfHeaders()))
	if len(b.logs.Tail(10)) == 0 {
		t.Fatal("clearing A erased B's logs")
	}
	requireOK(t, h.do(t, "POST", "/api/control", map[string]any{"action": "stop"}, h.csrfHeaders()))
	if a.sup.Status().State != supervisor.StateStopped || b.sup.Status().PID != pidB {
		t.Fatal("stopping A affected B")
	}
	requireOK(t, h.do(t, "POST", "/api/connections/delete", map[string]any{"id": id}, h.csrfHeaders()))
	if b.sup.Status().State != supervisor.StateStopped {
		t.Fatal("deleted connection still running")
	}
	if _, ok := h.store.Connection(id); ok {
		t.Fatal("deleted configuration remains")
	}
	if res := h.do(t, "GET", "/api/status?connection="+id, nil, nil); res.status != 404 {
		t.Fatal("deleted connection still accessible")
	}
	if res := h.do(t, "POST", "/api/control?connection=../outside", map[string]any{"action": "start"}, h.csrfHeaders()); res.status != 404 {
		t.Fatal("unknown connection fell back to default")
	}
}

func TestConnectionAPIsRequireAuthenticationAndCSRF(t *testing.T) {
	h := newHarness(t, true, false)
	for _, path := range []string{"/api/connections", "/api/connections/delete"} {
		if res := h.do(t, "POST", path, map[string]any{}, nil); res.status != 401 {
			t.Fatalf("%s: missing auth accepted", path)
		}
		headers := h.identityHeaders()
		delete(headers, RequestHeaderMark)
		if res := h.do(t, "POST", path, map[string]any{}, headers); res.status != 403 {
			t.Fatalf("%s: missing CSRF accepted", path)
		}
	}
}

func TestCommandApplyTargetsSelectedConnectionAndAutoStartSurvivesRestart(t *testing.T) {
	h := newProcessHarness(t)
	id := addTestConnection(t, h, "面板 B")
	command := "./frp-panel client -i command-client-b -s command-secret-b --api-url http://127.0.0.1:9000 --rpc-url grpc://127.0.0.1:9001"
	res := h.do(t, "POST", "/api/command/apply?connection="+id, map[string]any{
		"command": command, "name": "备用面板", "start": true, "auto_start": true,
	}, h.csrfHeaders())
	requireOK(t, res)
	if strings.Contains(res.body, "command-secret-b") {
		t.Fatal("command response leaked a secret")
	}
	if h.store.Get().ClientID != "" {
		t.Fatal("command overwrote the default connection")
	}
	before, _, _ := h.server.connection(id)
	if before.sup.Status().State != supervisor.StateRunning {
		t.Fatal("command did not start the selected connection")
	}
	// Reopen the real persisted store and construct fresh managers, as a container restart does.
	if err := h.server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if before.sup.Status().State != supervisor.StateStopped {
		t.Fatal("shutdown left a child running")
	}
	if _, _, err := h.server.connection(id); err == nil {
		t.Fatal("shutdown allowed a runtime to be recreated")
	}
	reopened, err := config.Open(h.server.opts.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	red := redact.New()
	fresh, err := New(reopened, nil, nil, red, h.server.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close(context.Background())
	fresh.AutoStart()
	a, _, _ := fresh.connection("default")
	b, cfg, _ := fresh.connection(id)
	if a.sup.Status().State != supervisor.StateStopped || b.sup.Status().State != supervisor.StateRunning || cfg.Name != "备用面板" {
		t.Fatal("per-connection auto start or configuration did not survive restart")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(b.logs.Tail(10)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("child output not captured after restart")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(b.logs.Tail(1)[0].Text, "command-secret-b") {
		t.Fatal("persisted secret was not registered with the redactor")
	}
}

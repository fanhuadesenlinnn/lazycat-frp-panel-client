package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/config"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/redact"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/web"
)

func unixTestDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix Socket transport is verified on the Linux release runner")
	}
	// Unix socket paths have a short platform-specific limit.
	dir, err := os.MkdirTemp("", "frpp-unix-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestPrivateSocketOwnershipAndStaleRecovery(t *testing.T) {
	dir := unixTestDir(t)
	socket := filepath.Join(dir, "manager.sock")
	listener, err := listenPrivateUnix(socket, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("socket is not private")
	}
	if other, err := listenPrivateUnix(socket, dir); err == nil {
		other.Close()
		t.Fatal("active listener was replaced")
	}
	if _, err := listenPrivateUnix(filepath.Join(dir, "outside", "manager.sock"), dir); err == nil {
		t.Fatal("socket escaped data directory")
	}
	listener.Close()
	// A non-socket file must never be removed, even if a previous start failed.
	if err := os.WriteFile(socket, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenPrivateUnix(socket, dir); err == nil {
		t.Fatal("regular file was replaced")
	}
	if data, _ := os.ReadFile(socket); string(data) != "preserve" {
		t.Fatal("regular file was modified")
	}
	os.Remove(socket)
	stale, err := listenPrivateUnix(socket, dir)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	recovered, err := listenPrivateUnix(socket, dir)
	if err != nil {
		t.Fatal(err)
	}
	recovered.Close()
}

func TestUnixGatewayPreservesAuthenticationCSRFAndConnectionScope(t *testing.T) {
	dir := unixTestDir(t)
	socket := filepath.Join(dir, "manager.sock")
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureAdminToken(); err != nil {
		t.Fatal(err)
	}
	backend, err := web.New(store, nil, nil, redact.New(), web.Options{DataDir: dir, TrustIngress: true, RequireAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close(context.Background())
	listener, err := listenPrivateUnix(socket, dir)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: backend.Handler()}
	go server.Serve(listener)
	defer server.Close()
	proxy, transport := unixGateway(socket)
	defer transport.CloseIdleConnections()
	front := httptest.NewServer(proxy)
	defer front.Close()
	request := func(method, path, body, role, origin string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, front.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if role != "" {
			req.Header.Set("X-HC-User-ID", "user")
			req.Header.Set("X-HC-User-Role", role)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", web.RequestHeaderMarkValue)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(data)
	}
	for _, role := range []string{"", "USER"} {
		if status, _ := request("GET", "/api/status", "", role, ""); status != 401 {
			t.Fatal("gateway bypassed authentication")
		}
	}
	if status, _ := request("POST", "/api/connections", `{"name":"host panel"}`, "ADMIN", "https://other.example"); status != 403 {
		t.Fatal("gateway bypassed CSRF")
	}
	status, body := request("POST", "/api/connections", `{"name":"host panel"}`, "ADMIN", front.URL)
	if status != 201 {
		t.Fatalf("create through gateway: %d %s", status, body)
	}
	var added struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &added); err != nil {
		t.Fatal(err)
	}
	status, body = request("POST", "/api/config?connection="+added.ID, `{"client_id":"host-client","client_secret":"host-secret"}`, "ADMIN", front.URL)
	if status != 200 || strings.Contains(body, "host-secret") {
		t.Fatalf("gateway config or redaction failed: %d %s", status, body)
	}
	if store.Get().ClientID != "" {
		t.Fatal("gateway update overwrote the default connection")
	}
	if saved, ok := store.Connection(added.ID); !ok || saved.ClientID != "host-client" {
		t.Fatal("selected connection not saved")
	}
	status, body = request("GET", "/api/status?connection="+added.ID, "", "ADMIN", "")
	if status != 200 || !strings.Contains(body, "host panel") || !strings.Contains(body, "default") {
		t.Fatal("multi-panel status missing through gateway")
	}
}

func TestGatewayRejectsNonLoopbackBinding(t *testing.T) {
	if err := runGateway("0.0.0.0:18080", "/tmp/manager.sock"); err == nil {
		t.Fatal("gateway accepted a public listener")
	}
}

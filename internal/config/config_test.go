package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"id 含空格", Config{ClientID: "a b"}},
		{"id 含斜杠", Config{ClientID: "a/b"}},
		{"secret 含换行", Config{ClientSecret: "abc\ndef"}},
		{"secret 含空格", Config{ClientSecret: "abc def"}},
		{"api 缺少协议", Config{APIURL: "frpp.example.com:9000"}},
		{"api 协议不支持", Config{APIURL: "ftp://frpp.example.com:9000"}},
		{"api 缺少主机", Config{APIURL: "http://"}},
		{"api 内嵌凭据", Config{APIURL: "http://user:pass@frpp.example.com:9000"}},
		{"api 带查询参数", Config{APIURL: "http://frpp.example.com:9000/?token=1"}},
		{"rpc 协议不支持", Config{RPCURL: "grpcs://frpp.example.com:9001"}},
		{"rpc 协议不支持 http", Config{RPCURL: "http://frpp.example.com:9001"}},
		{"日志级别非法", Config{LogLevel: "trace"}},
	}
	for _, tc := range cases {
		if err := tc.cfg.Validate(); err == nil {
			t.Errorf("%s: 期望校验失败", tc.name)
		}
	}
}

func TestValidateAcceptsGoodValues(t *testing.T) {
	cfg := Config{
		ClientID:     "user.c.client1",
		ClientSecret: "AbC-123_456",
		APIURL:       "https://frpp.example.com:443",
		RPCURL:       "wss://frpp.example.com:443",
		LogLevel:     "debug",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("期望校验通过，实际: %v", err)
	}
	if !cfg.Configured() {
		t.Fatal("Configured 应为 true")
	}
}

func TestPublicNeverExposesSecret(t *testing.T) {
	cfg := Config{ClientID: "id", ClientSecret: "super-secret-value", APIURL: "http://a", RPCURL: "grpc://a"}
	raw, err := json.Marshal(cfg.Public())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-value") {
		t.Fatalf("Public 输出泄漏了密钥: %s", raw)
	}
	var pub Public
	if err := json.Unmarshal(raw, &pub); err != nil {
		t.Fatal(err)
	}
	if !pub.SecretSet || pub.SecretLength != len("super-secret-value") {
		t.Fatalf("密钥元信息不正确: %+v", pub)
	}
}

func TestStoreRoundTripAndFileMode(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store.Get().LogLevel != "info" {
		t.Fatalf("默认日志级别应为 info，实际 %q", store.Get().LogLevel)
	}
	if _, err := store.Update(func(c *Config) error {
		c.ClientID = "id1"
		c.ClientSecret = "secret-1"
		c.APIURL = "http://a:9000"
		c.RPCURL = "grpc://a:9001"
		c.AutoStart = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("配置文件权限应为 0600，实际 %o", perm)
		}
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Get()
	if got.ClientID != "id1" || got.ClientSecret != "secret-1" || !got.AutoStart {
		t.Fatalf("重新载入结果不一致: %+v", got)
	}

	// 原子写入不应遗留临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Fatalf("遗留了临时文件: %s", e.Name())
		}
	}
}

func TestStoreUpdateRejectsInvalidAndKeepsOldValue(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(c *Config) error {
		c.ClientID = "good-id"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(c *Config) error {
		c.ClientID = "bad id"
		return nil
	}); err == nil {
		t.Fatal("非法配置应被拒绝")
	}
	if got := store.Get().ClientID; got != "good-id" {
		t.Fatalf("校验失败后不应改动已有配置，实际 %q", got)
	}
}

func TestEnsureAdminTokenStable(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.EnsureAdminToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 48 {
		t.Fatalf("令牌长度应为 48，实际 %d", len(first))
	}
	second, err := store.EnsureAdminToken()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("重复调用应返回同一个令牌")
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Get().AdminToken != first {
		t.Fatal("令牌应持久化")
	}
}

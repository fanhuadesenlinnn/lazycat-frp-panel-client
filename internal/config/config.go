// Package config holds the persisted manager configuration.
//
// The client secret is written to disk (mode 0600) but is never part of the
// JSON contract with the browser: Public() is the only shape the web layer is
// allowed to serialise.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileName is the configuration file name inside the data directory.
const FileName = "config.json"

// Log levels accepted by the upstream client.
var allowedLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

// Config is the full on-disk configuration.
type Config struct {
	Name              string       `json:"name"`
	Connections       []Connection `json:"connections,omitempty"`
	ClientID          string       `json:"client_id"`
	ClientSecret      string       `json:"client_secret"`
	APIURL            string       `json:"api_url"`
	RPCURL            string       `json:"rpc_url"`
	AutoStart         bool         `json:"auto_start"`
	EnableRemoteShell bool         `json:"enable_remote_shell"`
	EnableFunctions   bool         `json:"enable_functions"`
	LogLevel          string       `json:"log_level"`
	AdminToken        string       `json:"admin_token"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

// Public is the browser-visible projection of Config. It intentionally has no
// secret field at all, so a secret can not leak through accidental reuse.
type Public struct {
	Name              string    `json:"name"`
	ClientID          string    `json:"client_id"`
	SecretSet         bool      `json:"secret_set"`
	SecretLength      int       `json:"secret_length"`
	APIURL            string    `json:"api_url"`
	RPCURL            string    `json:"rpc_url"`
	AutoStart         bool      `json:"auto_start"`
	EnableRemoteShell bool      `json:"enable_remote_shell"`
	EnableFunctions   bool      `json:"enable_functions"`
	LogLevel          string    `json:"log_level"`
	UpdatedAt         time.Time `json:"updated_at"`
	Configured        bool      `json:"configured"`
}

// Public returns the browser-safe view.
func (c Config) Public() Public {
	return Public{
		Name:              c.Name,
		ClientID:          c.ClientID,
		SecretSet:         c.ClientSecret != "",
		SecretLength:      len([]rune(c.ClientSecret)),
		APIURL:            c.APIURL,
		RPCURL:            c.RPCURL,
		AutoStart:         c.AutoStart,
		EnableRemoteShell: c.EnableRemoteShell,
		EnableFunctions:   c.EnableFunctions,
		LogLevel:          c.LogLevel,
		UpdatedAt:         c.UpdatedAt,
		Configured:        c.Configured(),
	}
}

// Configured reports whether every field needed to launch the client is set.
func (c Config) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.APIURL != "" && c.RPCURL != ""
}

// Normalize trims surrounding whitespace from user supplied values.
func (c *Config) Normalize() {
	c.Name = strings.TrimSpace(c.Name)
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecret = strings.TrimSpace(c.ClientSecret)
	c.APIURL = strings.TrimSpace(c.APIURL)
	c.RPCURL = strings.TrimSpace(c.RPCURL)
	c.LogLevel = strings.ToLower(strings.TrimSpace(c.LogLevel))
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
}

// Validate checks every field that ends up in a child process argument or
// environment variable. It is deliberately strict: this application never
// passes unvalidated text to a shell, but it does pass it to the trusted
// upstream binary, so garbage in must be rejected early and loudly.
func (c Config) Validate() error {
	if len([]rune(c.Name)) > 80 || strings.ContainsFunc(c.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("连接名称最多 80 个字符，不能包含控制字符")
	}
	seen := map[string]bool{DefaultConnection: true}
	for _, connection := range c.Connections {
		if !validConnectionID(connection.ID) || seen[connection.ID] {
			return errors.New("连接标识无效或重复")
		}
		seen[connection.ID] = true
		if err := connection.Config().Validate(); err != nil {
			return err
		}
	}
	if c.ClientID != "" {
		if len(c.ClientID) > 128 {
			return errors.New("客户端 ID 过长（最多 128 个字符）")
		}
		for _, r := range c.ClientID {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '.', r == '_', r == '-':
			default:
				return fmt.Errorf("客户端 ID 含非法字符 %q：只允许字母、数字、点、下划线和连字符", r)
			}
		}
	}
	if c.ClientSecret != "" {
		if len(c.ClientSecret) > 512 {
			return errors.New("客户端密钥过长（最多 512 个字符）")
		}
		if strings.ContainsFunc(c.ClientSecret, func(r rune) bool { return r < 0x21 || r == 0x7f }) {
			return errors.New("客户端密钥不能包含空白或控制字符")
		}
	}
	if c.APIURL != "" {
		if err := validateURL(c.APIURL, []string{"http", "https"}); err != nil {
			return fmt.Errorf("API URL 无效：%w", err)
		}
	}
	if c.RPCURL != "" {
		// Upstream conf.GetRPCConnInfo only understands grpc, ws and wss.
		if err := validateURL(c.RPCURL, []string{"grpc", "ws", "wss"}); err != nil {
			return fmt.Errorf("RPC URL 无效：%w", err)
		}
	}
	if c.LogLevel != "" && !allowedLogLevels[c.LogLevel] {
		return fmt.Errorf("日志级别 %q 无效：可选 debug/info/warn/error", c.LogLevel)
	}
	return nil
}

func validateURL(raw string, schemes []string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("无法解析")
	}
	if u.Scheme == "" {
		return fmt.Errorf("缺少协议前缀（应为 %s://）", schemes[0])
	}
	ok := false
	for _, s := range schemes {
		if strings.EqualFold(u.Scheme, s) {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("协议 %q 不受支持，应为 %s://", u.Scheme, schemes[0])
	}
	if u.Host == "" {
		return errors.New("缺少主机名")
	}
	if u.Fragment != "" {
		return errors.New("不应包含 # 片段")
	}
	if u.User != nil {
		return errors.New("不应在 URL 中内嵌用户名或密码")
	}
	if u.RawQuery != "" {
		return errors.New("不应包含查询参数（?a=b）")
	}
	return nil
}

// NewAdminToken returns a fresh random access token.
func NewAdminToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Store is a concurrency-safe view over the configuration file.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Open loads the configuration from dir, creating defaults when the file does
// not exist yet.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, FileName)}
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("解析 %s 失败：%w", s.path, err)
		}
		cfg.Normalize()
		for i := range cfg.Connections {
			cfg.Connections[i].normalize()
		}
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("配置校验失败：%w", err)
		}
		s.cfg = cfg
	case os.IsNotExist(err):
		s.cfg = Config{LogLevel: "info"}
	default:
		return nil, fmt.Errorf("读取 %s 失败：%w", s.path, err)
	}
	return s, nil
}

// Path returns the backing file path.
func (s *Store) Path() string { return s.path }

// Get returns a copy of the current configuration.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cfg)
}

// Update applies mutate to a copy of the configuration, validates the result
// and only then persists it atomically.
func (s *Store) Update(mutate func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.cfg)
	if mutate != nil {
		if err := mutate(&next); err != nil {
			return clone(s.cfg), err
		}
	}
	next.Normalize()
	for i := range next.Connections {
		next.Connections[i].normalize()
	}
	if err := next.Validate(); err != nil {
		return clone(s.cfg), err
	}
	next.UpdatedAt = time.Now().UTC()
	if err := writeAtomic(s.path, next); err != nil {
		return clone(s.cfg), err
	}
	s.cfg = next
	return clone(next), nil
}

// EnsureAdminToken generates and persists an access token when none exists.
func (s *Store) EnsureAdminToken() (string, error) {
	if cur := s.Get(); cur.AdminToken != "" {
		return cur.AdminToken, nil
	}
	token, err := NewAdminToken()
	if err != nil {
		return "", err
	}
	cfg, err := s.Update(func(c *Config) error {
		if c.AdminToken == "" {
			c.AdminToken = token
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return cfg.AdminToken, nil
}

func writeAtomic(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	// Restrict permissions before any secret bytes reach the file.
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Best effort durability for the directory entry (not supported on Windows).
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

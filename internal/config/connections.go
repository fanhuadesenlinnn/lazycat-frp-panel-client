package config

import (
	"errors"
	"time"
)

// DefaultConnection preserves the original single-client configuration and its
// data directory when upgrading an existing installation.
const DefaultConnection = "default"

// Connection contains only per-connection settings, never the admin token.
type Connection struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	ClientID          string    `json:"client_id"`
	ClientSecret      string    `json:"client_secret"`
	APIURL            string    `json:"api_url"`
	RPCURL            string    `json:"rpc_url"`
	AutoStart         bool      `json:"auto_start"`
	EnableRemoteShell bool      `json:"enable_remote_shell"`
	EnableFunctions   bool      `json:"enable_functions"`
	LogLevel          string    `json:"log_level"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (c Connection) Config() Config {
	return Config{Name: c.Name, ClientID: c.ClientID, ClientSecret: c.ClientSecret,
		APIURL: c.APIURL, RPCURL: c.RPCURL, AutoStart: c.AutoStart,
		EnableRemoteShell: c.EnableRemoteShell, EnableFunctions: c.EnableFunctions,
		LogLevel: c.LogLevel, UpdatedAt: c.UpdatedAt}
}

func connectionFrom(id string, c Config) Connection {
	return Connection{ID: id, Name: c.Name, ClientID: c.ClientID, ClientSecret: c.ClientSecret,
		APIURL: c.APIURL, RPCURL: c.RPCURL, AutoStart: c.AutoStart,
		EnableRemoteShell: c.EnableRemoteShell, EnableFunctions: c.EnableFunctions,
		LogLevel: c.LogLevel, UpdatedAt: c.UpdatedAt}
}

func (c *Connection) normalize() {
	settings := c.Config()
	settings.Normalize()
	*c = connectionFrom(c.ID, settings)
}

func validConnectionID(id string) bool {
	if len(id) != 48 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func clone(c Config) Config {
	c.Connections = append([]Connection(nil), c.Connections...)
	return c
}

func (s *Store) Connection(id string) (Config, bool) {
	return findConnection(s.Get(), id)
}

func findConnection(c Config, id string) (Config, bool) {
	if id == DefaultConnection {
		c.Connections = nil
		c.AdminToken = ""
		if c.Name == "" {
			c.Name = "默认连接"
		}
		return c, true
	}
	for _, connection := range c.Connections {
		if connection.ID == id {
			return connection.Config(), true
		}
	}
	return Config{}, false
}

// ConnectionIDs returns a stable display order; the original connection stays first.
func (s *Store) ConnectionIDs() []string {
	ids := []string{DefaultConnection}
	for _, c := range s.Get().Connections {
		ids = append(ids, c.ID)
	}
	return ids
}

func (s *Store) UpdateConnection(id string, mutate func(*Config) error) (Config, error) {
	saved, err := s.Update(func(root *Config) error {
		if id == DefaultConnection {
			if err := mutate(root); err != nil {
				return err
			}
			return nil
		}
		for i := range root.Connections {
			if root.Connections[i].ID != id {
				continue
			}
			next := root.Connections[i].Config()
			if err := mutate(&next); err != nil {
				return err
			}
			next.Normalize()
			next.UpdatedAt = time.Now().UTC()
			root.Connections[i] = connectionFrom(id, next)
			return nil
		}
		return errors.New("连接不存在")
	})
	if err != nil {
		return Config{}, err
	}
	// Project the exact snapshot committed by this update, even if another
	// caller modifies or deletes the connection immediately afterwards.
	result, _ := findConnection(saved, id)
	return result, nil
}

func (s *Store) AddConnection(name string) (string, error) {
	id, err := NewAdminToken()
	if err != nil {
		return "", err
	}
	_, err = s.Update(func(c *Config) error {
		if len(c.Connections) >= 31 {
			return errors.New("最多支持 32 个连接")
		}
		if name == "" {
			name = "新连接"
		}
		c.Connections = append(c.Connections, Connection{ID: id, Name: name, LogLevel: "info", UpdatedAt: time.Now().UTC()})
		return nil
	})
	return id, err
}

func (s *Store) DeleteConnection(id string) error {
	if id == DefaultConnection {
		return errors.New("默认连接不能删除，可停止或修改配置")
	}
	_, err := s.Update(func(c *Config) error {
		for i := range c.Connections {
			if c.Connections[i].ID == id {
				c.Connections = append(c.Connections[:i], c.Connections[i+1:]...)
				return nil
			}
		}
		return errors.New("连接不存在")
	})
	return err
}

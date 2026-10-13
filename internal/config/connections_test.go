package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConnectionsPreserveLegacyAndPersistIndependently(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"client_id":"legacy","client_secret":"old-secret","api_url":"http://old-master","rpc_url":"grpc://old-master","auto_start":true,"admin_token":"old-admin"}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AddConnection("第二个面板")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateConnection(id, func(c *Config) error {
		c.ClientID = "new-client"
		c.ClientSecret = "new-secret"
		c.APIURL = "https://new-master"
		c.RPCURL = "wss://new-master"
		c.AutoStart = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	old, ok := s.Connection(DefaultConnection)
	if !ok || old.ClientID != "legacy" || old.ClientSecret != "old-secret" || !old.AutoStart || old.AdminToken != "" {
		t.Fatalf("legacy settings changed: %+v", old.Public())
	}
	added, ok := s.Connection(id)
	if !ok || added.Name != "第二个面板" || added.ClientID != "new-client" || added.ClientSecret != "new-secret" || !added.AutoStart {
		t.Fatalf("added connection did not persist: %+v", added.Public())
	}
	if s.Get().AdminToken != "old-admin" {
		t.Fatal("admin token changed")
	}
	// Get must not expose the store's mutable slice, even on failed updates.
	copy := s.Get()
	copy.Connections[0].Name = "mutated"
	failed, err := s.Update(func(c *Config) error { c.Connections[0].RPCURL = "invalid"; return nil })
	if err == nil {
		t.Fatal("invalid connection accepted")
	}
	failed.Connections[0].Name = "failed-mutation"
	if saved, _ := s.Connection(id); saved.Name != "第二个面板" {
		t.Fatal("slice mutation escaped into the store")
	}
	if err := s.DeleteConnection(id); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Connection(id); ok {
		t.Fatal("deleted connection reappeared")
	}
	if err := s.DeleteConnection(DefaultConnection); err == nil {
		t.Fatal("default connection should be preserved")
	}
}

func TestConnectionRejectsUnsafeDirectoryIDsAndDuplicateIDs(t *testing.T) {
	for _, id := range []string{"../outside", "default", "", "abc"} {
		if err := (Config{Connections: []Connection{{ID: id, LogLevel: "info"}}}).Validate(); err == nil {
			t.Fatalf("unsafe ID accepted: %q", id)
		}
	}
	id := "0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := (Config{Connections: []Connection{{ID: id}, {ID: id}}}).Validate(); err == nil {
		t.Fatal("duplicate ID accepted")
	}
}

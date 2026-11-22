package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	return dir
}

func TestConfigRoundTrip(t *testing.T) {
	dir := useTempConfigDir(t)
	cfg := &config{
		AppID:        "102012345",
		ClientSecret: "secret",
		Font:         "/tmp/font.ttf",
		Servers:      map[string]string{"acovia": "mc.acovia.net"},
	}
	if err := cfg.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	path := filepath.Join(dir, configFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config permission = %o, want 600", perm)
	}

	got, err := loadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AppID != cfg.AppID || got.ClientSecret != cfg.ClientSecret || got.Font != cfg.Font {
		t.Fatalf("loaded config mismatch: %+v", got)
	}
	if got.Servers["acovia"] != "mc.acovia.net" {
		t.Fatalf("servers = %v", got.Servers)
	}
}

func TestLoadConfigMissing(t *testing.T) {
	useTempConfigDir(t)
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if !strings.Contains(err.Error(), "config file not found") || !strings.Contains(err.Error(), "app_id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	dir := useTempConfigDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigEmptyCredentials(t *testing.T) {
	dir := useTempConfigDir(t)
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte(`{"app_id":"","client_secret":"x","servers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "missing app_id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigNilServers(t *testing.T) {
	dir := useTempConfigDir(t)
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte(`{"app_id":"a","client_secret":"b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Servers == nil {
		t.Fatal("Servers should be initialized")
	}
}

func TestSaveRejectsEmptyCredentials(t *testing.T) {
	useTempConfigDir(t)
	cfg := &config{Servers: map[string]string{}}
	if err := cfg.save(); err == nil {
		t.Fatal("expected validation error")
	}
}

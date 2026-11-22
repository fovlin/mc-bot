package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useTempWorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func TestConfigRoundTrip(t *testing.T) {
	dir := useTempWorkDir(t)
	cfg := &config{
		AppID:        "102012345",
		ClientSecret: "secret",
		Font:         "/tmp/font.ttf",
	}
	if err := cfg.saveTo(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	path := filepath.Join(dir, "config", configFileName)
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
}

func TestConfigPathUsesWorkingDirectory(t *testing.T) {
	dir := useTempWorkDir(t)
	path, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "config", configFileName)
	if path != want {
		t.Fatalf("configPath = %q, want %q", path, want)
	}
}

func TestLoadConfigMissing(t *testing.T) {
	useTempWorkDir(t)
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if !strings.Contains(err.Error(), "config file not found") || !strings.Contains(err.Error(), "mcbot-go init") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	dir := useTempWorkDir(t)
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, configFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigEmptyCredentials(t *testing.T) {
	dir := useTempWorkDir(t)
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, configFileName)
	if err := os.WriteFile(path, []byte(`{"app_id":"","client_secret":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "missing app_id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSaveRejectsEmptyCredentials(t *testing.T) {
	cfg := &config{}
	if err := cfg.saveTo(t.TempDir()); err == nil {
		t.Fatal("expected validation error")
	}
}

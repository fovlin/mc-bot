package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCreatesWorkspace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workspace")
	var out strings.Builder
	if err := runInit(dir, strings.NewReader("app\nsecret\n\n"), &out); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(dir, "config", configFileName)
	info, err := os.Stat(configFile)
	if err != nil {
		t.Fatalf("config not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config permission = %o", perm)
	}
	dataInfo, err := os.Stat(filepath.Join(dir, "data"))
	if err != nil || !dataInfo.IsDir() {
		t.Fatalf("data dir: %v", err)
	}

	t.Chdir(dir)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppID != "app" || cfg.ClientSecret != "secret" || cfg.Font != "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestInitOverwritePrompt(t *testing.T) {
	dir := t.TempDir()
	if err := (&config{AppID: "old", ClientSecret: "old"}).saveTo(dir); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := runInit(dir, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppID != "old" {
		t.Fatalf("declined overwrite changed config: %+v", cfg)
	}

	out.Reset()
	if err := runInit(dir, strings.NewReader("y\nnew\nnewsecret\n\n"), &out); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppID != "new" || cfg.ClientSecret != "newsecret" {
		t.Fatalf("overwrite failed: %+v", cfg)
	}
}

func TestInitRequiresCredentials(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	if err := runInit(dir, strings.NewReader("\napp\n\nsecret\n\n"), &out); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppID != "app" || cfg.ClientSecret != "secret" {
		t.Fatalf("config = %+v", cfg)
	}
	if !strings.Contains(out.String(), "required") {
		t.Fatalf("expected required prompts, got:\n%s", out.String())
	}
}

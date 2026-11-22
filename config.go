package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const configFileName = "mcbot-go.json"

const configTemplate = `{
  "app_id": "your AppID",
  "client_secret": "your AppSecret",
  "servers": {}
}`

type config struct {
	AppID        string            `json:"app_id"`
	ClientSecret string            `json:"client_secret"`
	Font         string            `json:"font,omitempty"`
	Servers      map[string]string `json:"servers"`
}

// configPath returns the path of the bot configuration file,
// normally ~/.config/mcbot-go.json on Linux.
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(dir, configFileName), nil
}

func loadConfig() (*config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config file not found: %s\nCreate it with mode 0600. Example:\n%s", path, configTemplate)
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Servers == nil {
		cfg.Servers = make(map[string]string)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *config) validate() error {
	if c.AppID == "" {
		return errors.New("missing app_id")
	}
	if c.ClientSecret == "" {
		return errors.New("missing client_secret")
	}
	return nil
}

// save writes the configuration atomically with mode 0600, because the file
// contains the client secret.
func (c *config) save() error {
	if err := c.validate(); err != nil {
		return err
	}

	path, err := configPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".mcbot-*.json")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config %s: %w", path, err)
	}
	return nil
}

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
  "client_secret": "your AppSecret"
}`

type config struct {
	AppID        string `json:"app_id"`
	ClientSecret string `json:"client_secret"`
	Font         string `json:"font,omitempty"`
}

func workDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("locate working directory: %w", err)
	}
	return dir, nil
}

func configPath() (string, error) {
	dir, err := workDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config", configFileName), nil
}

func dataDir() (string, error) {
	dir, err := workDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "data"), nil
}

func loadConfig() (*config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config file not found: %s\nRun \"mcbot-go init\" in this directory, or create it with mode 0600. Example:\n%s", path, configTemplate)
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
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

// saveTo writes config/<name> below dir atomically with mode 0600.
func (c *config) saveTo(dir string) error {
	if err := c.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(filepath.Join(dir, "config", configFileName), data, 0o600)
}

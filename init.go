package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

type prompter struct {
	in  io.Reader
	br  *bufio.Reader
	out io.Writer
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: in, br: bufio.NewReader(in), out: out}
}

func (p *prompter) line(label string) (string, error) {
	fmt.Fprint(p.out, label)
	line, err := p.br.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (p *prompter) required(label string) (string, error) {
	for {
		value, err := p.line(label)
		if err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
		fmt.Fprintln(p.out, "This value is required.")
	}
}

func (p *prompter) secret(label string) (string, error) {
	if file, ok := p.in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprint(p.out, label)
		value, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(p.out)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(value)), nil
	}
	return p.line(label)
}

// runInit interactively creates config/mcbot-go.json and data/ either in dir
// or in the current working directory.
func runInit(dir string, in io.Reader, out io.Writer) error {
	target := dir
	if target == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("locate working directory: %w", err)
		}
		target = wd
	} else {
		abs, err := filepath.Abs(target)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", target, err)
		}
		target = abs
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create working directory %s: %w", target, err)
	}

	p := newPrompter(in, out)
	configFile := filepath.Join(target, "config", configFileName)
	if _, err := os.Stat(configFile); err == nil {
		answer, err := p.line(fmt.Sprintf("%s already exists. Overwrite? [y/N]: ", configFile))
		if err != nil {
			return err
		}
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", configFile, err)
	}

	fmt.Fprintf(out, "Initializing %s\n", target)
	appID, err := p.required("AppID: ")
	if err != nil {
		return err
	}
	secret, err := p.secret("AppSecret: ")
	if err != nil {
		return err
	}
	for secret == "" {
		fmt.Fprintln(out, "This value is required.")
		secret, err = p.secret("AppSecret: ")
		if err != nil {
			return err
		}
	}
	font, err := p.line("Font path (optional, Enter to auto-detect): ")
	if err != nil {
		return err
	}
	if font != "" {
		if _, err := os.Stat(font); err != nil {
			fmt.Fprintf(out, "Warning: font not found: %v\n", err)
		}
	}

	cfg := &config{AppID: appID, ClientSecret: secret, Font: font}
	if err := cfg.saveTo(target); err != nil {
		return err
	}
	dataPath := filepath.Join(target, "data")
	if err := os.MkdirAll(dataPath, 0o700); err != nil {
		return fmt.Errorf("create data directory %s: %w", dataPath, err)
	}

	fmt.Fprintf(out, "\nCreated:\n  %s\n  %s\n\nRun:\n  cd %s && ./mcbot-go\n", configFile, dataPath, target)
	return nil
}

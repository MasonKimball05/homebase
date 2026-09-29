// Package config loads homebase.json: the list of apps to supervise.
package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	// Listen is the dashboard's address, e.g. "0.0.0.0:8090". Keep it behind
	// a firewall rule that only allows Tailscale (see scripts/install-windows.ps1).
	Listen string `json:"listen"`
	// LogDir holds one rolling log file per app. Relative paths are resolved
	// against the config file's folder.
	LogDir string `json:"log_dir"`
	Apps   []App  `json:"apps"`
}

type App struct {
	// Name is the app's ID in URLs and log files: lowercase letters, digits, dashes.
	Name  string `json:"name"`
	Title string `json:"title"`

	// What to run. Command may be a path relative to Dir.
	Dir     string   `json:"dir"`
	Command string   `json:"command"`
	Args    []string `json:"args"`

	// Env adds variables to the app's environment. EnvFile is a KEY=VALUE file
	// read at start time: put secrets there, outside any git repo.
	Env     map[string]string `json:"env"`
	EnvFile string            `json:"env_file"`

	// URL is where the dashboard links to (e.g. the Tailscale name).
	// Health is polled to decide up/down; usually the same app on 127.0.0.1.
	URL    string `json:"url"`
	Health string `json:"health"`

	Autostart bool `json:"autostart"`
	// Restart: "on-failure" (default), "always", or "never".
	Restart string `json:"restart"`
	// RestartWhenUnhealthy restarts an app whose health check fails
	// UnhealthyAfter times in a row, e.g. a hung server that's still running.
	RestartWhenUnhealthy bool `json:"restart_when_unhealthy"`
	UnhealthyAfter       int  `json:"unhealthy_after"`

	// Source is optional: where the app's code lives and how to build it.
	// homebase itself never runs this; scripts/update.ps1 uses it.
	Source *Source `json:"source,omitempty"`

	// Timings, in seconds. Zero means use the default.
	HealthIntervalSec int `json:"health_interval_sec"`
	StopTimeoutSec    int `json:"stop_timeout_sec"`
}

// Source says how to update an app from git: pull RepoDir, then run Build
// (a command and its arguments, run inside RepoDir).
type Source struct {
	RepoDir string   `json:"repo_dir"`
	Build   []string `json:"build"`
}

const (
	RestartOnFailure = "on-failure"
	RestartAlways    = "always"
	RestartNever     = "never"
)

func (a App) HealthInterval() time.Duration { return time.Duration(a.HealthIntervalSec) * time.Second }
func (a App) StopTimeout() time.Duration    { return time.Duration(a.StopTimeoutSec) * time.Second }

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Load reads and validates the config. Relative dirs resolve against the
// config file's own folder, so the file can move with its apps.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data, filepath.Dir(abs))
}

func Parse(data []byte, baseDir string) (Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8090"
	}
	if cfg.LogDir == "" {
		cfg.LogDir = "logs"
	}
	cfg.LogDir = resolve(baseDir, cfg.LogDir)

	seen := map[string]bool{}
	for i := range cfg.Apps {
		a := &cfg.Apps[i]
		if !nameRe.MatchString(a.Name) {
			return Config{}, fmt.Errorf("app #%d: name %q must be lowercase letters, digits and dashes", i+1, a.Name)
		}
		if seen[a.Name] {
			return Config{}, fmt.Errorf("app %q is listed twice", a.Name)
		}
		seen[a.Name] = true

		if a.Title == "" {
			a.Title = a.Name
		}
		if a.Command == "" {
			return Config{}, fmt.Errorf("app %q: command is required", a.Name)
		}
		if a.Dir == "" {
			return Config{}, fmt.Errorf("app %q: dir is required", a.Name)
		}
		a.Dir = resolve(baseDir, a.Dir)
		if a.EnvFile != "" {
			a.EnvFile = resolve(baseDir, a.EnvFile)
		}

		switch a.Restart {
		case "":
			a.Restart = RestartOnFailure
		case RestartOnFailure, RestartAlways, RestartNever:
		default:
			return Config{}, fmt.Errorf("app %q: restart must be on-failure, always, or never", a.Name)
		}

		for field, raw := range map[string]string{"url": a.URL, "health": a.Health} {
			if raw == "" {
				continue
			}
			if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return Config{}, fmt.Errorf("app %q: %s must be an http(s) URL", a.Name, field)
			}
		}
		if src := a.Source; src != nil {
			if src.RepoDir == "" || len(src.Build) == 0 {
				return Config{}, fmt.Errorf("app %q: source needs both repo_dir and build", a.Name)
			}
			src.RepoDir = resolve(baseDir, src.RepoDir)
		}
		if a.RestartWhenUnhealthy && a.Health == "" {
			return Config{}, fmt.Errorf("app %q: restart_when_unhealthy needs a health URL", a.Name)
		}

		if a.HealthIntervalSec <= 0 {
			a.HealthIntervalSec = 15
		}
		if a.StopTimeoutSec <= 0 {
			a.StopTimeoutSec = 10
		}
		if a.UnhealthyAfter <= 0 {
			a.UnhealthyAfter = 3
		}
	}
	return cfg, nil
}

func resolve(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// ReadEnvFile parses KEY=VALUE lines. Blank lines and # comments are
// ignored, and surrounding quotes on the value are removed.
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			// Don't echo the line: it may contain a secret.
			return nil, fmt.Errorf("%s line %d: expected KEY=VALUE", filepath.Base(path), n)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		env[key] = val
	}
	return env, sc.Err()
}

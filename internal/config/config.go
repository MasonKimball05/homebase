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
	Alerts Alerts `json:"alerts"`
	// AI, when present, lets the dashboard (and Loom on another machine)
	// chat with models served by Ollama on this PC. See internal/ai.
	AI *AI `json:"ai,omitempty"`
	// Projects, when present, adds a read-only Projects page: the git repos
	// in these folders, what changed in them, and their files. See internal/projects.
	Projects *Projects `json:"projects,omitempty"`
	// Away, when present, works through a queue of coding tasks while you're
	// away from this PC: the local model on a branch, Claude Code reviewing
	// (and fixing) it. Needs Projects. See internal/away.
	Away *Away `json:"away,omitempty"`
	Apps []App `json:"apps"`
}

// Away configures the task queue (internal/away). Paths are on this PC.
type Away struct {
	// Runner is a Loom checkout: its tools/qwen runs the local model with
	// Loom's own agent loop (npx vitest; needs node_modules there).
	Runner string `json:"runner"`
	// Worktrees is where each task gets its own git worktree while it runs.
	Worktrees string `json:"worktrees"`
	// Claude is the Claude Code CLI (logged in on this PC). Default "claude".
	Claude string `json:"claude"`
	// Ollama and Model for the local model. Defaults: ai.ollama and
	// "qwen3-coder:30b".
	Ollama string `json:"ollama"`
	Model  string `json:"model"`
	// IdleMinutes: in "when idle" mode, start after this long without
	// keyboard or mouse input (needs `homebase -idle-report` running in your
	// session). Default 20.
	IdleMinutes int `json:"idle_minutes"`
	// Rounds the local model gets (Claude sending it back with notes)
	// before Claude fixes the branch itself. Default 2.
	Rounds int `json:"rounds"`
	// Push lists projects whose finished away/… branches are pushed to their
	// origin (a private clone you review on GitHub). Others are never pushed.
	Push []string `json:"push,omitempty"`
	// Path: folders put in front of PATH for the local model's checks and
	// Claude Code (e.g. Python's user Scripts folder, where pip put ruff).
	Path []string `json:"path,omitempty"`
	// Tests: a project's test command for the local model's run_tests tool,
	// by project id, e.g. {"Parliament-Dev": "python manage.py test src"}.
	// Without one Loom's runner picks one itself (npm test, pytest…) or none.
	Tests map[string]string `json:"tests,omitempty"`
	// EmbedModel finds similar past tasks to show the local model (Ollama).
	// Default "nomic-embed-text"; "off" shows none.
	EmbedModel string `json:"embed_model,omitempty"`
}

// Projects lists the folders whose git repos the Projects page shows.
type Projects struct {
	Roots []string `json:"roots"`
	// RemoteEdit lets Loom on another machine open and edit these projects
	// (internal/projects/remote.go), with the token in remote-token.txt
	// next to homebase.json.
	RemoteEdit bool `json:"remote_edit,omitempty"`
}

// AI points homebase at an Ollama server. Ollama itself stays on 127.0.0.1;
// homebase is the one door to it, behind the same Tailscale-only firewall
// rule and CSRF checks as the rest of the dashboard.
type AI struct {
	// Ollama is the server's address. Default http://127.0.0.1:11434.
	Ollama string `json:"ollama"`
	// DefaultModel is picked first in the chat page, e.g. "qwen3-coder:30b".
	DefaultModel string `json:"default_model"`
}

// Alerts sends phone notifications through ntfy (https://ntfy.sh).
//
// The ntfy topic URL works like a password (anyone with it can read your
// alerts), and this config is committed to git, so keep the URL out of it:
// put NTFY_URL=... in EnvFile, or set HOMEBASE_NTFY_URL.
type Alerts struct {
	NtfyURL string `json:"ntfy_url"`
	EnvFile string `json:"env_file"`
	// DashboardURL opens when you tap a notification.
	DashboardURL string `json:"dashboard_url"`
	// A crash loop is CrashLoopRestarts restarts within CrashLoopWindowMin minutes.
	CrashLoopRestarts  int `json:"crash_loop_restarts"`
	CrashLoopWindowMin int `json:"crash_loop_window_min"`
	// StartupSummary sends "homebase is up: 2/2 running" shortly after boot.
	StartupSummary bool `json:"startup_summary"`
}

func (a Alerts) CrashLoopWindow() time.Duration {
	return time.Duration(a.CrashLoopWindowMin) * time.Minute
}

// ResolveNtfyURL finds the topic URL: HOMEBASE_NTFY_URL wins, then NTFY_URL
// from EnvFile, then NtfyURL. "" means alerts are off.
func (a Alerts) ResolveNtfyURL() (string, error) {
	raw := a.NtfyURL
	if a.EnvFile != "" {
		env, err := ReadEnvFile(a.EnvFile)
		if err != nil {
			return "", fmt.Errorf("alerts env file: %w", err)
		}
		if v := env["NTFY_URL"]; v != "" {
			raw = v
		}
	}
	if v := os.Getenv("HOMEBASE_NTFY_URL"); v != "" {
		raw = v
	}
	if raw == "" {
		return "", nil
	}
	// Don't include the value in the error: it's a secret.
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || len(u.Path) < 2 {
		return "", fmt.Errorf("the ntfy URL must look like https://ntfy.sh/<topic>")
	}
	// Plain http only for an ntfy server on this same machine.
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return "", fmt.Errorf("the ntfy URL must use https (http is only allowed for localhost)")
	}
	return raw, nil
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

	// LogFile is a log another program writes for this app, shown in its
	// Logs panel: for apps that usually run outside homebase (Ollama's tray
	// app logs to ${LOCALAPPDATA}\Ollama\server.log). ${VAR}s are expanded.
	LogFile string `json:"log_file,omitempty"`
	// LogSkip leaves matching lines of LogFile out of the Logs panel: Go
	// regular expressions, e.g. the requests homebase itself makes to check on
	// the app, which would otherwise drown out everything else.
	LogSkip []string `json:"log_skip,omitempty"`
	// ExternalOK: running outside homebase is this app's normal state (Ollama
	// runs from its tray app), so show that as healthy rather than as a
	// duplicate to clean up. Homebase still starts it when it isn't running.
	ExternalOK bool `json:"external_ok,omitempty"`

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
	if cfg.Alerts.EnvFile != "" {
		cfg.Alerts.EnvFile = resolve(baseDir, cfg.Alerts.EnvFile)
	}
	if cfg.Alerts.CrashLoopRestarts <= 0 {
		cfg.Alerts.CrashLoopRestarts = 3
	}
	if cfg.Alerts.CrashLoopWindowMin <= 0 {
		cfg.Alerts.CrashLoopWindowMin = 10
	}
	if d := cfg.Alerts.DashboardURL; d != "" {
		if u, err := url.Parse(d); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("alerts.dashboard_url must be an http(s) URL")
		}
	}

	if ai := cfg.AI; ai != nil {
		if ai.Ollama == "" {
			ai.Ollama = "http://127.0.0.1:11434"
		}
		if u, err := url.Parse(ai.Ollama); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return Config{}, fmt.Errorf("ai.ollama must be an http(s) address like http://127.0.0.1:11434")
		}
	}

	if p := cfg.Projects; p != nil {
		if len(p.Roots) == 0 {
			return Config{}, fmt.Errorf("projects.roots needs at least one folder")
		}
		for i, r := range p.Roots {
			p.Roots[i] = resolve(baseDir, os.ExpandEnv(r))
		}
	}

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
		if a.LogFile != "" {
			a.LogFile = resolve(baseDir, os.ExpandEnv(a.LogFile))
		}
		if len(a.LogSkip) > 0 && a.LogFile == "" {
			return Config{}, fmt.Errorf("app %q: log_skip only applies to a log_file", a.Name)
		}
		for _, pat := range a.LogSkip {
			if _, err := regexp.Compile(pat); err != nil {
				return Config{}, fmt.Errorf("app %q: log_skip %q isn't a valid pattern: %v", a.Name, pat, err)
			}
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

// Command homebase supervises the apps hosted on this machine and serves a
// dashboard to watch and control them.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/MasonKimball05/homebase/internal/ai"
	"github.com/MasonKimball05/homebase/internal/alerts"
	"github.com/MasonKimball05/homebase/internal/away"
	"github.com/MasonKimball05/homebase/internal/config"
	"github.com/MasonKimball05/homebase/internal/projects"
	"github.com/MasonKimball05/homebase/internal/resources"
	"github.com/MasonKimball05/homebase/internal/supervisor"
	"github.com/MasonKimball05/homebase/internal/web"
)

func main() {
	configPath := flag.String("config", "homebase.json", "path to config file")
	check := flag.Bool("check", false, "validate the config and exit without starting anything")
	testAlert := flag.Bool("test-alert", false, "send a test notification and exit")
	idleReport := flag.Bool("idle-report", false, "keep away-idle.json (next to the config) up to date with how long this session has been idle; run at logon (see README, Away)")
	flag.Parse()

	if *idleReport {
		reportIdle(*configPath)
		return
	}

	if *testAlert {
		if err := sendTestAlert(*configPath); err != nil {
			fmt.Fprintln(os.Stderr, "homebase:", err)
			os.Exit(1)
		}
		return
	}

	if *check {
		if err := checkConfig(*configPath); err != nil {
			fmt.Fprintln(os.Stderr, "homebase:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "homebase:", err)
		os.Exit(1)
	}
}

// checkConfig validates the file and the things it points at, so a typo is
// caught before a restart rather than showing up as a crashed app.
func checkConfig(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	problems := 0
	fmt.Printf("dashboard: %s   logs: %s\n", cfg.Listen, cfg.LogDir)
	for _, a := range cfg.Apps {
		var issues []string
		if st, err := os.Stat(a.Dir); err != nil || !st.IsDir() {
			issues = append(issues, "dir not found: "+a.Dir)
		}
		if !filepath.IsAbs(a.Command) {
			if _, err := os.Stat(filepath.Join(a.Dir, a.Command)); err != nil {
				if _, err := exec.LookPath(a.Command); err != nil {
					issues = append(issues, "command not found in dir or PATH: "+a.Command)
				}
			}
		} else if _, err := os.Stat(a.Command); err != nil {
			issues = append(issues, "command not found: "+a.Command)
		}
		if a.Source != nil {
			if _, err := os.Stat(filepath.Join(a.Source.RepoDir, ".git")); err != nil {
				issues = append(issues, "source.repo_dir is not a git clone: "+a.Source.RepoDir)
			}
		}
		if a.EnvFile != "" {
			if _, err := config.ReadEnvFile(a.EnvFile); err != nil {
				issues = append(issues, "env file: "+err.Error())
			}
		}
		mark := "ok"
		if len(issues) > 0 {
			mark = "PROBLEM"
			problems++
		}
		fmt.Printf("  %-7s %-14s %s\n", mark, a.Name, a.Title)
		for _, i := range issues {
			fmt.Printf("            - %s\n", i)
		}
	}
	switch ntfy, err := cfg.Alerts.ResolveNtfyURL(); {
	case err != nil:
		fmt.Printf("  WARNING alerts: %v (homebase will run without alerts)\n", err)
	case ntfy == "":
		fmt.Println("  alerts: off (no ntfy URL configured)")
	default:
		fmt.Println("  alerts: on (ntfy)")
	}
	if problems > 0 {
		return fmt.Errorf("%d app(s) have problems", problems)
	}
	fmt.Println("config OK")
	return nil
}

func sendTestAlert(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ntfy, err := cfg.Alerts.ResolveNtfyURL()
	if err != nil {
		return err
	}
	if ntfy == "" {
		return errors.New("no ntfy URL configured (set NTFY_URL in the alerts env_file)")
	}
	host, _ := os.Hostname()
	n := alerts.Ntfy{URL: ntfy, Click: cfg.Alerts.DashboardURL}
	err = n.Notify(context.Background(), alerts.Message{
		Title:    "homebase test alert from " + host,
		Body:     "If you can read this, homebase alerts reach your phone.",
		Priority: "default",
		Tags:     "white_check_mark",
	})
	if err == nil {
		fmt.Println("test alert sent")
	}
	return err
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// Listen before starting any apps, so a second homebase (port in use)
	// exits right away instead of launching duplicate apps.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr := supervisor.NewManager(cfg)
	appsDone := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(appsDone)
	}()

	// homebase's own log: stderr plus a file, since on the desktop it runs
	// as a hidden scheduled task with nowhere to print.
	if err := os.MkdirAll(cfg.LogDir, 0o755); err == nil {
		if f, err := os.OpenFile(filepath.Join(cfg.LogDir, "homebase.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			defer f.Close()
			log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}
	log.Printf("homebase starting: %d app(s), dashboard on %s", len(cfg.Apps), ln.Addr())

	// Alerts are optional: a bad or missing alerts config is logged, never
	// a reason to leave the apps down.
	logf := func(format string, args ...any) { log.Printf(format, args...) }
	var notifier alerts.Notifier
	switch ntfy, err := cfg.Alerts.ResolveNtfyURL(); {
	case err != nil:
		logf("alerts disabled: %v", err)
	case ntfy != "":
		notifier = alerts.Ntfy{URL: ntfy, Click: cfg.Alerts.DashboardURL}
		logf("alerts: sending to ntfy")
	}
	host, _ := os.Hostname()
	go alerts.NewWatcher(mgr.Statuses, notifier, cfg.Alerts, host, logf).Run(ctx)

	// CPU and memory for the machine and each app (Windows only; a no-op elsewhere).
	usage := resources.NewSampler(func() map[string]uint32 {
		roots := map[string]uint32{}
		for _, st := range mgr.Statuses() {
			if st.PID > 0 {
				roots[st.Name] = uint32(st.PID)
			}
		}
		return roots
	}, 3*time.Second)
	go usage.Run(ctx)

	dash := web.New(mgr).WithResources(usage.Latest)
	if cfg.AI != nil {
		// Chat with Ollama's models from the dashboard (and Loom) over Tailscale.
		proxy, err := ai.New(cfg.AI.Ollama, cfg.AI.DefaultModel)
		if err != nil {
			return err
		}
		dash = dash.WithAI(proxy)
	}
	if cfg.Projects != nil {
		// See what changed in this PC's projects from another machine; with
		// remote_edit, Loom can also open and edit them (token-protected).
		browser := projects.New(cfg.Projects.Roots)
		if cfg.Projects.RemoteEdit {
			tokenPath := filepath.Join(filepath.Dir(configPath), "remote-token.txt")
			token, err := projects.LoadOrCreateToken(tokenPath)
			if err != nil {
				return fmt.Errorf("remote editing: %w", err)
			}
			browser = browser.EnableRemote(token)
			fmt.Printf("homebase: remote editing on (token in %s)\n", tokenPath)
		}
		dash = dash.WithProjects(browser)
		if cfg.Away != nil {
			// Coding tasks while you're away: the local model, then Claude Code (internal/away).
			dir := filepath.Dir(configPath)
			ollama := cfg.Away.Ollama
			if ollama == "" && cfg.AI != nil {
				ollama = cfg.AI.Ollama
			}
			if ollama == "" {
				ollama = "http://127.0.0.1:11434"
			}
			model := cfg.Away.Model
			if model == "" {
				model = "qwen3-coder:30b"
			}
			claude := cfg.Away.Claude
			if claude == "" {
				claude = "claude"
			}
			logDir := filepath.Join(cfg.LogDir, "away")
			steps := away.CLI{Runner: cfg.Away.Runner, ClaudePath: claude, Ollama: ollama, Model: model, LogDir: logDir, Path: cfg.Away.Path}
			notify := func(title, body string) {
				if notifier != nil {
					if err := notifier.Notify(context.Background(), alerts.Message{Title: title, Body: body, Tags: "robot"}); err != nil {
						logf("away: notification: %v", err)
					}
				}
			}
			svc := away.New(away.Settings{
				Runner: cfg.Away.Runner, Worktrees: cfg.Away.Worktrees, Claude: claude, Ollama: ollama, Model: model,
				IdleMinutes: cfg.Away.IdleMinutes, Rounds: cfg.Away.Rounds, Push: cfg.Away.Push,
				StatePath: filepath.Join(dir, "away.json"), IdlePath: filepath.Join(dir, "away-idle.json"), LogDir: logDir,
			}, browser, steps, notify, logf)
			go svc.Run(ctx)
			dash = dash.WithAway(svc)
			logf("away: queue ready (runner %s)", cfg.Away.Runner)
		}
	}
	srv := &http.Server{Handler: dash.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	fmt.Printf("homebase: %d app(s), dashboard on http://%s (Ctrl+C to stop everything)\n", len(cfg.Apps), ln.Addr())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		stop()
		<-appsDone
		return err
	}
	// Don't exit until every app has been stopped.
	<-appsDone
	return nil
}

// reportIdle runs in your logon session (Windows only): homebase itself runs
// as a background task and can't see the keyboard and mouse, so this writes
// how long they've been untouched for internal/away's "when idle" mode.
func reportIdle(configPath string) {
	path := filepath.Join(filepath.Dir(configPath), "away-idle.json")
	for {
		if idle, ok := away.IdleSeconds(); ok {
			now := time.Now().Unix()
			b, _ := json.Marshal(away.IdleReport{LastInput: now - idle, Written: now})
			_ = os.WriteFile(path+".tmp", b, 0o644)
			_ = os.Rename(path+".tmp", path)
		} else {
			fmt.Fprintln(os.Stderr, "homebase: can't read the idle time here (Windows only)")
			return
		}
		time.Sleep(20 * time.Second)
	}
}

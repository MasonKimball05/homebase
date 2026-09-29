// Command homebase supervises the apps hosted on this machine and serves a
// dashboard to watch and control them.
package main

import (
	"context"
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

	"github.com/MasonKimball05/homebase/internal/alerts"
	"github.com/MasonKimball05/homebase/internal/config"
	"github.com/MasonKimball05/homebase/internal/supervisor"
	"github.com/MasonKimball05/homebase/internal/web"
)

func main() {
	configPath := flag.String("config", "homebase.json", "path to config file")
	check := flag.Bool("check", false, "validate the config and exit without starting anything")
	testAlert := flag.Bool("test-alert", false, "send a test notification and exit")
	flag.Parse()

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

	srv := &http.Server{Handler: web.New(mgr).Handler(), ReadHeaderTimeout: 5 * time.Second}
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

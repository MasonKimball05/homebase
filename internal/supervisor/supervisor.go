// Package supervisor starts apps, restarts them when they crash, and
// health-checks them.
//
// Each app has one goroutine (App.Run) that owns its process. Everything
// else (dashboard buttons, the health checker) sends it requests over a
// channel instead of touching the process directly, so there are never two
// goroutines trying to start or stop the same app at once.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MasonKimball05/homebase/internal/config"
)

type State string

const (
	Stopped  State = "stopped"
	Starting State = "starting" // process running, health check not passed yet
	Running  State = "running"
	Stopping State = "stopping"
	Backoff  State = "backoff"  // crashed; waiting to restart
	Crashed  State = "crashed"  // exited with an error and won't be restarted
	External State = "external" // something already answers on the health URL
)

type Health struct {
	OK         bool      `json:"ok"`
	StatusCode int       `json:"status_code,omitempty"`
	LatencyMs  int64     `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
	Failures   int       `json:"failures"` // consecutive
}

type Exit struct {
	Code      int       `json:"code"`
	At        time.Time `json:"at"`
	Error     string    `json:"error,omitempty"`
	Requested bool      `json:"requested"` // stopped on purpose, not a crash
}

// Status is a point-in-time snapshot for the dashboard.
type Status struct {
	Name          string     `json:"name"`
	Title         string     `json:"title"`
	URL           string     `json:"url,omitempty"`
	State         State      `json:"state"`
	Message       string     `json:"message,omitempty"`
	PID           int        `json:"pid,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	Restarts      int        `json:"restarts"`
	LastExit      *Exit      `json:"last_exit,omitempty"`
	Health        *Health    `json:"health,omitempty"`
	MemoryBytes   int64      `json:"memory_bytes,omitempty"`
	NextRestartAt *time.Time `json:"next_restart_at,omitempty"`
	Autostart     bool       `json:"autostart"`
}

type action int

const (
	actStart action = iota
	actStop
	actRestart
	actUnhealthy    // health checks keep failing on a running app
	actExternalGone // the externally-run copy stopped answering
)

// Settings that tests shrink so they don't have to wait for real delays.
type timing struct {
	backoffBase time.Duration // first restart delay; doubles per quick crash
	backoffMax  time.Duration
	stableAfter time.Duration // an app that ran this long resets the backoff
	health      time.Duration // overrides the config's health interval if set
}

var defaultTiming = timing{backoffBase: time.Second, backoffMax: time.Minute, stableAfter: time.Minute}

type App struct {
	cfg    config.App
	Logs   *Logs
	cmds   chan action
	kick   chan struct{} // wakes the health checker right after a start
	client *http.Client
	timing timing

	mu sync.RWMutex // guards st
	st Status
}

func NewApp(cfg config.App, logDir string) *App {
	return &App{
		cfg:  cfg,
		Logs: NewLogs(filepath.Join(logDir, cfg.Name+".log"), 500, 5<<20),
		cmds: make(chan action, 8),
		kick: make(chan struct{}, 1),
		client: &http.Client{
			Timeout: 5 * time.Second,
			// A redirect (e.g. to a login page) still means the app is up.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		timing: defaultTiming,
		st:     Status{Name: cfg.Name, Title: cfg.Title, URL: cfg.URL, State: Stopped, Autostart: cfg.Autostart},
	}
}

func (a *App) Name() string { return a.cfg.Name }

// Status returns a copy, safe to use after the lock is released.
func (a *App) Status() Status {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s := a.st
	if s.Health != nil {
		h := *s.Health
		s.Health = &h
	}
	return s
}

func (a *App) update(f func(s *Status)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(&a.st)
}

// Do queues "start", "stop" or "restart". It never blocks the caller.
func (a *App) Do(name string) error {
	act, ok := map[string]action{"start": actStart, "stop": actStop, "restart": actRestart}[name]
	if !ok {
		return fmt.Errorf("unknown action %q", name)
	}
	if s := a.Status(); s.State == External && act != actStart {
		return errors.New("this copy wasn't started by homebase, so homebase can't stop it; stop it where it was started (e.g. its old scheduled task)")
	}
	select {
	case a.cmds <- act:
		return nil
	default:
		return errors.New("busy; try again in a moment")
	}
}

// exitResult is what the waiter goroutine reports when the process ends.
type exitResult struct {
	code int
	err  error
}

// Run supervises the app until ctx is cancelled, then stops it.
func (a *App) Run(ctx context.Context) {
	var (
		proc      *exec.Cmd
		exited    chan exitResult // receives once when proc exits
		done      chan struct{}   // closed when proc exits (for terminate)
		startedAt time.Time
		want      bool // should the app be running?
		stopping  bool // we asked it to stop, so an exit isn't a crash
		thenStart bool // restart = stop, then start once it's gone
		quick     int  // consecutive crashes without running stably
		timer     *time.Timer
		timerC    <-chan time.Time // nil (blocks forever) unless a restart is scheduled
	)

	cancelTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer, timerC = nil, nil
		a.update(func(s *Status) { s.NextRestartAt = nil })
	}

	start := func() {
		cancelTimer()
		// Another copy (e.g. from an old startup task) already answering on
		// the health URL would make ours fail on "port in use". Report it
		// instead of starting a duplicate.
		if a.cfg.Health != "" {
			if h := a.probe(ctx); h.OK {
				a.update(func(s *Status) {
					s.State, s.Health = External, &h
					s.Message = "Already running outside homebase. Stop that copy (or disable its old startup task), then press Start."
				})
				a.Logs.Sysf("not starting: something already answers at %s", a.cfg.Health)
				return
			}
		}

		cmd, err := a.command()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			a.Logs.Sysf("failed to start: %v", err)
			a.update(func(s *Status) { s.State, s.Message, s.PID = Crashed, "Failed to start: "+err.Error(), 0 })
			return
		}

		if err := afterStart(cmd); err != nil {
			a.Logs.Sysf("warning: couldn't tie app to homebase's lifetime: %v", err)
		}
		proc, startedAt, stopping = cmd, time.Now(), false
		exited, done = make(chan exitResult, 1), make(chan struct{})
		a.Logs.Sysf("started (pid %d): %s", cmd.Process.Pid, strings.Join(append([]string{a.cfg.Command}, a.cfg.Args...), " "))
		state := Starting
		if a.cfg.Health == "" {
			state = Running // nothing to wait for
		}
		a.update(func(s *Status) {
			t := startedAt
			s.State, s.Message, s.PID, s.StartedAt = state, "", cmd.Process.Pid, &t
		})
		// Don't wait out the rest of a 15s interval to notice it came up.
		select {
		case a.kick <- struct{}{}:
		default:
		}

		// Wait in a separate goroutine; it reports back over the channel.
		go func(cmd *exec.Cmd, exited chan<- exitResult, done chan<- struct{}) {
			err := cmd.Wait()
			for _, w := range []any{cmd.Stdout, cmd.Stderr} {
				w.(*lineWriter).flush()
			}
			code := 0
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				code = ee.ExitCode()
			} else if err != nil {
				code = -1
			}
			exited <- exitResult{code, err}
			close(done)
		}(cmd, exited, done)
	}

	stop := func() {
		cancelTimer()
		if proc == nil {
			if s := a.Status(); s.State != External {
				a.update(func(s *Status) { s.State, s.Message = Stopped, "" })
			}
			return
		}
		if stopping {
			return
		}
		stopping = true
		a.update(func(s *Status) { s.State = Stopping })
		a.Logs.Sysf("stopping (pid %d)", proc.Process.Pid)
		// Terminate in the background so this loop keeps serving requests.
		go terminate(proc.Process.Pid, a.cfg.StopTimeout(), done)
	}

	// Health checks run on their own goroutine: a slow probe (up to 5s)
	// must not stall Start/Stop.
	hctx, stopHealth := context.WithCancel(ctx)
	defer stopHealth()
	go a.healthLoop(hctx)

	if a.cfg.Autostart {
		want = true
		start()
	}

	for {
		// A nil channel blocks forever, so these cases only fire when relevant.
		var exitedC <-chan exitResult
		if proc != nil {
			exitedC = exited
		}

		select {
		case <-ctx.Done():
			if proc != nil {
				a.Logs.Sysf("homebase is shutting down; stopping app")
				terminate(proc.Process.Pid, a.cfg.StopTimeout(), done)
				<-exited
			}
			a.update(func(s *Status) { s.State, s.PID, s.StartedAt = Stopped, 0, nil })
			a.Logs.Close()
			return

		case act := <-a.cmds:
			switch act {
			case actStart:
				want = true
				if proc == nil {
					quick = 0 // a manual start gets a fresh backoff
					start()
				}
			case actStop:
				want, thenStart = false, false
				stop()
			case actRestart:
				want = true
				if proc == nil {
					start()
				} else {
					thenStart = true
					stop()
				}
			case actUnhealthy:
				if proc != nil && !stopping {
					a.Logs.Sysf("health check failed %d times in a row; restarting", a.cfg.UnhealthyAfter)
					thenStart = true
					a.update(func(s *Status) { s.Restarts++ })
					stop()
				}
			case actExternalGone:
				if want && proc == nil {
					a.Logs.Sysf("the external copy stopped answering; starting our own")
					start()
				}
			}

		case r := <-exitedC:
			ranFor := time.Since(startedAt)
			pid := proc.Process.Pid
			proc = nil
			a.update(func(s *Status) {
				s.PID, s.StartedAt, s.MemoryBytes = 0, nil, 0
				s.LastExit = &Exit{Code: r.code, At: time.Now(), Requested: stopping}
				if r.err != nil {
					s.LastExit.Error = r.err.Error()
				}
			})

			if stopping {
				a.Logs.Sysf("stopped (pid %d)", pid)
				a.update(func(s *Status) { s.State, s.Message = Stopped, "" })
				if thenStart {
					thenStart = false
					start()
				}
				continue
			}

			a.Logs.Sysf("exited unexpectedly with code %d after %s", r.code, ranFor.Round(time.Second))
			restart := want && (a.cfg.Restart == config.RestartAlways ||
				(a.cfg.Restart == config.RestartOnFailure && r.code != 0))
			if !restart {
				want = false
				final, msg := Stopped, "Exited normally."
				if r.code != 0 {
					final, msg = Crashed, fmt.Sprintf("Exited with code %d.", r.code)
				}
				a.update(func(s *Status) { s.State, s.Message = final, msg })
				continue
			}

			// Back off exponentially on repeated quick crashes, so a broken
			// app doesn't spin the CPU restarting hundreds of times a minute.
			if ranFor >= a.timing.stableAfter {
				quick = 0
			}
			quick++
			delay := min(a.timing.backoffBase<<(quick-1), a.timing.backoffMax)
			at := time.Now().Add(delay)
			timer = time.NewTimer(delay)
			timerC = timer.C
			a.Logs.Sysf("restarting in %s", delay)
			a.update(func(s *Status) {
				s.State, s.NextRestartAt, s.Restarts = Backoff, &at, s.Restarts+1
				s.Message = fmt.Sprintf("Crashed (code %d). Restarting in %s.", r.code, delay.Round(time.Second))
			})

		case <-timerC:
			timer, timerC = nil, nil
			if want && proc == nil {
				start()
			}
		}
	}
}

// command builds the exec.Cmd for the app, with output wired to its logs.
func (a *App) command() (*exec.Cmd, error) {
	name := a.cfg.Command
	// "JobTracker.exe" in the app's folder: use that file, rather than
	// searching PATH (Go refuses to run programs from the current folder by
	// name, as a security measure).
	if !filepath.IsAbs(name) {
		if local := filepath.Join(a.cfg.Dir, name); fileExists(local) {
			name = local
		}
	}

	env := os.Environ()
	if a.cfg.EnvFile != "" {
		fileEnv, err := config.ReadEnvFile(a.cfg.EnvFile)
		if err != nil {
			return nil, fmt.Errorf("env file: %w", err)
		}
		env = appendEnv(env, fileEnv)
	}
	env = appendEnv(env, a.cfg.Env)

	cmd := exec.Command(name, a.cfg.Args...)
	cmd.Dir = a.cfg.Dir
	cmd.Env = env
	cmd.Stdout = &lineWriter{logs: a.Logs, stream: "out"}
	cmd.Stderr = &lineWriter{logs: a.Logs, stream: "err"}
	prepare(cmd)
	return cmd, nil
}

// appendEnv adds vars in sorted order; later entries win for duplicate keys.
func appendEnv(env []string, vars map[string]string) []string {
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		env = append(env, k+"="+vars[k])
	}
	return env
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (a *App) probe(ctx context.Context) Health {
	h := Health{CheckedAt: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Health, nil)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	req.Header.Set("User-Agent", "homebase-healthcheck")
	start := time.Now()
	resp, err := a.client.Do(req)
	h.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		h.Error = shortError(err)
		return h
	}
	resp.Body.Close()
	h.StatusCode = resp.StatusCode
	h.OK = resp.StatusCode < 400
	if !h.OK {
		h.Error = resp.Status
	}
	return h
}

// shortError drops the "Get "http://...": " prefix net/http adds.
func shortError(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}

func (a *App) healthLoop(ctx context.Context) {
	every := a.cfg.HealthInterval()
	if a.timing.health > 0 {
		every = a.timing.health
	}
	for {
		a.checkOnce(ctx)
		// Check every second while an app is starting up (or has no reading
		// yet), so it shows "running" as soon as it's ready rather than up
		// to a full interval later.
		wait := every
		if s := a.Status(); s.State == Starting || (s.PID > 0 && s.MemoryBytes == 0) {
			wait = min(time.Second, every)
		}
		select {
		case <-ctx.Done():
			return
		case <-a.kick:
		case <-time.After(wait):
		}
	}
}

func (a *App) checkOnce(ctx context.Context) {
	pid := a.Status().PID
	if pid > 0 {
		if mem, ok := memoryBytes(pid); ok {
			a.update(func(s *Status) {
				if s.PID == pid { // still the same process
					s.MemoryBytes = mem
				}
			})
		}
	}
	if a.cfg.Health == "" {
		return
	}

	h := a.probe(ctx)
	if ctx.Err() != nil {
		return
	}
	var notify action = -1
	a.update(func(s *Status) {
		if s.Health != nil && !h.OK {
			h.Failures = s.Health.Failures + 1
		} else if !h.OK {
			h.Failures = 1
		}
		s.Health = &h
		switch {
		case s.State == Starting && h.OK:
			s.State = Running
		case (s.State == Stopped || s.State == Crashed) && s.PID == 0 && h.OK:
			s.State = External
			s.Message = "Running, but not started by homebase."
		case s.State == External && !h.OK:
			s.State, s.Message = Stopped, ""
			notify = actExternalGone
		case s.State == Running && !h.OK && a.cfg.RestartWhenUnhealthy && h.Failures >= a.cfg.UnhealthyAfter:
			notify = actUnhealthy
		}
	})
	if notify >= 0 {
		select {
		case a.cmds <- notify:
		default:
		}
	}
}

// CheckNow runs a health check immediately (used after the dashboard loads).
func (a *App) CheckNow(ctx context.Context) { a.checkOnce(ctx) }

package supervisor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MasonKimball05/homebase/internal/config"
)

// The test binary doubles as the program being supervised: when
// HOMEBASE_HELPER is set it behaves like a tiny app instead of running tests.
// That way these tests start, crash and kill real processes.
func TestMain(m *testing.M) {
	mode := os.Getenv("HOMEBASE_HELPER")
	addr := os.Getenv("HELPER_ADDR")
	switch mode {
	case "":
		os.Exit(m.Run())
	case "serve", "fail500":
		fmt.Println("helper ready")
		fmt.Fprintln(os.Stderr, "a line on stderr")
		fmt.Print("no trailing newline")
		code := http.StatusOK
		if mode == "fail500" {
			code = http.StatusInternalServerError
		}
		err := http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	case "crash":
		fmt.Println("about to crash")
		os.Exit(3)
	case "exit0":
		os.Exit(0)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// helperApp configures an app that runs this test binary in the given mode.
func helperApp(t *testing.T, mode, addr string, mut func(*config.App)) *App {
	t.Helper()
	self, err := os.Executable() // absolute path: Go won't run a relative one
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.App{
		Name:           "helper",
		Title:          "Helper",
		Dir:            t.TempDir(),
		Command:        self,
		Env:            map[string]string{"HOMEBASE_HELPER": mode, "HELPER_ADDR": addr},
		Autostart:      true,
		Restart:        config.RestartOnFailure,
		StopTimeoutSec: 5,
		UnhealthyAfter: 2,
	}
	if addr != "" {
		cfg.Health = "http://" + addr + "/"
	}
	if mut != nil {
		mut(&cfg)
	}
	a := NewApp(cfg, t.TempDir())
	a.timing = timing{backoffBase: 50 * time.Millisecond, backoffMax: 200 * time.Millisecond, stableAfter: time.Minute, health: 100 * time.Millisecond}
	return a
}

// run starts the supervisor and returns a function that stops it and waits.
func run(t *testing.T, a *App) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("supervisor didn't shut down")
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, what string, cond func(Status) bool, a *App) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := a.Status(); cond(s) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; status: %+v", what, a.Status())
	return Status{}
}

func stateIs(want State) func(Status) bool { return func(s Status) bool { return s.State == want } }

func logText(a *App) string {
	var b strings.Builder
	for _, l := range a.Logs.Since(0) {
		fmt.Fprintf(&b, "[%s] %s\n", l.Stream, l.Text)
	}
	return b.String()
}

func TestStartsBecomesHealthyAndCapturesOutput(t *testing.T) {
	a := helperApp(t, "serve", freeAddr(t), nil)
	run(t, a)

	s := waitFor(t, "running", stateIs(Running), a)
	if s.PID == 0 || s.StartedAt == nil || s.Health == nil || !s.Health.OK {
		t.Errorf("unexpected status: %+v", s)
	}
	waitFor(t, "output", func(Status) bool { return strings.Contains(logText(a), "[err] a line on stderr") }, a)
	if !strings.Contains(logText(a), "[out] helper ready") {
		t.Errorf("stdout not captured:\n%s", logText(a))
	}
}

func TestManualStartIsNoticedQuickly(t *testing.T) {
	// With the normal 15s interval, a manual start must still show as
	// running within a couple of seconds.
	a := helperApp(t, "serve", freeAddr(t), func(c *config.App) { c.Autostart = false; c.HealthIntervalSec = 15 })
	a.timing.health = 0
	run(t, a)
	time.Sleep(100 * time.Millisecond) // let the checker go to sleep

	if err := a.Do("start"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	waitFor(t, "running", stateIs(Running), a)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %s to notice the app was up", took)
	}
}

func TestStopKillsTheProcess(t *testing.T) {
	addr := freeAddr(t)
	a := helperApp(t, "serve", addr, nil)
	run(t, a)
	waitFor(t, "running", stateIs(Running), a)

	if err := a.Do("stop"); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, "stopped", stateIs(Stopped), a)
	if s.PID != 0 || s.Restarts != 0 || s.LastExit == nil || !s.LastExit.Requested {
		t.Errorf("after stop: %+v (last exit %+v)", s, s.LastExit)
	}
	// Really gone: nothing answers any more.
	if resp, err := http.Get("http://" + addr + "/"); err == nil {
		resp.Body.Close()
		t.Error("app still answering after stop")
	}
	// A deliberate stop must not trigger a restart.
	time.Sleep(300 * time.Millisecond)
	if st := a.Status().State; st != Stopped {
		t.Errorf("state changed to %s after stop", st)
	}
}

func TestRestartGivesANewProcess(t *testing.T) {
	a := helperApp(t, "serve", freeAddr(t), nil)
	run(t, a)
	first := waitFor(t, "running", stateIs(Running), a).PID

	if err := a.Do("restart"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new process", func(s Status) bool { return s.State == Running && s.PID != 0 && s.PID != first }, a)
}

func TestCrashIsRestartedWithBackoff(t *testing.T) {
	a := helperApp(t, "crash", "", nil)
	run(t, a)

	s := waitFor(t, "several restarts", func(s Status) bool { return s.Restarts >= 3 }, a)
	if s.LastExit == nil || s.LastExit.Code != 3 || s.LastExit.Requested {
		t.Errorf("last exit: %+v", s.LastExit)
	}
	if !strings.Contains(logText(a), "restarting in") {
		t.Errorf("no backoff logged:\n%s", logText(a))
	}
}

func TestCleanExitIsNotRestartedOnFailurePolicy(t *testing.T) {
	a := helperApp(t, "exit0", "", nil)
	run(t, a)

	waitFor(t, "stopped", func(s Status) bool { return s.State == Stopped && s.LastExit != nil }, a)
	time.Sleep(300 * time.Millisecond)
	if s := a.Status(); s.Restarts != 0 || s.State != Stopped {
		t.Errorf("clean exit was restarted: %+v", s)
	}
}

func TestNeverPolicyLeavesCrashedApp(t *testing.T) {
	a := helperApp(t, "crash", "", func(c *config.App) { c.Restart = config.RestartNever })
	run(t, a)

	s := waitFor(t, "crashed", stateIs(Crashed), a)
	if s.Restarts != 0 || !strings.Contains(s.Message, "code 3") {
		t.Errorf("unexpected: %+v", s)
	}
}

func TestStopDuringBackoffCancelsTheRestart(t *testing.T) {
	a := helperApp(t, "crash", "", nil)
	a.timing.backoffBase = time.Hour // stay in backoff
	a.timing.backoffMax = time.Hour
	run(t, a)
	waitFor(t, "backoff", stateIs(Backoff), a)

	if err := a.Do("stop"); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, "stopped", stateIs(Stopped), a)
	if s.NextRestartAt != nil {
		t.Error("restart still scheduled after stop")
	}
}

func TestUnhealthyAppIsRestarted(t *testing.T) {
	a := helperApp(t, "fail500", freeAddr(t), func(c *config.App) { c.RestartWhenUnhealthy = true })
	run(t, a)
	// Starting -> never healthy, so force it to count as running first:
	// the rule only applies to apps that were up and then went bad.
	waitFor(t, "process started", func(s Status) bool { return s.PID != 0 }, a)
	a.update(func(s *Status) { s.State = Running })

	waitFor(t, "unhealthy restart", func(s Status) bool { return s.Restarts >= 1 }, a)
	if !strings.Contains(logText(a), "health check failed 2 times") {
		t.Errorf("reason not logged:\n%s", logText(a))
	}
}

func TestExternalCopyIsDetectedNotDuplicated(t *testing.T) {
	// Something else is already serving on the health URL.
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ext.Close()
	a := helperApp(t, "serve", strings.TrimPrefix(ext.URL, "http://"), nil)
	run(t, a)

	s := waitFor(t, "external", stateIs(External), a)
	if s.PID != 0 {
		t.Errorf("started a duplicate (pid %d)", s.PID)
	}
	if err := a.Do("stop"); err == nil || !strings.Contains(err.Error(), "wasn't started by homebase") {
		t.Errorf("stop on external copy: %v", err)
	}
}

func TestShutdownStopsRunningApps(t *testing.T) {
	addr := freeAddr(t)
	a := helperApp(t, "serve", addr, nil)
	stop := run(t, a)
	waitFor(t, "running", stateIs(Running), a)

	stop()
	if resp, err := http.Get("http://" + addr + "/"); err == nil {
		resp.Body.Close()
		t.Error("app left running after homebase shut down")
	}
}

func TestEnvFileReachesTheApp(t *testing.T) {
	dir := t.TempDir()
	envFile := dir + "/secrets.env"
	// The env file picks the helper's mode, proving its values are applied.
	if err := os.WriteFile(envFile, []byte("# comment\nHOMEBASE_HELPER=\"crash\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := helperApp(t, "serve", "", func(c *config.App) {
		c.EnvFile = envFile
		delete(c.Env, "HOMEBASE_HELPER")
		c.Restart = config.RestartNever
	})
	run(t, a)
	s := waitFor(t, "crashed via env-file mode", stateIs(Crashed), a)
	// Exit code 3 comes only from "crash" mode, so the env file was applied
	// (a failure to start would leave no exit code at all).
	if s.LastExit == nil || s.LastExit.Code != 3 {
		t.Errorf("env file not applied: %+v", s)
	}
}

func TestBadCommandFailsCleanly(t *testing.T) {
	a := helperApp(t, "serve", "", func(c *config.App) { c.Command = "definitely-not-a-real-program-xyz" })
	run(t, a)
	s := waitFor(t, "crashed", stateIs(Crashed), a)
	if !strings.Contains(s.Message, "Failed to start") {
		t.Errorf("message: %q", s.Message)
	}
}

func TestLogLineSplittingAndRing(t *testing.T) {
	l := NewLogs("", 3, 0)
	w := &lineWriter{logs: l, stream: "out"}
	w.Write([]byte("one\r\ntw"))
	w.Write([]byte("o\nthree\nfour\nfi"))
	w.flush()

	var got []string
	for _, ln := range l.Since(0) {
		got = append(got, ln.Text)
	}
	if strings.Join(got, ",") != "three,four,fi" { // only the last 3 kept
		t.Errorf("got %v", got)
	}
	if n := len(l.Since(l.Since(0)[1].Seq)); n != 1 {
		t.Errorf("Since returned %d lines, want 1", n)
	}
}

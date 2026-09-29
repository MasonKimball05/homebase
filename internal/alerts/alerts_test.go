package alerts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MasonKimball05/homebase/internal/config"
	"github.com/MasonKimball05/homebase/internal/supervisor"
)

// fakeNotifier records messages, and can be told to fail.
type fakeNotifier struct {
	got  []Message
	fail bool
}

func (f *fakeNotifier) Notify(_ context.Context, m Message) error {
	if f.fail {
		return errors.New("offline")
	}
	f.got = append(f.got, m)
	return nil
}

// harness drives a Watcher with a fake clock and hand-written statuses.
type harness struct {
	t      *testing.T
	w      *Watcher
	n      *fakeNotifier
	clock  time.Time
	status map[string]*supervisor.Status
	order  []string
}

func newHarness(t *testing.T, cfg config.Alerts, apps ...string) *harness {
	if cfg.CrashLoopRestarts == 0 {
		cfg.CrashLoopRestarts = 3
	}
	if cfg.CrashLoopWindowMin == 0 {
		cfg.CrashLoopWindowMin = 10
	}
	h := &harness{t: t, n: &fakeNotifier{}, clock: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC), status: map[string]*supervisor.Status{}}
	for _, name := range apps {
		started := h.clock.Add(-time.Hour)
		h.status[name] = &supervisor.Status{Name: name, Title: strings.ToUpper(name[:1]) + name[1:], State: supervisor.Running, StartedAt: &started}
		h.order = append(h.order, name)
	}
	h.w = NewWatcher(h.statuses, h.n, cfg, "arkans-pc1", func(string, ...any) {})
	h.w.now = func() time.Time { return h.clock }
	h.w.started = h.clock
	return h
}

func (h *harness) statuses() []supervisor.Status {
	out := []supervisor.Status{}
	for _, name := range h.order {
		out = append(out, *h.status[name])
	}
	return out
}

func (h *harness) tick(advance time.Duration) []Message {
	h.clock = h.clock.Add(advance)
	before := len(h.n.got)
	h.w.Tick(context.Background())
	return h.n.got[before:]
}

func (h *harness) crash(name string, code int) {
	s := h.status[name]
	s.State = supervisor.Backoff
	s.Restarts++
	s.LastExit = &supervisor.Exit{Code: code, At: h.clock}
}

func (h *harness) runFresh(name string) {
	s := h.status[name]
	started := h.clock
	s.State, s.StartedAt = supervisor.Running, &started
}

func expectOne(t *testing.T, msgs []Message, titleContains string) Message {
	t.Helper()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Title, titleContains) {
		t.Fatalf("want one message containing %q, got %+v", titleContains, msgs)
	}
	return msgs[0]
}

func expectNone(t *testing.T, msgs []Message) {
	t.Helper()
	if len(msgs) != 0 {
		t.Fatalf("want no messages, got %+v", msgs)
	}
}

func TestHealthyAppsStayQuiet(t *testing.T) {
	h := newHarness(t, config.Alerts{}, "web")
	for i := 0; i < 10; i++ {
		expectNone(t, h.tick(5*time.Second))
	}
}

func TestOneCrashThatRecoversIsNotAnAlert(t *testing.T) {
	h := newHarness(t, config.Alerts{}, "web")
	h.tick(0)
	h.crash("web", 1)
	expectNone(t, h.tick(5*time.Second))
	h.runFresh("web")
	expectNone(t, h.tick(5*time.Second))
}

func TestCrashLoopAlertsOnceThenRecovers(t *testing.T) {
	h := newHarness(t, config.Alerts{}, "web")
	h.tick(0)
	for i := 0; i < 3; i++ {
		h.crash("web", 2)
		msgs := h.tick(10 * time.Second)
		if i < 2 {
			expectNone(t, msgs)
		} else {
			m := expectOne(t, msgs, "Web is crash-looping on arkans-pc1")
			if m.Priority != "urgent" || !strings.Contains(m.Body, "3 restarts in 10 min; last exit code 2") {
				t.Errorf("message: %+v", m)
			}
		}
		h.runFresh("web")
		h.tick(time.Second)
	}
	// Keeps crashing: no repeat alert.
	h.crash("web", 2)
	expectNone(t, h.tick(10*time.Second))

	// Up again, but not for long enough to count as fixed.
	h.runFresh("web")
	expectNone(t, h.tick(time.Minute))
	// Stable for 2 minutes: recovered.
	m := expectOne(t, h.tick(90*time.Second), "Web recovered")
	if m.Priority != "default" || !strings.Contains(m.Body, "was crash-looping") {
		t.Errorf("recovery: %+v", m)
	}
}

func TestCrashedForGood(t *testing.T) {
	h := newHarness(t, config.Alerts{}, "web")
	h.tick(0)
	s := h.status["web"]
	s.State, s.Message = supervisor.Crashed, "Exited with code 1."
	m := expectOne(t, h.tick(5*time.Second), "Web is crashed")
	if !strings.Contains(m.Body, "Exited with code 1.") {
		t.Errorf("body: %q", m.Body)
	}
}

func TestUnhealthyNeedsThreeFailures(t *testing.T) {
	h := newHarness(t, config.Alerts{}, "web")
	s := h.status["web"]
	for fails := 1; fails <= 3; fails++ {
		s.Health = &supervisor.Health{OK: false, Failures: fails, Error: "503 Service Unavailable"}
		msgs := h.tick(15 * time.Second)
		if fails < 3 {
			expectNone(t, msgs)
		} else {
			m := expectOne(t, msgs, "Web is unhealthy")
			if m.Priority != "high" || !strings.Contains(m.Body, "503") {
				t.Errorf("message: %+v", m)
			}
		}
	}
	s.Health = &supervisor.Health{OK: true}
	expectOne(t, h.tick(15*time.Second), "Web recovered")
}

func TestStoppingAProblemAppIsNotARecovery(t *testing.T) {
	h := newHarness(t, config.Alerts{}, "web")
	s := h.status["web"]
	s.Health = &supervisor.Health{OK: false, Failures: 3}
	expectOne(t, h.tick(5*time.Second), "unhealthy")

	// You press Stop: the problem is gone, but "recovered" would be a lie.
	s.State, s.Health = supervisor.Stopped, nil
	expectNone(t, h.tick(5*time.Second))
}

func TestSimultaneousProblemsAreOneNotification(t *testing.T) {
	h := newHarness(t, config.Alerts{}, "web", "api")
	h.status["web"].State = supervisor.Crashed
	h.status["api"].Health = &supervisor.Health{OK: false, Failures: 4}

	m := expectOne(t, h.tick(5*time.Second), "2 apps need attention")
	if m.Priority != "urgent" { // the worst problem sets the priority
		t.Errorf("priority %q", m.Priority)
	}
	if strings.Index(m.Body, "Web is crashed") > strings.Index(m.Body, "Api is unhealthy") {
		t.Errorf("worst problem should come first:\n%s", m.Body)
	}
}

func TestStartupSummaryAfterAMinute(t *testing.T) {
	h := newHarness(t, config.Alerts{StartupSummary: true}, "web", "api")
	h.status["api"].State = supervisor.Crashed
	h.status["api"].Message = "Exited with code 1."

	msgs := h.tick(10 * time.Second) // "api crashed" alert, but no summary yet
	expectOne(t, msgs, "Api is crashed")

	m := expectOne(t, h.tick(time.Minute), "arkans-pc1 is up: 1/2 apps running")
	if !strings.Contains(m.Body, "✅ Web") || !strings.Contains(m.Body, "Api: crashed") {
		t.Errorf("summary body:\n%s", m.Body)
	}
	expectNone(t, h.tick(time.Hour)) // only once
}

func TestFailedDeliveryIsRetried(t *testing.T) {
	h := newHarness(t, config.Alerts{}, "web")
	h.n.fail = true // e.g. Wi-Fi not up yet right after boot
	h.status["web"].State = supervisor.Crashed
	expectNone(t, h.tick(5*time.Second))

	h.n.fail = false
	expectOne(t, h.tick(5*time.Second), "Web is crashed")
	expectNone(t, h.tick(5*time.Second)) // not sent twice
}

func TestRestartsBeforeWatchingDontCount(t *testing.T) {
	h := newHarness(t, config.Alerts{}, "web")
	h.status["web"].Restarts = 7 // restarted a lot before homebase's watcher started
	expectNone(t, h.tick(5*time.Second))
}

func TestNtfyRequest(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()

	n := Ntfy{URL: srv.URL + "/topic", Click: "http://arkans-pc1:8090/"}
	err := n.Notify(context.Background(), Message{Title: "Web is crashed", Body: "❗ details", Priority: "urgent", Tags: "rotating_light"})
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"Title": "Web is crashed", "Priority": "urgent", "Tags": "rotating_light", "Click": "http://arkans-pc1:8090/"} {
		if got.Header.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Header.Get(k), want)
		}
	}
	if body != "❗ details" {
		t.Errorf("body %q", body)
	}
}

func TestNtfyErrorsHideTheTopic(t *testing.T) {
	err := Ntfy{URL: "https://127.0.0.1:1/very-secret-topic"}.Notify(context.Background(), Message{})
	if err == nil || strings.Contains(err.Error(), "very-secret-topic") {
		t.Errorf("err = %v", err)
	}
}

// Package alerts watches app statuses and pushes a phone notification when
// something needs attention, and again when it's fixed.
//
// It only reads statuses (it never touches processes), so a bug here can't
// affect the apps themselves.
package alerts

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/MasonKimball05/homebase/internal/config"
	"github.com/MasonKimball05/homebase/internal/supervisor"
)

// Problem kinds, worst first.
const (
	crashed     = "crashed"
	crashLoop   = "crash-looping"
	unhealthy   = "unhealthy"
	healthyNow  = "" // no problem
	unhealthyAt = 3  // consecutive failed checks before "unhealthy"
	// A crash-looping app must stay up this long before it counts as recovered.
	stableFor = 2 * time.Minute
	// The morning summary waits for apps to settle after boot.
	summaryDelay = 60 * time.Second
	maxPending   = 20
)

// Message is one notification.
type Message struct {
	Title    string
	Body     string
	Priority string // "urgent", "high", "default", "low"
	Tags     string // ntfy emoji shortcodes, e.g. "rotating_light"
}

// Notifier delivers a message (ntfy in production, a fake in tests).
type Notifier interface {
	Notify(ctx context.Context, m Message) error
}

type appState struct {
	lastRestarts int
	restarts     []time.Time // restart times inside the crash-loop window
	problem      string      // what we last alerted about ("" = nothing)
}

type Watcher struct {
	statuses func() []supervisor.Status
	notifier Notifier // nil: alerts off, only logged
	cfg      config.Alerts
	host     string
	logf     func(format string, args ...any)
	now      func() time.Time

	started     time.Time
	summaryDone bool
	apps        map[string]*appState
	pending     []Message // not yet delivered; retried next tick
}

func NewWatcher(statuses func() []supervisor.Status, n Notifier, cfg config.Alerts, host string, logf func(string, ...any)) *Watcher {
	return &Watcher{
		statuses: statuses, notifier: n, cfg: cfg, host: host, logf: logf,
		now: time.Now, started: time.Now(), apps: map[string]*appState{},
	}
}

// Run checks every few seconds until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.Tick(ctx)
		}
	}
}

type change struct {
	app, title    string
	from, to      string
	detail        string
	problemSorted int // for ordering: worst first
}

// Tick evaluates every app once and sends at most one notification.
func (w *Watcher) Tick(ctx context.Context) {
	now := w.now()
	statuses := w.statuses()
	var changes []change

	for _, s := range statuses {
		st := w.apps[s.Name]
		if st == nil {
			// First sight: don't count restarts from before homebase started watching.
			st = &appState{lastRestarts: s.Restarts}
			w.apps[s.Name] = st
		}
		for ; st.lastRestarts < s.Restarts; st.lastRestarts++ {
			st.restarts = append(st.restarts, now)
		}
		cutoff := now.Add(-w.cfg.CrashLoopWindow())
		st.restarts = slices.DeleteFunc(st.restarts, func(t time.Time) bool { return t.Before(cutoff) })

		problem, detail, known := w.classify(s, st, now)
		if !known || problem == st.problem {
			continue
		}
		// Leaving a problem because you stopped the app isn't news.
		if problem == healthyNow && s.State != supervisor.Running {
			st.problem = healthyNow
			continue
		}
		changes = append(changes, change{app: s.Name, title: s.Title, from: st.problem, to: problem, detail: detail, problemSorted: rank(problem)})
		st.problem = problem
	}

	if len(changes) > 0 {
		w.queue(format(changes, w.host))
	}
	if w.cfg.StartupSummary && !w.summaryDone && now.Sub(w.started) >= summaryDelay {
		w.summaryDone = true
		w.queue(summary(statuses, w.host))
	}
	w.flush(ctx)
}

// classify decides what, if anything, is wrong with an app right now.
// known=false means "in transition, keep the previous verdict" (starting,
// stopping, waiting to restart), which avoids flip-flopping notifications.
func (w *Watcher) classify(s supervisor.Status, st *appState, now time.Time) (problem, detail string, known bool) {
	if len(st.restarts) >= w.cfg.CrashLoopRestarts {
		d := fmt.Sprintf("%d restarts in %d min", len(st.restarts), w.cfg.CrashLoopWindowMin)
		if s.LastExit != nil {
			d += fmt.Sprintf("; last exit code %d", s.LastExit.Code)
		}
		// A looping app has to prove it's stable before we call it fixed.
		if s.State == supervisor.Running && s.StartedAt != nil && now.Sub(*s.StartedAt) >= stableFor {
			st.restarts = nil
		} else {
			return crashLoop, d, true
		}
	}

	switch s.State {
	case supervisor.Crashed:
		return crashed, s.Message, true
	case supervisor.Running:
		if s.Health != nil && !s.Health.OK && s.Health.Failures >= unhealthyAt {
			d := fmt.Sprintf("health check failed %d times", s.Health.Failures)
			if s.Health.Error != "" {
				d += ": " + s.Health.Error
			}
			return unhealthy, d, true
		}
		if st.problem == crashLoop && s.StartedAt != nil && now.Sub(*s.StartedAt) < stableFor {
			return "", "", false // still proving itself
		}
		return healthyNow, "", true
	case supervisor.Stopped, supervisor.External:
		return healthyNow, "", true
	default: // starting, stopping, backoff
		return "", "", false
	}
}

func rank(problem string) int {
	switch problem {
	case crashed:
		return 0
	case crashLoop:
		return 1
	case unhealthy:
		return 2
	default:
		return 3 // recoveries last
	}
}

// format turns this tick's changes into one notification, so the whole
// desktop going sideways is one buzz, not five.
func format(changes []change, host string) Message {
	slices.SortStableFunc(changes, func(a, b change) int { return a.problemSorted - b.problemSorted })

	var lines []string
	problems, recoveries := 0, 0
	worst := ""
	for _, c := range changes {
		if c.to == healthyNow {
			recoveries++
			lines = append(lines, fmt.Sprintf("✅ %s recovered (was %s)", c.title, c.from))
			continue
		}
		problems++
		if worst == "" {
			worst = c.to
		}
		line := fmt.Sprintf("❗ %s is %s", c.title, c.to)
		if c.detail != "" {
			line += ": " + c.detail
		}
		lines = append(lines, line)
	}

	m := Message{Body: strings.Join(lines, "\n")}
	switch {
	case problems == 0:
		m.Priority, m.Tags = "default", "white_check_mark"
		if recoveries == 1 {
			m.Title = fmt.Sprintf("%s recovered", changes[0].title)
		} else {
			m.Title = fmt.Sprintf("%d apps recovered", recoveries)
		}
	default:
		m.Tags = "rotating_light"
		m.Priority = "urgent"
		if worst == unhealthy {
			m.Priority, m.Tags = "high", "warning"
		}
		if problems == 1 {
			m.Title = fmt.Sprintf("%s is %s", changes[0].title, changes[0].to)
		} else {
			m.Title = fmt.Sprintf("%d apps need attention", problems)
		}
	}
	m.Title += " on " + host
	return m
}

func summary(statuses []supervisor.Status, host string) Message {
	up := 0
	var lines []string
	for _, s := range statuses {
		if s.State == supervisor.Running && (s.Health == nil || s.Health.OK) {
			up++
			lines = append(lines, "✅ "+s.Title)
		} else {
			lines = append(lines, fmt.Sprintf("⚠️ %s: %s", s.Title, stateWord(s.State)))
		}
	}
	m := Message{
		Title:    fmt.Sprintf("%s is up: %d/%d apps running", host, up, len(statuses)),
		Body:     strings.Join(lines, "\n"),
		Priority: "low",
		Tags:     "sunrise",
	}
	if up < len(statuses) {
		m.Priority, m.Tags = "default", "warning"
	}
	return m
}

// stateWord matches the dashboard's wording.
func stateWord(s supervisor.State) string {
	if s == supervisor.Backoff {
		return "restarting"
	}
	return string(s)
}

func (w *Watcher) queue(m Message) {
	w.logf("alert: %s | %s", m.Title, strings.ReplaceAll(m.Body, "\n", " | "))
	if w.notifier == nil {
		return
	}
	w.pending = append(w.pending, m)
	if len(w.pending) > maxPending {
		w.pending = w.pending[len(w.pending)-maxPending:]
	}
}

// flush delivers queued messages in order, stopping at the first failure
// (e.g. no internet yet right after boot) and retrying next tick.
func (w *Watcher) flush(ctx context.Context) {
	for len(w.pending) > 0 {
		if err := w.notifier.Notify(ctx, w.pending[0]); err != nil {
			w.logf("alert delivery failed (will retry): %v", err)
			return
		}
		w.pending = w.pending[1:]
	}
}

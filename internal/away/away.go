// Package away works through a queue of coding tasks while you're away from
// this PC. For each task:
//
//  1. a git worktree of the project on a new branch, away/<id>, from the
//     commit the project is on (your own checkout is never touched);
//  2. the local model (Qwen, through Loom's agent loop: tools/qwen in the
//     Runner checkout) does the task there; its changes are committed;
//  3. Claude Code reviews the branch, read-only, and answers pass, revise
//     (with notes) or fail. On revise the local model gets the notes and
//     another round, up to Rounds;
//  4. if it still hasn't passed, Claude fixes the branch itself;
//  5. a phone notification, and the branch waits for you: Accept merges it
//     into the branch it started from, Discard deletes it.
//
// Nothing is ever pushed. The queue runs while the mode is "on", or in
// "idle" mode while nobody has touched this PC for IdleMinutes (and stops
// the moment someone does: the task goes back to the front of the queue).
package away

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ModeOff  = "off"
	ModeOn   = "on"
	ModeIdle = "idle"

	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusDone      = "done"      // passed review, or fixed by Claude: waiting for Accept or Discard
	StatusNeedsYou  = "needs-you" // Claude said the task can't or shouldn't be done as written
	StatusFailed    = "failed"    // something broke (see Error and the log)
	StatusAccepted  = "accepted"
	StatusDiscarded = "discarded"
	StatusCancelled = "cancelled"
)

// Round is one step of a task: the local model's attempt, a review, or Claude's fix.
type Round struct {
	Who     string `json:"who"` // "qwen", "review", "claude-fix", "check"
	Verdict string `json:"verdict,omitempty"`
	Notes   string `json:"notes,omitempty"`
	Seconds int    `json:"seconds"`
	Commit  string `json:"commit,omitempty"`
	// How the model showed its thinking, apart from the verdict. Qwen rounds:
	// how much it explained, counted from its transcript. Reviews: the
	// reviewer's 1-5 grade of how good its stated reasons were, whether what
	// it said contradicted what it did, and why.
	Thinking       *ThinkingStats `json:"thinking,omitempty"`
	Quality        int            `json:"quality,omitempty"`
	Misleading     bool           `json:"misleading,omitempty"`
	ReasoningNotes string         `json:"reasoningNotes,omitempty"`
}

// ThinkingStats: how much the model explained itself in a round, counted
// from its transcript without asking any model (thinkingStats): of its tool
// calls, the share with something said just before that's more than what
// it'll look at next ("Let me read the file:", which doesn't count); of its
// working turns (its summaries left out), the share giving a reason, and the
// share that are only that filler.
type ThinkingStats struct {
	Calls     int `json:"calls"`
	Explained int `json:"explained"` // % of Calls
	Turns     int `json:"turns"`
	Reasons   int `json:"reasons"` // % of Turns
	Filler    int `json:"filler"`  // % of Turns
}

type Task struct {
	ID         string  `json:"id"`
	Project    string  `json:"project"`
	Text       string  `json:"text"`
	Status     string  `json:"status"`
	Stage      string  `json:"stage,omitempty"` // what it's doing now, while running
	Branch     string  `json:"branch,omitempty"`
	Base       string  `json:"base,omitempty"` // the branch it started from (Accept merges into it)
	BaseCommit string  `json:"baseCommit,omitempty"`
	Verdict    string  `json:"verdict,omitempty"` // "pass", "fixed", "fail"
	Notes      string  `json:"notes,omitempty"`   // Claude's last word, for you
	Rounds     []Round `json:"rounds"`
	Error      string  `json:"error,omitempty"`
	Changed    int     `json:"changed"` // files changed on the branch
	Pushed     bool    `json:"pushed,omitempty"`
	// Shown: the examples (recall.go) the local model was shown for this attempt.
	Shown []string `json:"shown,omitempty"`
	// HeldOut: this attempt ran without examples on purpose (RecallHoldout).
	HeldOut bool `json:"heldOut,omitempty"`
	// Auto: queued by Away itself (refill.go), "tests" or "drill"; "" when
	// someone added it.
	Auto string `json:"auto,omitempty"`
	// Drill: the bug planted for a drill (run.go plants it, the review is told).
	Drill    *Drill    `json:"drill,omitempty"`
	Created  time.Time `json:"created"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
}

// Settings is what Service needs (config.Away with defaults filled in, plus paths).
type Settings struct {
	Runner, Worktrees, Claude, Ollama, Model string
	IdleMinutes, Rounds                      int
	// Push: projects whose away/… branches are pushed to their origin when
	// done (a private clone you review on GitHub). Never main, never --force.
	Push []string
	// Tests: a project's test command for the local model's run_tests tool
	// (Loom's runner can't work out every project's, e.g. Django's).
	Tests       map[string]string
	StatePath   string // the queue, as JSON
	ExamplesDir string // one file per finished attempt (examples.go); "" = don't keep them
	EmbedModel  string // for recall.go (Ollama); "" or "off" = don't show examples
	// RecallHoldout: every Nth task (by its id) runs without examples on
	// purpose, so with and without can be compared fairly. 0 = never.
	RecallHoldout int
	IdlePath      string // written by `homebase -idle-report`
	LogDir        string // one log per task
	// Autofill: projects Away may queue work for itself when the queue is
	// empty, and how many such tasks a day (refill.go). Python test
	// commands only.
	Autofill map[string]int
}

// Projects finds a project's folder by id (internal/projects).
type Projects interface {
	Path(id string) (string, bool)
}

// Steps does the work; Service decides when. (Real: steps.go; tests: a fake.)
type Steps interface {
	// Qwen runs the local model on the task in dir; its log goes to log.
	// tests is the command behind its run_tests tool ("": the runner picks one);
	// its transcript (Loom's conversation items, as JSON) goes to transcript.
	Qwen(ctx context.Context, dir, task, tests, transcript string, log *os.File) error
	// Tests runs a test command line in dir: whether it passed, and its output.
	Tests(ctx context.Context, dir, command string, log *os.File) (bool, string)
	// Claude runs Claude Code in dir with the prompt; edit lets it change files. Returns its reply.
	Claude(ctx context.Context, dir, prompt string, edit bool, log *os.File) (string, error)
}

type state struct {
	Mode string `json:"mode"`
	// Stopped: why Away switched itself off (the local model broke), until
	// someone sets the mode again.
	Stopped string `json:"stopped,omitempty"`
	// Waiting: the local model didn't answer, so Away keeps its mode but
	// starts nothing until Retry (run.go), until a task gets past the model
	// or someone sets the mode again.
	Waiting *Wait   `json:"waiting,omitempty"`
	Tasks   []*Task `json:"tasks"`
	// Tried: functions an automatic refill already used or gave up on
	// (refill.go), so it doesn't pick them again.
	Tried []string `json:"tried,omitempty"`
}

// Wait: the local model didn't answer (10-21: Ollama not ready yet, still
// updating itself just after the PC was switched on), so Away waits a while
// and tries again before giving up.
type Wait struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"` // the first try that got no answer
	At     time.Time `json:"at"`    // the last one
	Retry  time.Time `json:"retry"` // when the next one starts
	Tries  int       `json:"tries"` // tries without an answer so far
	Max    int       `json:"max"`   // Away switches off after this many
}

// How long Away waits for a model that didn't answer, and how many tries
// it gets in all (about half an hour) before Away switches off.
const (
	modelWait  = 10 * time.Minute
	modelTries = 4
)

type Service struct {
	set      Settings
	projects Projects
	steps    Steps
	notify   func(title, body string)
	logf     func(string, ...any)
	embed    Embedder  // for recall.go; nil = no examples shown
	generate Generator // for suggest.go's drafts; nil = no drafting
	now      func() time.Time

	mu      sync.Mutex
	st      state
	cancel  context.CancelFunc // the running task's
	running string
	byIdle  bool // the running task was started by idle mode (so activity stops it)
	// refillAfter: a refill that found nothing to queue waits until then.
	refillAfter time.Time
	wake        chan struct{} // something changed: look at the queue now
}

func New(set Settings, projects Projects, steps Steps, notify func(title, body string), logf func(string, ...any)) *Service {
	if set.IdleMinutes <= 0 {
		set.IdleMinutes = 20
	}
	if set.Rounds <= 0 {
		set.Rounds = 2
	}
	if notify == nil {
		notify = func(string, string) {}
	}
	s := &Service{set: set, projects: projects, steps: steps, notify: notify, logf: logf, now: time.Now, wake: make(chan struct{}, 1)}
	if set.EmbedModel != "" && set.EmbedModel != "off" && set.Ollama != "" {
		s.embed = OllamaEmbedder(set.Ollama, set.EmbedModel)
	}
	if set.Ollama != "" && set.Model != "" {
		s.generate = OllamaGenerator(set.Ollama, set.Model)
	}
	s.st = state{Mode: ModeOff}
	if b, err := os.ReadFile(set.StatePath); err == nil {
		_ = json.Unmarshal(b, &s.st)
	}
	// homebase stopped mid-task last time (a restart, an update): its
	// worktree and branch are still there. With commits on the branch, that's
	// work worth keeping: it stops there for you to look at (Accept, Discard,
	// Try again). With none, it's cleared and the task starts over.
	for _, t := range s.st.Tasks {
		if t.Status != StatusRunning {
			continue
		}
		repo, _ := projects.Path(t.Project)
		if repo != "" && t.Branch != "" && t.BaseCommit != "" {
			removeWorktree(context.Background(), repo, s.worktree(*t))
			if out, err := git(context.Background(), repo, "rev-list", "--count", t.BaseCommit+".."+t.Branch); err == nil && strings.TrimSpace(out) != "0" {
				t.Status, t.Stage, t.Finished = StatusFailed, "", s.now()
				t.Error = "homebase restarted during this attempt; what it had done is on the branch (Changes), unreviewed"
				continue
			}
			dropBranch(context.Background(), repo, "", t.Branch)
		}
		t.Status, t.Stage = StatusQueued, ""
		t.Branch, t.Base, t.BaseCommit, t.Rounds, t.Changed, t.Shown = "", "", "", []Round{}, 0, nil
	}
	if s.st.Mode != ModeOn && s.st.Mode != ModeIdle {
		s.st.Mode = ModeOff
	}
	return s
}

// ---------------- the loop ----------------

// Run starts tasks while the mode allows it, and stops one when it no longer does.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		s.step(ctx)
		select {
		case <-ctx.Done():
			s.mu.Lock()
			if s.cancel != nil {
				s.cancel()
			}
			s.mu.Unlock()
			return
		case <-tick.C:
		case <-s.wake:
		}
	}
}

func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// step: one look at the mode, the idle time and the queue.
func (s *Service) step(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idle, known := s.idleFor()
	away := s.st.Mode == ModeOn || (s.st.Mode == ModeIdle && known && idle >= time.Duration(s.set.IdleMinutes)*time.Minute)
	if s.running != "" {
		// Back at the PC (idle mode), or switched off: stop, and put it back at the front.
		back := s.byIdle && s.st.Mode == ModeIdle && (!known || idle < time.Minute)
		if s.st.Mode == ModeOff || back {
			s.cancel()
		}
		return
	}
	if !away {
		return
	}
	if w := s.st.Waiting; w != nil && s.now().Before(w.Retry) {
		return
	}
	var next *Task
	for _, t := range s.st.Tasks {
		if t.Status == StatusQueued {
			next = t
			break
		}
	}
	if next == nil {
		// Nothing queued: maybe Away can queue something itself (refill.go).
		if p := s.refillProject(); p != "" {
			tctx, cancel := context.WithCancel(ctx)
			s.cancel, s.running, s.byIdle = cancel, refillRunning, s.st.Mode == ModeIdle
			go s.refill(tctx, p)
		}
		return
	}
	tctx, cancel := context.WithCancel(ctx)
	s.cancel, s.running, s.byIdle = cancel, next.ID, s.st.Mode == ModeIdle
	next.Status, next.Stage, next.Started, next.Error = StatusRunning, "starting", s.now(), ""
	next.Rounds, next.Verdict, next.Notes, next.Finished = nil, "", "", time.Time{}
	s.saveLocked()
	go s.runTask(tctx, next.ID)
}

// idleFor: how long since the last keyboard or mouse input, from the
// reporter's file. Unknown if it isn't running (or stopped writing).
func (s *Service) idleFor() (time.Duration, bool) {
	b, err := os.ReadFile(s.set.IdlePath)
	if err != nil {
		return 0, false
	}
	var r IdleReport
	if json.Unmarshal(b, &r) != nil || r.Written == 0 {
		return 0, false
	}
	now := s.now().Unix()
	if now-r.Written > 120 {
		return 0, false
	}
	return time.Duration(now-r.LastInput) * time.Second, true
}

// IdleReport is the file `homebase -idle-report` keeps up to date (Unix seconds).
type IdleReport struct {
	LastInput int64 `json:"last_input"`
	Written   int64 `json:"written"`
}

// ---------------- the queue, for the API ----------------

// View is the API's answer: the mode, whether you're away, and the tasks (newest first).
type View struct {
	Mode        string  `json:"mode"`
	IdleMinutes int     `json:"idleMinutes"`
	IdleSeconds *int    `json:"idleSeconds"` // null: the idle reporter isn't running
	Running     string  `json:"running,omitempty"`
	Stopped     string  `json:"stopped,omitempty"` // why Away switched itself off
	Waiting     *Wait   `json:"waiting,omitempty"` // the local model didn't answer: when Away tries again
	Tasks       []*Task `json:"tasks"`
}

func (s *Service) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := View{Mode: s.st.Mode, IdleMinutes: s.set.IdleMinutes, Running: s.running, Stopped: s.st.Stopped}
	if s.st.Waiting != nil {
		w := *s.st.Waiting
		v.Waiting = &w
	}
	if d, ok := s.idleFor(); ok {
		n := int(d.Seconds())
		v.IdleSeconds = &n
	}
	b, _ := json.Marshal(s.st.Tasks)
	_ = json.Unmarshal(b, &v.Tasks) // a copy, so the caller can't race the runner
	sort.SliceStable(v.Tasks, func(i, j int) bool { return v.Tasks[i].Created.After(v.Tasks[j].Created) })
	if v.Tasks == nil {
		v.Tasks = []*Task{}
	}
	return v
}

func (s *Service) SetMode(mode string) error {
	if mode != ModeOff && mode != ModeOn && mode != ModeIdle {
		return errors.New("mode is off, on or idle")
	}
	s.mu.Lock()
	// Setting the mode yourself also means "try the model now".
	s.st.Mode, s.st.Stopped, s.st.Waiting = mode, "", nil
	s.saveLocked()
	s.mu.Unlock()
	s.poke()
	return nil
}

const maxTaskText = 8000

func (s *Service) Add(project, text string) (*Task, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("write the task first")
	}
	if len(text) > maxTaskText {
		return nil, errors.New("that task is too long (8000 characters at most)")
	}
	if _, ok := s.projects.Path(project); !ok {
		return nil, errors.New("no such project")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &Task{ID: s.newID(), Project: project, Text: text, Status: StatusQueued, Rounds: []Round{}, Created: s.now()}
	s.st.Tasks = append(s.st.Tasks, t)
	s.saveLocked()
	s.poke()
	return t, nil
}

func (s *Service) newID() string {
	id := s.now().Format("0102-150405")
	for n := 2; s.find(id) != nil; n++ {
		id = s.now().Format("0102-150405") + "-" + strconv.Itoa(n)
	}
	return id
}

func (s *Service) find(id string) *Task {
	for _, t := range s.st.Tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

var errNoTask = errors.New("no such task")

// Act: cancel, retry, accept, discard, delete.
func (s *Service) Act(ctx context.Context, id, action string) error {
	s.mu.Lock()
	t := s.find(id)
	if t == nil {
		s.mu.Unlock()
		return errNoTask
	}
	if action == "cancel" {
		defer s.mu.Unlock()
		switch t.Status {
		case StatusQueued:
			t.Status = StatusCancelled
		case StatusRunning:
			t.Stage = "cancelling"
			s.cancelled(id)
		default:
			return errors.New("only a queued or running task can be cancelled")
		}
		s.saveLocked()
		return nil
	}
	if t.Status == StatusRunning {
		s.mu.Unlock()
		return errors.New("it's running: cancel it first")
	}
	task := *t
	s.mu.Unlock()

	repo, ok := s.projects.Path(task.Project)
	if !ok {
		return errors.New("that project isn't listed any more")
	}
	var status string
	switch action {
	case "accept":
		// A failed attempt can be accepted when it left commits (you've seen the diff).
		if task.Status != StatusDone && task.Status != StatusNeedsYou && !(task.Status == StatusFailed && task.Branch != "") {
			return errors.New("only a finished task can be accepted")
		}
		removeWorktree(ctx, repo, s.worktree(task)) // a leftover worktree would keep the branch from being deleted
		if task.Drill != nil {
			// A drill's branch is the planted bug and its fix: nothing to merge.
			// Accepting it says the fix was right (the example counts).
			dropBranch(ctx, repo, s.worktree(task), task.Branch)
		} else if err := accept(ctx, repo, task); err != nil {
			return err
		}
		s.unpush(ctx, repo, task)
		s.markExample(task, "accepted")
		status = StatusAccepted
	case "discard":
		if task.Status != StatusDone && task.Status != StatusNeedsYou && task.Status != StatusFailed {
			return errors.New("only a finished task can be discarded")
		}
		dropBranch(ctx, repo, s.worktree(task), task.Branch)
		s.unpush(ctx, repo, task)
		s.markExample(task, "discarded")
		status = StatusDiscarded
	case "retry":
		if task.Status == StatusQueued || task.Status == StatusAccepted {
			return errors.New("nothing to retry")
		}
		dropBranch(ctx, repo, s.worktree(task), task.Branch)
		s.unpush(ctx, repo, task)
		s.markExample(task, "retried")
		status = StatusQueued
	case "delete":
		if task.Status == StatusDone || task.Status == StatusNeedsYou {
			return errors.New("accept or discard it first")
		}
		dropBranch(ctx, repo, s.worktree(task), task.Branch)
		s.unpush(ctx, repo, task)
		s.markExample(task, "removed")
		s.mu.Lock()
		for i, x := range s.st.Tasks {
			if x.ID == id {
				s.st.Tasks = append(s.st.Tasks[:i], s.st.Tasks[i+1:]...)
				break
			}
		}
		s.saveLocked()
		s.mu.Unlock()
		return nil
	default:
		return errors.New("unknown action")
	}
	s.mu.Lock()
	if t := s.find(id); t != nil {
		t.Status = status
		if status == StatusQueued {
			t.Branch, t.Base, t.BaseCommit, t.Verdict, t.Notes, t.Error, t.Rounds, t.Changed = "", "", "", "", "", "", []Round{}, 0
			t.Pushed, t.Shown = false, nil
		}
	}
	s.saveLocked()
	s.mu.Unlock()
	s.poke()
	return nil
}

// cancelled: stop the running task for good (not back to the queue).
var cancelledIDs sync.Map

func (s *Service) cancelled(id string) {
	if s.running == id && s.cancel != nil {
		cancelledIDs.Store(id, true)
		s.cancel()
	}
}

// Diff is the task's branch against where it started.
func (s *Service) Diff(ctx context.Context, id string) (string, error) {
	s.mu.Lock()
	t := s.find(id)
	var task Task
	if t != nil {
		task = *t
	}
	s.mu.Unlock()
	if t == nil {
		return "", errNoTask
	}
	if task.Branch == "" || task.BaseCommit == "" {
		return "", nil
	}
	repo, ok := s.projects.Path(task.Project)
	if !ok {
		return "", errors.New("that project isn't listed any more")
	}
	out, err := git(ctx, repo, "diff", "--no-color", task.BaseCommit+"..."+task.Branch)
	if err != nil {
		return "", errors.New("the branch is gone")
	}
	if len(out) > 2<<20 {
		out = out[:2<<20] + "\n… (cut at 2 MB)\n"
	}
	return out, nil
}

// Log is the end of the task's log.
func (s *Service) Log(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", errNoTask
	}
	b, err := os.ReadFile(filepath.Join(s.set.LogDir, id+".log"))
	if err != nil {
		return "", nil
	}
	if len(b) > 64<<10 {
		b = b[len(b)-64<<10:]
	}
	return string(b), nil
}

var validID = regexp.MustCompile(`^[0-9]{4}-[0-9]{6}(-[0-9]+)?$`)

// saveLocked writes the queue (temp file then rename). Caller holds mu.
func (s *Service) saveLocked() {
	b, err := json.MarshalIndent(s.st, "", " ")
	if err != nil {
		return
	}
	tmp := s.set.StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		if err := os.Rename(tmp, s.set.StatePath); err != nil {
			s.logf("away: saving the queue: %v", err)
		}
	}
}

func (s *Service) update(id string, f func(t *Task)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.find(id); t != nil {
		f(t)
		s.saveLocked()
	}
}

func (s *Service) worktree(t Task) string {
	return filepath.Join(s.set.Worktrees, fmt.Sprintf("%s-%s", safeName(t.Project), t.ID))
}

func safeName(s string) string {
	return regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(s, "-")
}

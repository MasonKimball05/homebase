package away

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

type fakeProjects map[string]string

func (f fakeProjects) Path(id string) (string, bool) { p, ok := f[id]; return p, ok }

// fakeSteps: the "model" writes a file; the "reviewer" answers from a script.
type fakeSteps struct {
	mu            sync.Mutex
	reviews       []string // replies to give, in order
	fix           string
	qwen          func(dir, task string) error
	prompts       []string
	tests         []string
	claudePrompts []string
	block         chan struct{} // if set, Qwen waits here (or for cancel)
	// testRuns: what Tests was asked to run; testFails: how many runs fail first.
	testRuns  []string
	testFails int
}

func (f *fakeSteps) Qwen(ctx context.Context, dir, task, tests, transcript string, log *os.File) error {
	// What Loom's runner writes: the conversation, as items.
	os.WriteFile(transcript, []byte(`{"model":"fake","ms":5,"items":[{"kind":"user","text":`+strconv.Quote(task)+`},{"kind":"tool","name":"read_file","args":{"path":"README.md"},"status":"done","result":"hi"},{"kind":"assistant","text":"Done.","thinking":"hmm"}]}`), 0o644)
	f.mu.Lock()
	f.prompts = append(f.prompts, task)
	f.tests = append(f.tests, tests)
	n := len(f.prompts)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.qwen != nil {
		return f.qwen(dir, task)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "QWEN.md")); err != nil || string(b) != "local rules\n" {
		return fmt.Errorf("the local QWEN.md wasn't copied into the worktree: %v", err)
	}
	return os.WriteFile(filepath.Join(dir, "out.txt"), []byte(fmt.Sprintf("attempt %d\n", n)), 0o644)
}

func (f *fakeSteps) Tests(ctx context.Context, dir, command string, log *os.File) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.testRuns = append(f.testRuns, command)
	if f.testFails > 0 {
		f.testFails--
		return false, "FAIL: test_thing\nAssertionError: 1 != 2"
	}
	return true, "OK"
}

func (f *fakeSteps) Claude(ctx context.Context, dir, prompt string, edit bool, log *os.File) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claudePrompts = append(f.claudePrompts, prompt)
	if edit {
		os.WriteFile(filepath.Join(dir, "out.txt"), []byte("fixed by claude\n"), 0o644)
		return f.fix, nil
	}
	r := f.reviews[0]
	f.reviews = f.reviews[1:]
	return r, nil
}

func setup(t *testing.T, steps Steps) (*Service, string, []string) {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "proj")
	os.MkdirAll(filepath.Join(repo, "node_modules", "dep"), 0o755)
	run(t, base, "init", "-q", "-b", "main", repo)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".env"), []byte("SECRET=1\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "node_modules", "dep", "index.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(repo, "QWEN.md"), []byte("local rules\n"), 0o644) // kept out of git, like Parliament-Dev's
	os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("tracked rules\n"), 0o644)
	run(t, repo, "add", "README.md", ".gitignore", "AGENTS.md")
	run(t, repo, "commit", "-q", "-m", "start")
	var notes []string
	var mu sync.Mutex
	s := New(Settings{Worktrees: filepath.Join(base, "wt"), StatePath: filepath.Join(base, "away.json"), IdlePath: filepath.Join(base, "idle.json"), LogDir: filepath.Join(base, "logs"), Rounds: 2, Tests: map[string]string{"proj": "npm test"}, ExamplesDir: filepath.Join(base, "examples")},
		fakeProjects{"proj": repo}, steps, func(title, body string) { mu.Lock(); notes = append(notes, title); mu.Unlock() }, t.Logf)
	return s, repo, notes
}

func waitFor(t *testing.T, s *Service, id string, done func(*Task) bool) Task {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s.step(context.Background())
		for _, x := range s.View().Tasks {
			if x.ID == id && done(x) {
				return *x
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s never got there: %+v", id, s.View().Tasks)
	return Task{}
}

func finished(x *Task) bool {
	return x.Status != StatusQueued && x.Status != StatusRunning
}

func TestPassesReviewAndWaitsForYou(t *testing.T) {
	steps := &fakeSteps{reviews: []string{"Looks right.\n{\"verdict\": \"pass\", \"notes\": \"Adds out.txt.\"}"}}
	s, repo, _ := setup(t, steps)
	task, err := s.Add("proj", "write out.txt")
	if err != nil {
		t.Fatal(err)
	}
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	if len(steps.tests) != 1 || steps.tests[0] != "npm test" {
		t.Fatalf("the project's test command didn't reach the model: %q", steps.tests)
	}
	if got.Status != StatusDone || got.Verdict != "pass" || got.Changed != 1 || got.Base != "main" {
		t.Fatalf("%+v", got)
	}
	// Your checkout is untouched; the work is on the branch, without the
	// shared node_modules or the copied .env.
	if _, err := os.Stat(filepath.Join(repo, "out.txt")); err == nil {
		t.Fatal("wrote into the checkout")
	}
	files := run(t, repo, "ls-tree", "-r", "--name-only", got.Branch)
	if !strings.Contains(files, "out.txt") || strings.Contains(files, "node_modules") || strings.Contains(files, ".env") || strings.Contains(files, "QWEN.md") {
		t.Fatalf("branch files: %s", files)
	}
	// The worktree is cleaned up, and the shared node_modules survived that.
	if entries, _ := os.ReadDir(s.set.Worktrees); len(entries) != 0 {
		t.Fatalf("worktree left: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(repo, "node_modules", "dep", "index.js")); err != nil {
		t.Fatal("removing the worktree deleted the real node_modules")
	}
	diff, _ := s.Diff(context.Background(), task.ID)
	if !strings.Contains(diff, "+attempt 1") {
		t.Fatalf("diff: %s", diff)
	}
	if err := s.Act(context.Background(), task.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "out.txt")); string(b) != "attempt 1\n" {
		t.Fatalf("not merged: %q", b)
	}
	if strings.Contains(run(t, repo, "branch"), "away/") {
		t.Fatal("branch kept after accepting")
	}
}

// 10-19: Claude's fix reply ended without a verdict line, and the round
// showed "revise" as if the work went back again.
func TestAFixWithoutAVerdictLineIsFixed(t *testing.T) {
	steps := &fakeSteps{
		reviews: []string{`{"verdict": "revise", "notes": "1. split it"}`, `{"verdict": "revise", "notes": "2. split it"}`},
		fix:     "The tests are finished: 5 tests, all pass.",
	}
	s, _, _ := setup(t, steps)
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	last := got.Rounds[len(got.Rounds)-1]
	if last.Who != "claude-fix" || last.Verdict != "fixed" || got.Verdict != "fixed" || got.Status != StatusDone {
		t.Fatalf("%+v / %+v", last, got)
	}
}

func TestRevisesThenClaudeFixes(t *testing.T) {
	steps := &fakeSteps{
		reviews: []string{`{"verdict": "revise", "notes": "1. say hello"}`, `{"verdict": "revise", "notes": "2. still wrong"}`},
		fix:     `Done. {"verdict": "fixed", "notes": "Rewrote out.txt."}`,
	}
	s, repo, _ := setup(t, steps)
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	if got.Status != StatusDone || got.Verdict != "fixed" || got.Notes != "Rewrote out.txt." {
		t.Fatalf("%+v", got)
	}
	if len(steps.prompts) != 2 || !strings.Contains(steps.prompts[1], "1. say hello") {
		t.Fatalf("round 2 didn't get the notes: %q", steps.prompts)
	}
	who := []string{}
	for _, r := range got.Rounds {
		who = append(who, r.Who)
	}
	if strings.Join(who, ",") != "qwen,review,qwen,review,claude-fix" {
		t.Fatalf("rounds: %v", who)
	}
	if out := run(t, repo, "show", got.Branch+":out.txt"); out != "fixed by claude\n" {
		t.Fatalf("branch has %q", out)
	}
	if n := strings.Count(run(t, repo, "log", "--oneline", "main.."+got.Branch), "\n"); n != 3 {
		t.Fatalf("%d commits, want 3", n)
	}
}

func TestFailNeedsYou(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict": "fail", "notes": "There is no such page."}`}}
	s, _, _ := setup(t, steps)
	task, _ := s.Add("proj", "fix the about page")
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	if got.Status != StatusNeedsYou || got.Notes != "There is no such page." {
		t.Fatalf("%+v", got)
	}
}

func TestIdleModeWaitsAndStopsWhenYouComeBack(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}, block: make(chan struct{})}
	s, repo, _ := setup(t, steps)
	now := time.Now()
	s.now = func() time.Time { return now }
	report := func(idle time.Duration) {
		b, _ := json.Marshal(IdleReport{LastInput: now.Add(-idle).Unix(), Written: now.Unix()})
		os.WriteFile(s.set.IdlePath, b, 0o644)
	}
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeIdle)

	report(5 * time.Minute) // not idle long enough
	s.step(context.Background())
	if s.View().Running != "" {
		t.Fatal("started before 20 minutes")
	}
	os.Remove(s.set.IdlePath) // no reporter: never starts
	s.step(context.Background())
	if s.View().Running != "" {
		t.Fatal("started without knowing the idle time")
	}
	report(25 * time.Minute)
	waitFor(t, s, task.ID, func(x *Task) bool { return x.Status == StatusRunning && x.Branch != "" })

	report(10 * time.Second) // you're back
	got := waitFor(t, s, task.ID, func(x *Task) bool { return x.Status == StatusQueued })
	if got.Branch != "" {
		t.Fatalf("%+v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(run(t, repo, "branch"), "away/") {
		t.Fatal("the paused attempt's branch was left behind")
	}

	// Away again: it starts over and finishes.
	close(steps.block)
	report(30 * time.Minute)
	if got := waitFor(t, s, task.ID, finished); got.Status != StatusDone {
		t.Fatalf("%+v", got)
	}
}

func TestCancelAndDiscard(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}, block: make(chan struct{})}
	s, repo, _ := setup(t, steps)
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	waitFor(t, s, task.ID, func(x *Task) bool { return x.Status == StatusRunning && x.Branch != "" })
	if err := s.Act(context.Background(), task.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if got := waitFor(t, s, task.ID, finished); got.Status != StatusCancelled {
		t.Fatalf("%+v", got)
	}
	close(steps.block)
	if err := s.Act(context.Background(), task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	got := waitFor(t, s, task.ID, finished)
	if err := s.Act(context.Background(), got.ID, "discard"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(run(t, repo, "branch"), "away/") {
		t.Fatalf("discard kept the branch\ntask: %+v\nbranches: %s\nworktrees: %s\nlog: %s", got, run(t, repo, "branch", "-v"), run(t, repo, "worktree", "list"), func() string { l, _ := s.Log(task.ID); return l }())
	}
}

func TestAcceptRefusesADirtyCheckout(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}}
	s, repo, _ := setup(t, steps)
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	waitFor(t, s, task.ID, finished)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("mine\n"), 0o644)
	if err := s.Act(context.Background(), task.ID, "accept"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("accepted into a dirty checkout: %v", err)
	}
	run(t, repo, "checkout", "-q", "README.md")
	run(t, repo, "checkout", "-q", "-b", "other")
	if err := s.Act(context.Background(), task.ID, "accept"); err == nil || !strings.Contains(err.Error(), "switch it back") {
		t.Fatalf("accepted into another branch: %v", err)
	}
}

func TestAPI(t *testing.T) {
	steps := &fakeSteps{reviews: []string{}}
	s, _, _ := setup(t, steps)
	mux := http.NewServeMux()
	s.Register(mux, func(r *http.Request) bool { return r.Header.Get("X-Homebase") == "1" })
	do := func(method, path, body string, csrf bool) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if csrf {
			req.Header.Set("X-Homebase", "1")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, _ := do("POST", "/api/away/tasks", `{"project":"proj","task":"x"}`, false); code != http.StatusForbidden {
		t.Fatalf("no CSRF header: %d", code)
	}
	if code, body := do("POST", "/api/away/tasks", `{"project":"nope","task":"x"}`, true); code != http.StatusBadRequest {
		t.Fatalf("unknown project: %d %s", code, body)
	}
	if code, body := do("POST", "/api/away/tasks", `{"project":"proj","task":"  "}`, true); code != http.StatusBadRequest {
		t.Fatalf("empty task: %d %s", code, body)
	}
	code, body := do("POST", "/api/away/tasks", `{"project":"proj","task":"write a test"}`, true)
	if code != 200 || !strings.Contains(body, `"status":"queued"`) {
		t.Fatalf("add: %d %s", code, body)
	}
	if code, _ := do("POST", "/api/away/mode", `{"mode":"sometimes"}`, true); code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", code)
	}
	_, body = do("GET", "/api/away", "", false)
	if !strings.Contains(body, `"mode":"off"`) || !strings.Contains(body, `"idleSeconds":null`) {
		t.Fatalf("view: %s", body)
	}
	if code, _ := do("GET", "/api/away/tasks/..%2F..%2Fetc/log", "", false); code == 200 {
		t.Fatal("log path escaped")
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct{ in, verdict, notes string }{
		{"blah\n{\"verdict\": \"pass\", \"notes\": \"ok\"}", "pass", "ok"},
		{"{\"verdict\":\"revise\",\"notes\":\"a\"} then {\"verdict\":\"pass\",\"notes\":\"b\"}", "pass", "b"},
		{"no json here", "revise", "no json here"},
		{"{\"verdict\":\"maybe\"}", "revise", "{\"verdict\":\"maybe\"}"},
	}
	for _, c := range cases {
		v, n := parseVerdict(c.in)
		if v != c.verdict || n != c.notes {
			t.Errorf("%q: %q %q", c.in, v, n)
		}
	}
}

func TestSkipCommit(t *testing.T) {
	for _, p := range []string{"node_modules/x.js", ".venv/bin/python", ".env", "config/.env.local", "prod.env", "key.pem", ".qwen-run.json"} {
		if !skipCommit(p) {
			t.Errorf("would commit %s", p)
		}
	}
	for _, p := range []string{"src/app.ts", ".env.example", "environment.py", "docs/env.md"} {
		if skipCommit(p) {
			t.Errorf("would skip %s", p)
		}
	}
}

func TestARestartMidTaskKeepsWorkThatWasCommitted(t *testing.T) {
	// A first attempt gets as far as a commit, then homebase "restarts"
	// (a new Service over the same state file and repo).
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}, block: make(chan struct{})}
	s, repo, _ := setup(t, steps)
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	running := waitFor(t, s, task.ID, func(x *Task) bool { return strings.HasPrefix(x.Stage, "local model") })
	wt := s.worktree(running)
	os.WriteFile(filepath.Join(wt, "out.txt"), []byte("work\n"), 0o644)
	run(t, wt, "add", "out.txt")
	run(t, wt, "commit", "-q", "-m", "round 1")
	s.mu.Lock()
	s.saveLocked() // the state file as homebase left it, mid-task
	s.mu.Unlock()

	s2 := New(s.set, fakeProjects{"proj": repo}, &fakeSteps{}, nil, t.Logf)
	got := s2.View().Tasks[0]
	if got.Status != StatusFailed || !strings.Contains(got.Error, "restarted") || got.Branch != running.Branch {
		t.Fatalf("%+v", got)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("the stale worktree was left")
	}
	// You can still take the work.
	if err := s2.Act(context.Background(), task.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "out.txt")); string(b) != "work\n" {
		t.Fatalf("not merged: %q", b)
	}
	close(steps.block)
}

func TestARestartBeforeAnyCommitStartsOver(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}, block: make(chan struct{})}
	s, repo, _ := setup(t, steps)
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	running := waitFor(t, s, task.ID, func(x *Task) bool { return strings.HasPrefix(x.Stage, "local model") })
	s.mu.Lock()
	s.saveLocked()
	s.mu.Unlock()
	s2 := New(s.set, fakeProjects{"proj": repo}, &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}}, nil, t.Logf)
	if got := s2.View().Tasks[0]; got.Status != StatusQueued || got.Branch != "" {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(run(t, repo, "branch"), running.Branch) {
		t.Fatal("the empty branch was left, so the next attempt couldn't make it")
	}
	close(steps.block)
	// And the next attempt works (the old failure: "couldn't make a worktree").
	if got := waitFor(t, s2, task.ID, finished); got.Status != StatusDone {
		t.Fatalf("%+v", got)
	}
}

// A broken model isn't the task's fault: the task goes back in the queue,
// Away switches itself off (once, with the reason), and nothing is left
// behind or kept as an example.
// clock: a fake s.now the test moves on, safe to read from the runner.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// settle steps the queue until nothing is running and the model has been
// asked n times in all.
func settle(t *testing.T, s *Service, steps *fakeSteps, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s.step(context.Background())
		steps.mu.Lock()
		asked := len(steps.prompts)
		steps.mu.Unlock()
		if asked >= n && s.View().Running == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the model was never asked %d times: %+v", n, s.View())
}

// A model that still doesn't answer after about half an hour of tries
// switches Away off, once, with one notification.
func TestABrokenModelSwitchesAwayOff(t *testing.T) {
	for name, err := range map[string]error{
		"model error":   &ModelError{Msg: "Ollama: HTTP 500 llama-server binary not found"},
		"runner exited": fmt.Errorf("ollama is down"),
	} {
		t.Run(name, func(t *testing.T) {
			steps := &fakeSteps{qwen: func(dir, task string) error { return err }}
			s, repo, _ := setup(t, steps)
			c := &clock{t: time.Date(2026, 10, 21, 10, 12, 0, 0, time.Local)}
			s.now = c.now
			var mu sync.Mutex
			var notes []string
			s.notify = func(title, body string) { mu.Lock(); notes = append(notes, title+": "+body); mu.Unlock() }
			first, _ := s.Add("proj", "first")
			second, _ := s.Add("proj", "second")
			s.SetMode(ModeOn)
			for try := 1; try < modelTries; try++ {
				settle(t, s, steps, try)
				if v := s.View(); v.Mode != ModeOn || v.Waiting == nil || v.Waiting.Tries != try {
					t.Fatalf("try %d: %q %+v", try, v.Mode, v.Waiting)
				}
				c.add(modelWait)
			}
			settle(t, s, steps, modelTries)
			for range 5 { // a second task would (wrongly) start here
				s.step(context.Background())
				time.Sleep(20 * time.Millisecond)
			}
			v := s.View()
			if v.Mode != ModeOff || !strings.Contains(v.Stopped, "the local model couldn't run") || !strings.Contains(v.Stopped, "4 tries over 30 min") || v.Waiting != nil {
				t.Fatalf("Away didn't switch itself off: %q %q %+v", v.Mode, v.Stopped, v.Waiting)
			}
			for _, x := range v.Tasks {
				if (x.ID == first.ID || x.ID == second.ID) && (x.Status != StatusQueued || x.Branch != "" || len(x.Rounds) != 0) {
					t.Fatalf("%+v", x)
				}
			}
			if len(steps.prompts) != modelTries || steps.prompts[modelTries-1] != "first" {
				t.Fatalf("kept going after the model failed: %q", steps.prompts)
			}
			mu.Lock()
			if len(notes) != 1 || !strings.Contains(notes[0], "switched off") {
				t.Fatalf("notifications: %q", notes)
			}
			mu.Unlock()
			if strings.Contains(run(t, repo, "branch"), "away/") {
				t.Fatal("an empty branch was left behind")
			}
			if list, _ := s.Examples(""); len(list) != 0 {
				t.Fatalf("the model never ran, so there's nothing to keep: %+v", list)
			}
			s.SetMode(ModeOn) // you fixed it and switched it back on
			if s.View().Stopped != "" {
				t.Fatal("the reason outlived switching it back on")
			}
		})
	}
}

// 10-21: the PC was switched on and Away started a task while Ollama was
// still installing an update. Away waits (mode as it was), tries again, and
// carries on.
func TestAModelThatDoesntAnswerPausesAway(t *testing.T) {
	var down sync.Mutex
	broken := true
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}}
	steps.qwen = func(dir, task string) error {
		down.Lock()
		defer down.Unlock()
		if broken {
			return &ModelError{Msg: "fetch failed"}
		}
		return os.WriteFile(filepath.Join(dir, "out.txt"), []byte("done\n"), 0o644)
	}
	s, _, _ := setup(t, steps)
	c := &clock{t: time.Date(2026, 10, 21, 10, 12, 0, 0, time.Local)}
	s.now = c.now
	s.generate = func(ctx context.Context, prompt string) (string, error) {
		down.Lock()
		defer down.Unlock()
		if broken {
			return "", fmt.Errorf("fetch failed")
		}
		return "OK", nil
	}
	notified := 0
	s.notify = func(title, body string) { notified++ }
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeIdle)
	s.SetMode(ModeOn)
	settle(t, s, steps, 1)

	v := s.View()
	w := v.Waiting
	if v.Mode != ModeOn || v.Stopped != "" || w == nil || w.Tries != 1 || w.Max != modelTries || !w.Retry.Equal(c.now().Add(modelWait)) || !strings.Contains(w.Reason, "fetch failed") {
		t.Fatalf("%q %q %+v", v.Mode, v.Stopped, w)
	}
	if v.Tasks[0].Status != StatusQueued || notified != 0 {
		t.Fatalf("%+v, %d notifications", v.Tasks[0], notified)
	}
	c.add(modelWait - time.Second)
	for range 5 { // too soon: nothing starts
		s.step(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	if len(steps.prompts) != 1 {
		t.Fatalf("tried again before the wait was up: %d runs", len(steps.prompts))
	}

	down.Lock()
	broken = false // Ollama finished updating
	down.Unlock()
	c.add(time.Second)
	if got := waitFor(t, s, task.ID, finished); got.Status != StatusDone {
		t.Fatalf("%+v", got)
	}
	if v := s.View(); v.Mode != ModeOn || v.Waiting != nil || v.Stopped != "" {
		t.Fatalf("%q %q %+v", v.Mode, v.Stopped, v.Waiting)
	}
}

// Setting the mode yourself during the wait means "try now".
func TestSettingTheModeEndsTheWait(t *testing.T) {
	steps := &fakeSteps{qwen: func(dir, task string) error { return &ModelError{Msg: "fetch failed"} }}
	s, _, _ := setup(t, steps)
	c := &clock{t: time.Date(2026, 10, 21, 10, 12, 0, 0, time.Local)}
	s.now = c.now
	s.Add("proj", "anything")
	s.SetMode(ModeOn)
	settle(t, s, steps, 1)
	if s.View().Waiting == nil {
		t.Fatal("Away isn't waiting")
	}
	s.SetMode(ModeOn)
	if s.View().Waiting != nil {
		t.Fatal("still waiting")
	}
	settle(t, s, steps, 2)
	if w := s.View().Waiting; w == nil || w.Tries != 1 {
		t.Fatalf("the count didn't start over: %+v", w)
	}
}

func TestPathGoesFirst(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	env := CLI{Path: []string{"/opt/tools"}}.env("X=1")
	var path string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path = kv
		}
	}
	if path != "PATH=/opt/tools"+string(os.PathListSeparator)+"/usr/bin" || env[len(env)-1] != "X=1" {
		t.Fatalf("%q", path)
	}
}

func TestTranscriptError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.json")
	os.WriteFile(p, []byte(`{"items":[{"kind":"user","text":"x"},{"kind":"error","text":"Ollama: HTTP 500 boom"}]}`), 0o644)
	if got := transcriptError(p); got != "Ollama: HTTP 500 boom" {
		t.Fatalf("%q", got)
	}
	os.WriteFile(p, []byte(`{"items":[{"kind":"error","text":"retrying"},{"kind":"assistant","text":"done"}]}`), 0o644)
	if got := transcriptError(p); got != "" {
		t.Fatalf("an earlier error counted: %q", got)
	}
	if transcriptError(filepath.Join(dir, "missing.json")) != "" {
		t.Fatal("missing file counted as an error")
	}
}

func TestPushedBranchesAreRemovedFromOrigin(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`, `{"verdict":"pass","notes":"ok"}`}}
	s, repo, _ := setup(t, steps)
	origin := filepath.Join(t.TempDir(), "origin.git")
	run(t, repo, "init", "-q", "--bare", origin)
	run(t, repo, "remote", "add", "origin", origin)
	run(t, repo, "push", "-q", "origin", "main")
	s.set.Push = []string{"proj"}
	s.SetMode(ModeOn)

	remote := func() string { return run(t, origin, "branch", "--list", "away/*") }
	for _, action := range []string{"accept", "discard"} {
		task, _ := s.Add("proj", "write out.txt")
		got := waitFor(t, s, task.ID, func(x *Task) bool { return finished(x) && x.Pushed })
		if !strings.Contains(remote(), got.Branch) {
			t.Fatalf("%s: not pushed: %q", action, remote())
		}
		if err := s.Act(context.Background(), task.ID, action); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(remote(), got.Branch) {
			t.Fatalf("%s left %s on origin", action, got.Branch)
		}
	}
	// main was never pushed by Away (the accepted merge stays local).
	if run(t, origin, "rev-parse", "main") == run(t, repo, "rev-parse", "main") {
		t.Fatal("Away pushed main")
	}
}

func readExample(t *testing.T, s *Service, id string) Example {
	t.Helper()
	list, err := s.Examples("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if e.ID == id {
			b, err := s.ExampleJSON(e.Name)
			if err != nil {
				t.Fatal(err)
			}
			var ex Example
			if err := json.Unmarshal(b, &ex); err != nil {
				t.Fatal(err)
			}
			return ex
		}
	}
	t.Fatalf("no example for %s in %+v", id, list)
	return Example{}
}

func TestAFinishedAttemptIsKeptAsAnExample(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"Adds out.txt."}`}}
	s, _, _ := setup(t, steps)
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	waitFor(t, s, task.ID, finished)

	ex := readExample(t, s, task.ID)
	if ex.Author != "qwen" || ex.Status != StatusDone || ex.Verdict != "pass" || ex.Outcome != "" || ex.Task != "write out.txt" {
		t.Fatalf("%+v", ex)
	}
	if len(ex.Files) != 1 || ex.Files[0] != "out.txt" || !strings.Contains(ex.FinalDiff, "+attempt 1") {
		t.Fatalf("files %v, diff %q", ex.Files, ex.FinalDiff)
	}
	if len(ex.Rounds) != 2 || ex.Rounds[0].Who != "qwen" || !strings.Contains(ex.Rounds[0].Diff, "+attempt 1") || ex.Rounds[1].Notes != "Adds out.txt." {
		t.Fatalf("rounds %+v", ex.Rounds)
	}
	var compact bytes.Buffer
	json.Compact(&compact, ex.Rounds[0].Transcript)
	tr := compact.String()
	if !strings.Contains(tr, `"name":"read_file"`) || !strings.Contains(tr, "Done.") || strings.Contains(tr, "hmm") {
		t.Fatalf("transcript (thinking should be dropped): %s", tr)
	}

	if err := s.Act(context.Background(), task.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	if ex := readExample(t, s, task.ID); ex.Outcome != "accepted" || ex.OutcomeAt.IsZero() {
		t.Fatalf("outcome not recorded: %+v", ex)
	}
}

func TestClaudesFixIsToldApartFromQwensWork(t *testing.T) {
	steps := &fakeSteps{
		reviews: []string{`{"verdict":"revise","notes":"1. fix it"}`, `{"verdict":"revise","notes":"2. still"}`},
		fix:     `{"verdict":"fixed","notes":"Rewrote it."}`,
	}
	s, _, _ := setup(t, steps)
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	waitFor(t, s, task.ID, finished)
	ex := readExample(t, s, task.ID)
	if ex.Author != "qwen+claude" {
		t.Fatalf("author %q", ex.Author)
	}
	last := ex.Rounds[len(ex.Rounds)-1]
	if last.Who != "claude-fix" || !strings.Contains(last.Diff, "+fixed by claude") || strings.Contains(ex.Rounds[0].Diff, "claude") {
		t.Fatalf("rounds %+v", ex.Rounds)
	}
	if ex.Rounds[0].Transcript == nil || ex.Rounds[2].Transcript == nil || ex.Rounds[1].Transcript != nil {
		t.Fatal("transcripts belong to the qwen rounds only")
	}
}

func TestEveryAttemptGetsItsOwnExample(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`, `{"verdict":"pass","notes":"ok"}`}}
	s, _, _ := setup(t, steps)
	now := time.Now()
	s.now = func() time.Time { return now }
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	waitFor(t, s, task.ID, finished)
	if err := s.Act(context.Background(), task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	waitFor(t, s, task.ID, func(x *Task) bool { return finished(x) && x.Started.Equal(now) })
	list, _ := s.Examples("proj")
	if len(list) != 2 || list[1].Outcome != "retried" || list[0].Outcome != "" {
		t.Fatalf("%+v", list)
	}
	if other, _ := s.Examples("elsewhere"); len(other) != 0 {
		t.Fatalf("filter: %+v", other)
	}
}

func TestExampleNamesStayInTheirFolder(t *testing.T) {
	s, _, _ := setup(t, &fakeSteps{})
	for _, bad := range []string{"../away.json", "proj/../../x.json", "proj/1015-120000@20261015-120000.json/../../../etc", "/etc/passwd"} {
		if _, err := s.ExampleJSON(bad); err == nil {
			t.Errorf("read %q", bad)
		}
	}
}

// wordEmbedder: a vector of word counts over a small vocabulary, so tasks
// that share words are "similar" and ones that don't aren't.
func wordEmbedder(calls *int) Embedder {
	vocab := []string{"tests", "house", "kai", "check", "csv", "export", "readme", "out.txt", "write"}
	return func(ctx context.Context, texts []string) ([][]float64, error) {
		*calls++
		out := make([][]float64, len(texts))
		for i, t := range texts {
			v := make([]float64, len(vocab))
			for j, w := range vocab {
				v[j] = float64(strings.Count(strings.ToLower(t), w))
			}
			out[i] = v
		}
		return out, nil
	}
}

func TestSimilarAcceptedWorkIsShownToTheModel(t *testing.T) {
	steps := &fakeSteps{
		reviews: []string{`{"verdict":"revise","notes":"1. put the file in docs/"}`, `{"verdict":"pass","notes":"Writes out.txt."}`, `{"verdict":"pass","notes":"ok"}`, `{"verdict":"pass","notes":"ok"}`, `{"verdict":"pass","notes":"ok"}`},
	}
	s, _, _ := setup(t, steps)
	calls := 0
	s.embed = wordEmbedder(&calls)
	s.set.EmbedModel = "fake"
	s.SetMode(ModeOn)

	first, _ := s.Add("proj", "write out.txt with tests")
	waitFor(t, s, first.ID, finished)
	if err := s.Act(context.Background(), first.ID, "accept"); err != nil {
		t.Fatal(err)
	}

	// Similar: it gets the accepted diff, the reviewer's note and its first attempt's lesson.
	second, _ := s.Add("proj", "write out.txt again, with tests")
	got := waitFor(t, s, second.ID, finished)
	prompt := steps.prompts[len(steps.prompts)-1]
	for _, want := range []string{"How similar tasks went before", "Accepted: write out.txt with tests", "+attempt", "Writes out.txt.", "put the file in docs/"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if len(got.Shown) != 1 {
		t.Fatalf("shown: %v", got.Shown)
	}
	if ex := readExample(t, s, second.ID); len(ex.Shown) != 1 || ex.Shown[0] != got.Shown[0] {
		t.Fatalf("the example doesn't record what was shown: %v", ex.Shown)
	}

	// Unrelated: nothing is shown.
	third, _ := s.Add("proj", "csv export")
	got = waitFor(t, s, third.ID, finished)
	if len(got.Shown) != 0 || strings.Contains(steps.prompts[len(steps.prompts)-1], "How similar") {
		t.Fatalf("shown for an unrelated task: %v", got.Shown)
	}
	// Each example's vector is embedded once, then cached.
	before := calls
	fourth, _ := s.Add("proj", "write out.txt, tests too")
	waitFor(t, s, fourth.ID, finished)
	if calls != before+1 {
		t.Fatalf("embed calls %d -> %d (cache not used?)", before, calls)
	}
}

func TestRecallNeverStopsATask(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`, `{"verdict":"pass","notes":"ok"}`}}
	s, _, _ := setup(t, steps)
	s.set.EmbedModel = "fake"
	s.SetMode(ModeOn)
	first, _ := s.Add("proj", "write out.txt")
	waitFor(t, s, first.ID, finished)
	s.Act(context.Background(), first.ID, "accept")
	s.embed = func(ctx context.Context, texts []string) ([][]float64, error) {
		return nil, fmt.Errorf("ollama is down")
	}
	second, _ := s.Add("proj", "write out.txt")
	if got := waitFor(t, s, second.ID, finished); got.Status != StatusDone || len(got.Shown) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestCosine(t *testing.T) {
	if c := cosine([]float64{1, 0}, []float64{1, 0}); math.Abs(c-1) > 1e-9 {
		t.Fatal(c)
	}
	if c := cosine([]float64{1, 0}, []float64{0, 1}); c != 0 {
		t.Fatal(c)
	}
	if cosine(nil, []float64{1}) != 0 || cosine([]float64{0, 0}, []float64{1, 1}) != 0 {
		t.Fatal("degenerate vectors")
	}
}

func TestStatsSayHowAnAttemptWent(t *testing.T) {
	steps := &fakeSteps{
		reviews: []string{`{"verdict":"pass","notes":"ok"}`, `{"verdict":"revise","notes":"1. x"}`, `{"verdict":"revise","notes":"2. y"}`},
		fix:     `{"verdict":"fixed","notes":"Done."}`,
	}
	s, _, _ := setup(t, steps)
	s.SetMode(ModeOn)
	easy, _ := s.Add("proj", "write out.txt")
	waitFor(t, s, easy.ID, finished)
	hard, _ := s.Add("proj", "write out.txt properly")
	waitFor(t, s, hard.ID, finished)

	byID := map[string]ExampleSummary{}
	list, _ := s.Examples("proj")
	for _, e := range list {
		byID[e.ID] = e
	}
	e := byID[easy.ID].Stats
	if !e.Reviewed || !e.PassedFirstReview || e.ClaudeFixed || e.QwenRounds != 1 || e.ToolCalls != 1 || e.TestRuns != 0 {
		t.Fatalf("easy: %+v", e)
	}
	h := byID[hard.ID].Stats
	if h.PassedFirstReview || !h.ClaudeFixed || h.QwenRounds != 2 || h.ToolCalls != 2 {
		t.Fatalf("hard: %+v", h)
	}
	// run_tests calls are counted from the transcript.
	tr := json.RawMessage(`{"items":[{"kind":"tool","name":"run_tests"},{"kind":"tool","name":"read_file"},{"kind":"tool","name":"run_tests"},{"kind":"assistant","text":"ok"}]}`)
	if st := statsOf(Example{Rounds: []ExampleRound{{Who: "qwen", Seconds: 60, Transcript: tr}}}); st.TestRuns != 2 || st.ToolCalls != 3 || st.Reviewed {
		t.Fatalf("%+v", st)
	}
}

func TestItsOwnFailingTestsGoBackWithoutAReview(t *testing.T) {
	steps := &fakeSteps{
		reviews:   []string{`{"verdict":"pass","notes":"Good tests."}`},
		testFails: 1,
		qwen: func(dir, task string) error {
			os.MkdirAll(filepath.Join(dir, "tests", "kai"), 0o755)
			return os.WriteFile(filepath.Join(dir, "tests", "kai", "test_x.py"), []byte(task[:10]+"\n"), 0o644)
		},
	}
	s, _, _ := setup(t, steps)
	s.set.Tests = map[string]string{"proj": "python manage.py test --noinput"}
	s.SetMode(ModeOn)
	task, _ := s.Add("proj", "write tests for x")
	got := waitFor(t, s, task.ID, finished)

	// Round 1's tests failed: back to the model with the output, no review spent.
	if len(steps.claudePrompts) != 1 {
		t.Fatalf("reviews: %d, want 1 (only after the tests passed)", len(steps.claudePrompts))
	}
	if steps.testRuns[0] != "python manage.py test --noinput tests.kai.test_x" {
		t.Fatalf("ran %q", steps.testRuns[0])
	}
	if !strings.Contains(steps.prompts[1], "Your tests fail") || !strings.Contains(steps.prompts[1], "AssertionError: 1 != 2") {
		t.Fatalf("round 2 prompt: %s", steps.prompts[1])
	}
	// The review that did happen knew the tests had passed.
	if !strings.Contains(steps.claudePrompts[0], "already ran `python manage.py test --noinput tests.kai.test_x`") {
		t.Fatalf("review prompt: %s", steps.claudePrompts[0])
	}
	who := []string{}
	for _, r := range got.Rounds {
		who = append(who, r.Who+":"+r.Verdict)
	}
	if strings.Join(who, ",") != "qwen:,tests:fail,qwen:,tests:pass,review:pass" || got.Status != StatusDone {
		t.Fatalf("rounds %v, %+v", who, got)
	}
}

func TestFailingTestsInTheLastRoundGoStraightToClaudesFix(t *testing.T) {
	steps := &fakeSteps{
		fix:       `{"verdict":"fixed","notes":"Fixed the tests."}`,
		testFails: 2,
		qwen: func(dir, task string) error {
			return os.WriteFile(filepath.Join(dir, "test_a.py"), []byte(fmt.Sprint(len(task))+"\n"), 0o644)
		},
	}
	s, _, _ := setup(t, steps)
	s.set.Tests = map[string]string{"proj": "npx vitest run"}
	s.SetMode(ModeOn)
	task, _ := s.Add("proj", "write tests")
	got := waitFor(t, s, task.ID, finished)
	if len(steps.claudePrompts) != 1 || got.Verdict != "fixed" {
		t.Fatalf("claude calls %d, %+v", len(steps.claudePrompts), got)
	}
	if steps.testRuns[0] != "npx vitest run test_a.py" {
		t.Fatalf("paths, not modules, for a non-Python runner: %q", steps.testRuns[0])
	}
}

func TestTestTargets(t *testing.T) {
	files := []string{"src/tests/kai/test_checks_kai.py", "web/a.test.ts"}
	if got := testTargets("python manage.py test --noinput", files); strings.Join(got, " ") != "src.tests.kai.test_checks_kai" {
		t.Fatal(got)
	}
	if got := testTargets("npm test --", files); strings.Join(got, " ") != strings.Join(files, " ") {
		t.Fatal(got)
	}
	// Skirmish (Godot): paths, which scripts/godot-test.ps1 turns into res:// ones.
	if got := testTargets(`powershell -File C:\x\godot-test.ps1`, []string{"game/tests/test_wallet.gd"}); strings.Join(got, " ") != "game/tests/test_wallet.gd" {
		t.Fatal(got)
	}
	for f, want := range map[string]bool{"src/tests/test_x.py": true, "x_test.go": true, "a/b.spec.tsx": true, "game/tests/test_wallet.gd": true,
		"game/tests/_probe_hud.gd": false, "src/testing.py": false, "contest.py": false, "README.md": false} {
		if testFile.MatchString(f) != want {
			t.Errorf("%s: %v", f, !want)
		}
	}
}

func TestEmbeddingsCacheIsNotAnExample(t *testing.T) {
	s, _, _ := setup(t, &fakeSteps{})
	os.MkdirAll(filepath.Join(s.set.ExamplesDir, "proj"), 0o755)
	os.WriteFile(filepath.Join(s.set.ExamplesDir, "proj", ".embeddings.json"), []byte(`{"model":"x","vectors":{}}`), 0o644)
	if list, _ := s.Examples(""); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}

func TestEveryThirdTaskIsHeldOutForComparison(t *testing.T) {
	// The same answer for the same id; about a third of ids.
	held := 0
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("1018-%06d", i)
		if heldOut(id, 3) != heldOut(id, 3) {
			t.Fatal("not stable")
		}
		if heldOut(id, 3) {
			held++
		}
	}
	if held < 70 || held > 130 {
		t.Fatalf("%d of 300 held out", held)
	}
	if heldOut("x", 0) || heldOut("x", -1) {
		t.Fatal("0 or less means never")
	}
	// A held-out task gets no examples, and says so.
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`, `{"verdict":"pass","notes":"ok"}`}}
	s, _, _ := setup(t, steps)
	calls := 0
	s.embed = wordEmbedder(&calls)
	s.set.EmbedModel, s.set.RecallHoldout = "fake", 1 // every task
	s.SetMode(ModeOn)
	first, _ := s.Add("proj", "write out.txt")
	waitFor(t, s, first.ID, finished)
	s.Act(context.Background(), first.ID, "accept")
	second, _ := s.Add("proj", "write out.txt")
	got := waitFor(t, s, second.ID, finished)
	if !got.HeldOut || len(got.Shown) != 0 || calls != 0 || strings.Contains(steps.prompts[len(steps.prompts)-1], "How similar") {
		t.Fatalf("%+v (embed calls %d)", got, calls)
	}
	if ex := readExample(t, s, second.ID); !ex.HeldOut {
		t.Fatal("the example doesn't say it was held out")
	}
}

// 10-20: one garbled tool call isn't a broken model. When the model still
// answers, the round gets one more go, and Away stays on.
func TestOneBrokenAnswerIsRetried(t *testing.T) {
	calls := 0
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}}
	steps.qwen = func(dir, task string) error {
		calls++
		if calls == 1 {
			return &ModelError{Msg: "expected element type <function> but have <parameter>"}
		}
		return os.WriteFile(filepath.Join(dir, "out.txt"), []byte("done\n"), 0o644)
	}
	s, _, _ := setup(t, steps)
	s.generate = func(ctx context.Context, prompt string) (string, error) { return "OK", nil }
	task, _ := s.Add("proj", "write out.txt")
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	if got.Status != StatusDone || got.Verdict != "pass" || calls != 2 {
		t.Fatalf("%d calls, %+v", calls, got)
	}
	if v := s.View(); v.Mode != ModeOn || v.Stopped != "" {
		t.Fatalf("Away switched itself off: %q %q", v.Mode, v.Stopped)
	}
}

// If its answers keep breaking while the model itself answers, that task
// fails and the queue goes on.
func TestAnswersThatKeepBreakingFailOnlyTheTask(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}}
	steps.qwen = func(dir, task string) error {
		if task == "bad" {
			return &ModelError{Msg: "expected element type <function> but have <parameter>"}
		}
		return os.WriteFile(filepath.Join(dir, "out.txt"), []byte("done\n"), 0o644)
	}
	s, _, _ := setup(t, steps)
	s.generate = func(ctx context.Context, prompt string) (string, error) { return "OK", nil }
	bad, _ := s.Add("proj", "bad")
	good, _ := s.Add("proj", "good")
	s.SetMode(ModeOn)
	if got := waitFor(t, s, bad.ID, finished); got.Status != StatusFailed || !strings.Contains(got.Error, "its answer broke off") {
		t.Fatalf("%+v", got)
	}
	if got := waitFor(t, s, good.ID, finished); got.Status != StatusDone {
		t.Fatalf("the queue stopped: %+v", got)
	}
	if v := s.View(); v.Mode != ModeOn {
		t.Fatalf("Away switched itself off: %q", v.Stopped)
	}
}

// A model that sees nothing (no tool calls) still stops Away at once, even
// though it would answer a one-line question.
func TestASetupProblemStillStopsAway(t *testing.T) {
	steps := &fakeSteps{qwen: func(dir, task string) error {
		return &ModelError{Setup: true, Msg: "it answered without using a single tool"}
	}}
	s, _, _ := setup(t, steps)
	asked := false
	s.generate = func(ctx context.Context, prompt string) (string, error) { asked = true; return "OK", nil }
	s.Add("proj", "anything")
	s.SetMode(ModeOn)
	deadline := time.Now().Add(20 * time.Second)
	for s.View().Mode != ModeOff && time.Now().Before(deadline) {
		s.step(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	if v := s.View(); v.Mode != ModeOff || v.Waiting != nil || asked || len(steps.prompts) != 1 {
		t.Fatalf("mode %q, waiting %+v, asked the model: %v, %d runs", v.Mode, v.Waiting, asked, len(steps.prompts))
	}
}

// Godot writes a .uid for every script missing one when it imports a fresh
// worktree. Only the ones for scripts this branch added belong in it.
func TestStrayGodotUIDsAreNotCommitted(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}}
	steps.qwen = func(dir, task string) error {
		os.MkdirAll(filepath.Join(dir, "game", "tests"), 0o755)
		os.WriteFile(filepath.Join(dir, "game", "debug_perk.gd.uid"), []byte("uid://stray\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "game", "tests", "test_new.gd"), []byte("extends SceneTree\n"), 0o644)
		return os.WriteFile(filepath.Join(dir, "game", "tests", "test_new.gd.uid"), []byte("uid://mine\n"), 0o644)
	}
	s, repo, _ := setup(t, steps)
	os.MkdirAll(filepath.Join(repo, "game"), 0o755)
	os.WriteFile(filepath.Join(repo, "game", "debug_perk.gd"), []byte("extends Node\n"), 0o644)
	run(t, repo, "add", "game")
	run(t, repo, "commit", "-q", "-m", "a script without a uid")
	task, _ := s.Add("proj", "Add tests in game/tests/test_new.gd")
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	files := run(t, repo, "diff", "--name-only", "main.."+got.Branch)
	if !strings.Contains(files, "game/tests/test_new.gd.uid") || strings.Contains(files, "debug_perk.gd.uid") {
		t.Fatalf("branch changes:\n%s", files)
	}
}

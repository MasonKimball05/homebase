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
	mu      sync.Mutex
	reviews []string // replies to give, in order
	fix     string
	qwen    func(dir, task string) error
	prompts []string
	tests   []string
	block   chan struct{} // if set, Qwen waits here (or for cancel)
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

func (f *fakeSteps) Claude(ctx context.Context, dir, prompt string, edit bool, log *os.File) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func TestAFailureLeavesNoEmptyBranch(t *testing.T) {
	steps := &fakeSteps{qwen: func(dir, task string) error { return fmt.Errorf("ollama is down") }}
	s, repo, _ := setup(t, steps)
	task, _ := s.Add("proj", "anything")
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	if got.Status != StatusFailed || !strings.Contains(got.Error, "ollama is down") || got.Branch != "" {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(run(t, repo, "branch"), "away/") {
		t.Fatal("an empty branch was left behind")
	}
	if list, _ := s.Examples(""); len(list) != 0 {
		t.Fatalf("the model never ran, so there's nothing to keep: %+v", list)
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

func TestAModelErrorStopsTheTask(t *testing.T) {
	steps := &fakeSteps{qwen: func(dir, task string) error { return &ModelError{"Ollama: HTTP 500 llama-server binary not found"} }}
	s, _, _ := setup(t, steps)
	task, _ := s.Add("proj", "anything")
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	if got.Status != StatusFailed || !strings.Contains(got.Error, "llama-server binary not found") {
		t.Fatalf("%+v", got)
	}
	if len(steps.prompts) != 1 {
		t.Fatalf("kept going after the model failed: %d rounds", len(steps.prompts))
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

package away

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	block   chan struct{} // if set, Qwen waits here (or for cancel)
}

func (f *fakeSteps) Qwen(ctx context.Context, dir, task string, log *os.File) error {
	f.mu.Lock()
	f.prompts = append(f.prompts, task)
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
	s := New(Settings{Worktrees: filepath.Join(base, "wt"), StatePath: filepath.Join(base, "away.json"), IdlePath: filepath.Join(base, "idle.json"), LogDir: filepath.Join(base, "logs"), Rounds: 2},
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
		t.Fatal("discard kept the branch")
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

func TestRunningTaskStartsOverAfterARestart(t *testing.T) {
	dir := t.TempDir()
	st := state{Mode: ModeOn, Tasks: []*Task{{ID: "1015-120000", Status: StatusRunning, Rounds: []Round{}}}}
	b, _ := json.Marshal(st)
	os.WriteFile(filepath.Join(dir, "away.json"), b, 0o600)
	s := New(Settings{StatePath: filepath.Join(dir, "away.json")}, fakeProjects{}, &fakeSteps{}, nil, t.Logf)
	if v := s.View(); v.Tasks[0].Status != StatusQueued || v.Mode != ModeOn {
		t.Fatalf("%+v", v)
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
}

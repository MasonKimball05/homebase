package away

// Refill: when Away is free to work and the queue is empty, it queues work
// for itself, within a daily cap per project (Settings.Autofill). Until
// 10-22 every batch waited for someone to find functions, write the cases
// and check them against the code by hand; this does the same steps:
//
//   - A tests task: a suggested function (suggest.go) nobody has tried.
//     Claude writes the tests in a scratch worktree and lists the cases; Away
//     runs them itself and queues the task only when they pass, one test per
//     case. Claude's file is kept in LogDir/answers as the answer key.
//   - A drill (every third refill): a function whose accepted tests are
//     merged. Away tries its mutants (mutants.py) until one makes those
//     tests fail, and queues "a bug has crept in: find and fix it". The
//     right answer is the code as it was, so the task checks itself.
//
// Nothing here merges anything or touches the project's checkout: the work
// happens in a detached worktree that's removed afterwards.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Drill: a bug Away planted on purpose (run.go plants it in the task's
// worktree before the local model starts, as a commit of its own).
type Drill struct {
	File     string `json:"file"`     // the source file, relative
	Function string `json:"function"` // the function the bug is in
	Mutant   int    `json:"mutant"`   // mutants.py's id for the change
	What     string `json:"what"`     // the change, for the review (not the model)
	Target   string `json:"target"`   // the tests that catch it, as the test command names them
}

const (
	refillRunning = "(refilling the queue)" // s.running while a refill works
	refillPause   = 30 * time.Minute        // after a refill that found nothing to queue
	drillEvery    = 3                       // every third automatic task is a drill
	drillTries    = 5                       // mutants tried per function
)

var newTestFile = regexp.MustCompile(`in a new file ([\w./-]+\.py)\b`)

// refillProject: a project that may get an automatic task now, or "".
// Called with s.mu held.
func (s *Service) refillProject() string {
	if s.now().Before(s.refillAfter) {
		return ""
	}
	day := s.now().Format("2006-01-02")
	for _, p := range slices.Sorted(maps.Keys(s.set.Autofill)) {
		limit := s.set.Autofill[p]
		if limit <= 0 || s.set.Tests[p] == "" || verbose(s.set.Tests[p]) == "" {
			continue // only Python test commands Away can read test by test
		}
		if _, ok := s.projects.Path(p); !ok {
			continue
		}
		n := 0
		for _, t := range s.st.Tasks {
			if t.Auto != "" && t.Project == p && t.Created.Format("2006-01-02") == day {
				n++
			}
		}
		if n < limit {
			return p
		}
	}
	return ""
}

// refill makes at most one task for project. It runs as s.running, so
// coming back to the PC (idle mode) cancels it like a task.
func (s *Service) refill(ctx context.Context, project string) {
	_ = os.MkdirAll(s.set.LogDir, 0o755)
	log, _ := os.OpenFile(filepath.Join(s.set.LogDir, "refill.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if log != nil {
		defer log.Close()
		fmt.Fprintf(log, "\n==== %s  %s\n", s.now().Format(time.RFC3339), project)
	}
	made := ""
	if s.autoCount(project)%drillEvery == drillEvery-1 {
		made = s.makeDrill(ctx, project, log)
	}
	if made == "" && ctx.Err() == nil {
		made = s.makeTestsTask(ctx, project, log)
	}
	if made == "" && ctx.Err() == nil && log != nil {
		fmt.Fprintf(log, "---- nothing queued; trying again in %s\n", refillPause)
	}

	s.mu.Lock()
	s.cancel, s.running = nil, ""
	if made == "" && ctx.Err() == nil {
		s.refillAfter = s.now().Add(refillPause)
	}
	s.mu.Unlock()
	s.poke()
}

func (s *Service) autoCount(project string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.st.Tasks {
		if t.Auto != "" && t.Project == project {
			n++
		}
	}
	return n
}

// tried / markTried: functions a refill already used or gave up on
// (key "tests:path:fn" or "drill:path:fn"), so it doesn't go round in circles.
func (s *Service) tried(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if contains(s.st.Tried, key) {
		return true
	}
	for _, t := range s.st.Tasks {
		if m := underTest.FindStringSubmatch(t.Text); m != nil && key == "tests:"+m[2]+":"+m[1] {
			return true // someone queued it by hand
		}
		if t.Drill != nil && key == "drill:"+t.Drill.File+":"+t.Drill.Function {
			return true
		}
	}
	return false
}

func (s *Service) markTried(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !contains(s.st.Tried, key) {
		s.st.Tried = append(s.st.Tried, key)
		s.saveLocked()
	}
}

// scratch: a detached worktree of the project's HEAD to try things in, and
// the function that removes it.
func (s *Service) scratch(ctx context.Context, repo, project string, log *os.File) (string, func(), error) {
	wt := filepath.Join(s.set.Worktrees, safeName(project)+"-refill")
	removeWorktree(ctx, repo, wt) // one left over from an interrupted refill
	_ = os.MkdirAll(s.set.Worktrees, 0o755)
	if _, err := git(ctx, repo, "worktree", "add", "--detach", wt, "HEAD"); err != nil {
		return "", nil, fmt.Errorf("couldn't make a worktree: %v", err)
	}
	linkDeps(repo, wt, log)
	return wt, func() { removeWorktree(context.Background(), repo, wt) }, nil
}

func (s *Service) makeTestsTask(ctx context.Context, project string, log *os.File) string {
	repo, ok := s.projects.Path(project)
	if !ok {
		return ""
	}
	list, err := s.Suggest(project)
	if err != nil {
		return ""
	}
	command := s.set.Tests[project]
	for _, sg := range list {
		key := "tests:" + sg.Path + ":" + sg.Function
		if s.tried(key) || ctx.Err() != nil {
			continue
		}
		s.markTried(key) // whatever happens, don't pick it again
		if id := s.writeTestsTask(ctx, project, repo, command, sg, log); id != "" {
			return id
		}
		if ctx.Err() != nil {
			return ""
		}
	}
	return ""
}

// writeTestsTask has Claude write and prove the tests for one function, and
// queues the task built from them.
func (s *Service) writeTestsTask(ctx context.Context, project, repo, command string, sg Suggestion, log *os.File) string {
	wt, done, err := s.scratch(ctx, repo, project, log)
	if err != nil {
		if log != nil {
			fmt.Fprintln(log, err)
		}
		return ""
	}
	defer done()
	if log != nil {
		fmt.Fprintf(log, "---- tests for %s in %s\n", sg.Function, sg.Path)
	}
	reply, err := s.steps.Claude(ctx, wt, fillPrompt(sg, command, testAreas(repo)), true, log)
	if err != nil {
		return ""
	}
	var ans struct {
		Verdict  string   `json:"verdict"`
		TestFile string   `json:"test_file"`
		DB       bool     `json:"db"`
		Cases    []string `json:"cases"`
		Setup    string   `json:"setup"`
		Why      string   `json:"why"`
	}
	if json.Unmarshal([]byte(lastJSON(reply)), &ans) != nil || ans.Verdict != "ok" || len(ans.Cases) < 3 || len(ans.Cases) > 8 {
		if log != nil {
			fmt.Fprintf(log, "---- skipped: %s\n", firstLine(ans.Why+" "+lastJSON(reply), 300))
		}
		return ""
	}
	_, testRel, ok := insideRepo(wt, ans.TestFile)
	if !ok || !testFile.MatchString(testRel) || !strings.HasSuffix(testRel, ".py") {
		return ""
	}
	if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(testRel))); err == nil {
		return "" // must be a new file
	}
	key, err := os.ReadFile(filepath.Join(wt, filepath.FromSlash(testRel)))
	if err != nil {
		return ""
	}
	// Claude says they pass; Away checks: they pass, and there's one per case.
	targets := testTargets(command, []string{testRel})
	if len(targets) == 0 {
		return ""
	}
	passed, out := s.steps.Tests(ctx, wt, verbose(command)+" "+targets[0], log)
	if n := len(testResults(out)); !passed || n != len(ans.Cases) {
		if log != nil {
			fmt.Fprintf(log, "---- not queued: passed=%v, %d tests for %d cases\n", passed, n, len(ans.Cases))
		}
		return ""
	}
	text := draftText(sg.Function, sg.Path, testRel, ans.DB, ans.Cases, command)
	if setup := strings.TrimSpace(ans.Setup); setup != "" {
		text = strings.Replace(text, " Run run_tests", " "+firstLine(setup, 1200)+" Run run_tests", 1)
	}
	t, err := s.addAuto(project, text, "tests", nil)
	if err != nil {
		return ""
	}
	s.keepAnswer(project, t.ID, key)
	if log != nil {
		fmt.Fprintf(log, "---- queued %s\n", t.ID)
	}
	return t.ID
}

func fillPrompt(sg Suggestion, command string, areas []string) string {
	return fmt.Sprintf(`You're preparing a practice task for a local model. Write tests for %s in %s in a new test file, in the folder where tests like these live (the existing ones: %s), named test_<something>.py, and make sure the file doesn't exist yet.

- Read %s and what it uses first. Cover 4 to 7 cases, one test each, chosen to catch real mistakes: boundaries, each branch, permissions for a view (who may, who may not, logged out), what must not change.
- Every expected value must be what the code actually does: run the file with `+"`%s <module or path>`"+` until it passes. If the code looks wrong somewhere, test what it does and say so in "why".
- Use the code's own names for constants, and the project's usual helpers and models; look at a neighbouring test for how users and objects are made.
- If %s can't sensibly be tested this way (it needs the network, a real file upload, a missing template, too much setup), don't write anything: answer skip.

End your reply with exactly one line of JSON and nothing after it:
{"verdict": "ok" | "skip", "test_file": "path/to/test_x.py", "db": true | false, "cases": ["one short sentence per test, saying the input and the expected result", ...], "setup": "one or two sentences telling the model how to build what the tests need (which models or helpers, from where, any URL names)", "why": "for skip, or anything odd you noticed in the code"}`,
		sg.Function, sg.Path, strings.Join(areas, ", "), sg.Path, command, sg.Function)
}

func (s *Service) makeDrill(ctx context.Context, project string, log *os.File) string {
	repo, ok := s.projects.Path(project)
	if !ok {
		return ""
	}
	command := s.set.Tests[project]
	type cand struct{ fn, rel, testRel string }
	var cands []cand
	s.mu.Lock()
	for i := len(s.st.Tasks) - 1; i >= 0; i-- { // newest first
		t := s.st.Tasks[i]
		if t.Project != project || t.Status != StatusAccepted || t.Drill != nil {
			continue
		}
		m, f := underTest.FindStringSubmatch(t.Text), newTestFile.FindStringSubmatch(t.Text)
		if m != nil && f != nil {
			cands = append(cands, cand{m[1], m[2], f[1]})
		}
	}
	s.mu.Unlock()
	for _, c := range cands {
		key := "drill:" + c.rel + ":" + c.fn
		if s.tried(key) || ctx.Err() != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(c.testRel))); err != nil {
			continue // its tests aren't on this branch (yet)
		}
		s.markTried(key)
		if id := s.plantDrill(ctx, project, repo, command, c.fn, c.rel, c.testRel, log); id != "" {
			return id
		}
	}
	return ""
}

func (s *Service) plantDrill(ctx context.Context, project, repo, command, fn, rel, testRel string, log *os.File) string {
	targets := testTargets(command, []string{testRel})
	if len(targets) == 0 {
		return ""
	}
	wt, done, err := s.scratch(ctx, repo, project, log)
	if err != nil {
		return ""
	}
	defer done()
	if log != nil {
		fmt.Fprintf(log, "---- drill in %s (%s), against %s\n", fn, rel, targets[0])
	}
	py, err := s.mutantsCommand()
	if err != nil {
		return ""
	}
	ok, out := s.steps.Tests(ctx, wt, mutantCommand(py, "list", rel, fn, -1), log)
	var list []mutant
	if !ok || json.Unmarshal([]byte(lastLineWith(out, "[")), &list) != nil {
		return ""
	}
	file := filepath.Join(wt, filepath.FromSlash(rel))
	orig, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	tries := 0
	for _, mu := range list {
		if mu.Kind == "stub" || tries >= drillTries || ctx.Err() != nil {
			continue // the stub is no puzzle: the whole function is gone
		}
		tries++
		_ = os.WriteFile(file, orig, 0o644)
		if ok, _ := s.steps.Tests(ctx, wt, mutantCommand(py, "apply", rel, fn, mu.ID), log); !ok {
			continue
		}
		caught, out := s.steps.Tests(ctx, wt, verbose(command)+" "+targets[0], log)
		if caught || len(testResults(out)) == 0 {
			continue // not caught, or the tests didn't even run
		}
		text := fmt.Sprintf("A bug has crept into %s in %s: some tests in %s fail now. Run run_tests with the target %s to see which, read %s and the failing tests, and fix the code so they pass. Change only %s: don't change any test. Say in your reply what the bug was.",
			fn, rel, testRel, targets[0], fn, rel)
		t, err := s.addAuto(project, text, "drill", &Drill{File: rel, Function: fn, Mutant: mu.ID, What: fmt.Sprintf("%s (line %d)", mu.What, mu.Line), Target: targets[0]})
		if err != nil {
			return ""
		}
		if log != nil {
			fmt.Fprintf(log, "---- queued drill %s: %s\n", t.ID, mu.What)
		}
		return t.ID
	}
	return ""
}

// mutantsCommand writes mutants.py where Away can run it, and returns
// "python <path>". No quotes: on Windows, Go escapes them on the way to
// cmd /C, and Python then looked for a file literally named
// "C:\...\mutants.py" (10-20: the mutation check skipped every task).
// Neither path holds a space: the log folder is Homebase's own, and the
// file paths come from underTest's [\w./-] or the project's own files.
func (s *Service) mutantsCommand() (string, error) {
	script := filepath.Join(s.set.LogDir, "mutants.py")
	_ = os.MkdirAll(s.set.LogDir, 0o755)
	if err := os.WriteFile(script, mutantsPy, 0o644); err != nil {
		return "", err
	}
	return python() + " " + script, nil
}

func (s *Service) addAuto(project, text, kind string, d *Drill) (*Task, error) {
	t, err := s.Add(project, text)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if x := s.find(t.ID); x != nil {
		x.Auto, x.Drill = kind, d
		s.saveLocked()
		t = x
	}
	return t, nil
}

// keepAnswer: Claude's passing tests for an automatic task, kept for later
// (comparing with what the local model wrote; training data).
func (s *Service) keepAnswer(project, id string, b []byte) {
	if s.set.ExamplesDir == "" {
		return
	}
	dir := filepath.Join(s.set.ExamplesDir, safeName(project), "answers")
	if os.MkdirAll(dir, 0o755) == nil {
		_ = os.WriteFile(filepath.Join(dir, id+".py"), b, 0o644)
	}
}

// lastJSON: the last {...} in a reply that has a "verdict" (allowing arrays,
// unlike verdictLine).
func lastJSON(reply string) string {
	i := strings.LastIndex(reply, `"verdict"`)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(reply[:i], "{")
	if start < 0 {
		return ""
	}
	var v map[string]any
	if json.NewDecoder(strings.NewReader(reply[start:])).Decode(&v) != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

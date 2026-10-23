package away

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// refillSteps: the local model and reviews are fakes; tests really run, and
// "Claude" writes the test file the refill asks for (good, or short a test).
type refillSteps struct {
	realTests
	tests   string // the file Claude writes, at tests/test_calc_auto.py
	cases   []string
	prompts []string
}

func (r *refillSteps) Claude(ctx context.Context, dir, prompt string, edit bool, log *os.File) (string, error) {
	if !strings.Contains(prompt, "practice task") {
		return r.realTests.fakeSteps.Claude(ctx, dir, prompt, edit, log)
	}
	r.prompts = append(r.prompts, prompt)
	os.WriteFile(filepath.Join(dir, "tests", "test_calc_auto.py"), []byte(r.tests), 0o644)
	b, _ := json.Marshal(map[string]any{"verdict": "ok", "test_file": "tests/test_calc_auto.py", "db": false, "cases": r.cases, "setup": "Import too_short and MIN_LENGTH from pkg.calc."})
	return "Done.\n" + string(b), nil
}

const strongTests = "import unittest\nfrom pkg.calc import too_short, MIN_LENGTH\n\n\nclass T(unittest.TestCase):\n" +
	"    def test_unset(self):\n        self.assertIs(too_short(None), False)\n\n" +
	"    def test_one_short(self):\n        self.assertIs(too_short('a' * (MIN_LENGTH - 1)), True)\n\n" +
	"    def test_minimum(self):\n        self.assertIs(too_short('a' * MIN_LENGTH), False)\n"

var threeCases = []string{"None is not too short", "one under MIN_LENGTH is too short", "exactly MIN_LENGTH is not"}

// A repo with pkg/calc.py (too_short, untested) and a tests package.
func refillProject(t *testing.T, steps Steps) (*Service, string) {
	t.Helper()
	if _, err := exec.LookPath(python()); err != nil {
		t.Skip("no " + python())
	}
	s, repo, _ := setup(t, steps)
	os.MkdirAll(filepath.Join(repo, "pkg"), 0o755)
	os.MkdirAll(filepath.Join(repo, "tests"), 0o755)
	os.WriteFile(filepath.Join(repo, "pkg", "__init__.py"), nil, 0o644)
	os.WriteFile(filepath.Join(repo, "pkg", "calc.py"), []byte(calc), 0o644)
	os.WriteFile(filepath.Join(repo, "tests", "__init__.py"), nil, 0o644)
	run(t, repo, "add", "pkg", "tests")
	run(t, repo, "commit", "-q", "-m", "calc")
	s.set.Tests = map[string]string{"proj": python() + " -m unittest"}
	s.set.Autofill = map[string]int{"proj": 3}
	return s, repo
}

// One step, then wait for the refill it started (without starting tasks).
func refillOnce(t *testing.T, s *Service) {
	t.Helper()
	s.SetMode(ModeOn)
	s.step(context.Background())
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		busy := s.running == refillRunning
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the refill never finished")
}

func TestRefillQueuesAProvenTestsTask(t *testing.T) {
	steps := &refillSteps{realTests: realTests{&fakeSteps{}}, tests: strongTests, cases: threeCases}
	s, repo := refillProject(t, steps)
	refillOnce(t, s)
	v := s.View()
	if len(v.Tasks) != 1 {
		t.Fatalf("%d tasks", len(v.Tasks))
	}
	got := v.Tasks[0]
	for _, want := range []string{
		"Add tests for too_short in pkg/calc.py, in a new file tests/test_calc_auto.py",
		"one test each: None is not too short; one under MIN_LENGTH is too short; exactly MIN_LENGTH is not.",
		"Import too_short and MIN_LENGTH from pkg.calc. Run run_tests with the target tests.test_calc_auto",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("task lacks %q:\n%s", want, got.Text)
		}
	}
	if got.Auto != "tests" || got.Status != StatusQueued {
		t.Fatalf("%+v", got)
	}
	// The answer key is kept; the project and the scratch worktree aren't touched.
	if b, err := os.ReadFile(filepath.Join(s.set.ExamplesDir, "proj", "answers", got.ID+".py")); err != nil || string(b) != strongTests {
		t.Fatalf("answer key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "tests", "test_calc_auto.py")); err == nil {
		t.Fatal("wrote into the checkout")
	}
	if entries, _ := os.ReadDir(s.set.Worktrees); len(entries) != 0 {
		t.Fatalf("scratch worktree left: %v", entries)
	}
	// It doesn't pick the same function again.
	if !s.tried("tests:pkg/calc.py:too_short") {
		t.Fatal("not marked as tried")
	}
}

func TestRefillChecksClaudesTestsItself(t *testing.T) {
	// Five cases listed, three tests written: not one test per case, so no task.
	steps := &refillSteps{realTests: realTests{&fakeSteps{}}, tests: strongTests, cases: append(threeCases, "a fourth", "a fifth")}
	s, _ := refillProject(t, steps)
	refillOnce(t, s)
	if n := len(s.View().Tasks); n != 0 {
		t.Fatalf("queued %d", n)
	}
	s.mu.Lock()
	waits := s.refillAfter.After(s.now())
	s.mu.Unlock()
	if !waits {
		t.Fatal("a refill that found nothing should wait before trying again")
	}
	s.step(context.Background())
	if s.View().Running == refillRunning {
		t.Fatal("tried again straight away")
	}
}

func TestRefillStaysWithinTheDailyCap(t *testing.T) {
	steps := &refillSteps{realTests: realTests{&fakeSteps{}}, tests: strongTests, cases: threeCases}
	s, _ := refillProject(t, steps)
	s.set.Autofill = map[string]int{"proj": 1}
	s.addAuto("proj", "an automatic task from earlier today", "tests", nil)
	s.mu.Lock()
	for _, x := range s.st.Tasks {
		x.Status = StatusAccepted
	}
	s.mu.Unlock()
	refillOnce(t, s)
	if len(steps.prompts) != 0 {
		t.Fatal("went past the day's cap")
	}
}

// A drill: a function with accepted, merged tests gets a bug its tests catch.
func TestDrillPlantsABugTheTestsCatch(t *testing.T) {
	s, repo := refillProject(t, realTests{&fakeSteps{}})
	os.WriteFile(filepath.Join(repo, "tests", "test_calc.py"), []byte(strongTests), 0o644)
	run(t, repo, "add", "tests")
	run(t, repo, "commit", "-q", "-m", "tests")
	done, _ := s.Add("proj", "Add tests for too_short in pkg/calc.py, in a new file tests/test_calc.py, using unittest.")
	s.mu.Lock()
	s.find(done.ID).Status = StatusAccepted
	s.mu.Unlock()
	id := s.makeDrill(context.Background(), "proj", nil)
	if id == "" {
		t.Fatal("no drill")
	}
	var d Task
	for _, x := range s.View().Tasks {
		if x.ID == id {
			d = *x
		}
	}
	if d.Auto != "drill" || d.Drill == nil || d.Drill.Target != "tests.test_calc" || d.Drill.Function != "too_short" || d.Drill.What == "" {
		t.Fatalf("%+v %+v", d, d.Drill)
	}
	if !strings.Contains(d.Text, "A bug has crept into too_short in pkg/calc.py") || strings.Contains(d.Text, d.Drill.What) {
		t.Fatalf("the task text gives the bug away, or misses it:\n%s", d.Text)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "pkg", "calc.py")); string(b) != calc {
		t.Fatal("the checkout was changed")
	}
}

// Running a drill: the bug is planted before the model starts, the diff
// starts after it, the review is told, and Accept merges nothing.
func TestADrillRunsAndAcceptMergesNothing(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"fixed the comparison"}`}}
	steps.qwen = func(dir, task string) error {
		b, _ := os.ReadFile(filepath.Join(dir, "pkg", "calc.py"))
		if string(b) == calc {
			return os.WriteFile(filepath.Join(dir, "out.txt"), []byte("no bug here?\n"), 0o644)
		}
		return os.WriteFile(filepath.Join(dir, "pkg", "calc.py"), []byte(calc), 0o644) // the fix
	}
	s, repo := refillProject(t, realTests{steps})
	os.WriteFile(filepath.Join(repo, "tests", "test_calc.py"), []byte(strongTests), 0o644)
	run(t, repo, "add", "tests")
	run(t, repo, "commit", "-q", "-m", "tests")
	done, _ := s.Add("proj", "Add tests for too_short in pkg/calc.py, in a new file tests/test_calc.py, using unittest.")
	s.mu.Lock()
	s.find(done.ID).Status = StatusAccepted
	s.mu.Unlock()
	id := s.makeDrill(context.Background(), "proj", nil)
	s.set.Autofill = nil
	s.SetMode(ModeOn)
	got := waitFor(t, s, id, finished)
	if got.Status != StatusDone || got.Verdict != "pass" {
		t.Fatalf("%+v", got)
	}
	if len(steps.claudePrompts) != 1 || !strings.Contains(steps.claudePrompts[0], "This task is a drill") || !strings.Contains(steps.claudePrompts[0], got.Drill.What) {
		t.Fatalf("the review wasn't told about the drill")
	}
	if diff := run(t, repo, "diff", got.BaseCommit+".."+got.Branch, "--", "pkg/calc.py"); !strings.Contains(diff, "MIN_LENGTH") {
		t.Fatalf("the diff from the planted bug should show the fix:\n%s", diff)
	}
	head := run(t, repo, "rev-parse", "HEAD")
	if err := s.Act(context.Background(), id, "accept"); err != nil {
		t.Fatal(err)
	}
	if run(t, repo, "rev-parse", "HEAD") != head || strings.Contains(run(t, repo, "branch"), "away/") {
		t.Fatal("accepting a drill merged something, or kept its branch")
	}
}

func TestLastJSON(t *testing.T) {
	got := lastJSON("Wrote it.\n{\"verdict\": \"ok\", \"cases\": [\"a\", \"b\"], \"setup\": \"x {y}\"}\n")
	if !strings.Contains(got, `"cases":["a","b"]`) {
		t.Fatalf("%q", got)
	}
	if lastJSON("no json") != "" {
		t.Fatal("found json in none")
	}
}

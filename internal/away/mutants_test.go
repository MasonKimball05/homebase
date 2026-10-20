package away

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realTests: fakeSteps, but tests (and the mutation script) really run.
type realTests struct{ *fakeSteps }

func (r realTests) Tests(ctx context.Context, dir, command string, log *os.File) (bool, string) {
	return CLI{}.Tests(ctx, dir, command, log)
}

const calc = `MIN_LENGTH = 32


def require(f):
    return f


@require
def too_short(token):
    """Whether a set token is too short to trust."""
    if not token:
        return False
    return len(token) < MIN_LENGTH
`

func mutationProject(t *testing.T, tests string) (*Service, string) {
	t.Helper()
	if _, err := exec.LookPath(python()); err != nil {
		t.Skip("no " + python())
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tests"), 0o755)
	os.WriteFile(filepath.Join(dir, "calc.py"), []byte(calc), 0o644)
	os.WriteFile(filepath.Join(dir, "tests", "__init__.py"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "tests", "test_calc.py"), []byte(tests), 0o644)
	s := New(Settings{LogDir: t.TempDir()}, fakeProjects{}, realTests{&fakeSteps{}}, nil, t.Logf)
	return s, dir
}

func TestMutationCheckFindsTestsThatTestNothing(t *testing.T) {
	s, dir := mutationProject(t, `import unittest
from calc import too_short, MIN_LENGTH


class T(unittest.TestCase):
    def test_never_calls_it(self):
        self.assertEqual(MIN_LENGTH, 32)

    def test_short(self):
        self.assertIsInstance(too_short("a"), bool)
`)
	task := Task{Text: "Add tests for too_short in calc.py, in a new file tests/test_calc.py."}
	notes, summary := s.mutationCheck(context.Background(), task, dir, python()+" -m unittest", []string{"tests.test_calc"}, nil)
	for _, want := range []string{
		"these tests still pass, so they never really call it: test_never_calls_it.",
		"`len(token) < MIN_LENGTH` with < changed to <=",
		"Don't change it; change your tests",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	if strings.Contains(notes, "@require") {
		t.Error("removed a decorator that guards nothing")
	}
	if !strings.Contains(summary, "test(s) pass even without it") {
		t.Errorf("summary: %q", summary)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "calc.py")); string(b) != calc {
		t.Fatalf("the real file wasn't put back:\n%s", b)
	}
}

func TestMutationCheckPassesTestsThatCatchBugs(t *testing.T) {
	s, dir := mutationProject(t, `import unittest
from calc import too_short, MIN_LENGTH


class T(unittest.TestCase):
    def test_unset(self):
        self.assertIs(too_short(None), False)

    def test_one_short(self):
        self.assertIs(too_short("a" * (MIN_LENGTH - 1)), True)

    def test_minimum(self):
        self.assertIs(too_short("a" * MIN_LENGTH), False)
`)
	task := Task{Text: "Add tests for too_short in calc.py, in a new file tests/test_calc.py."}
	notes, summary := s.mutationCheck(context.Background(), task, dir, python()+" -m unittest", []string{"tests.test_calc"}, nil)
	if notes != "" || !strings.Contains(summary, "0 not noticed") {
		t.Fatalf("summary %q, notes:\n%s", summary, notes)
	}
}

func TestMutationCheckSkipsWhatItCantCheck(t *testing.T) {
	s, dir := mutationProject(t, "")
	for _, c := range []struct{ text, command string }{
		{"Fix the bug in calc.py.", python() + " -m unittest"},           // no function named
		{"Add tests for too_short in calc.py.", "npx vitest run"},        // can't name each test
		{"Add tests for missing in calc.py.", python() + " -m unittest"}, // no such function
	} {
		if notes, summary := s.mutationCheck(context.Background(), Task{Text: c.text}, dir, c.command, []string{"tests.test_calc"}, nil); notes != "" || summary != "" {
			t.Errorf("%q: %q %q", c.text, summary, notes)
		}
	}
}

func TestTestResults(t *testing.T) {
	out := "test_a (tests.test_x.T.test_a) ... ok\ntest_b (tests.test_x.T.test_b)\nThe docstring. ... FAIL\r\ntest_c (tests.test_x.T.test_c) ... ERROR\n"
	got := testResults(out)
	if len(got) != 3 || !got["test_a"] || got["test_b"] || got["test_c"] {
		t.Fatalf("%v", got)
	}
}

// In a real attempt: weak tests go back to Qwen before any review, without
// using up a round, and the review sees the strengthened ones.
func TestWeakTestsGoBackBeforeReview(t *testing.T) {
	if _, err := exec.LookPath(python()); err != nil {
		t.Skip("no " + python())
	}
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}}
	steps.qwen = func(dir, task string) error {
		os.MkdirAll(filepath.Join(dir, "tests"), 0o755)
		os.WriteFile(filepath.Join(dir, "tests", "__init__.py"), nil, 0o644)
		test := "import unittest\nfrom calc import too_short\n\n\nclass T(unittest.TestCase):\n    def test_short(self):\n        self.assertIsInstance(too_short('a'), bool)\n"
		if strings.Contains(task, "broke too_short") {
			test = "import unittest\nfrom calc import too_short, MIN_LENGTH\n\n\nclass T(unittest.TestCase):\n" +
				"    def test_unset(self):\n        self.assertIs(too_short(None), False)\n\n" +
				"    def test_one_short(self):\n        self.assertIs(too_short('a' * (MIN_LENGTH - 1)), True)\n\n" +
				"    def test_minimum(self):\n        self.assertIs(too_short('a' * MIN_LENGTH), False)\n"
		}
		return os.WriteFile(filepath.Join(dir, "tests", "test_calc.py"), []byte(test), 0o644)
	}
	s, repo, _ := setup(t, realTests{steps})
	os.WriteFile(filepath.Join(repo, "calc.py"), []byte(calc), 0o644)
	run(t, repo, "add", "calc.py")
	run(t, repo, "commit", "-q", "-m", "calc")
	s.set.Tests = map[string]string{"proj": python() + " -m unittest"}
	task, _ := s.Add("proj", "Add tests for too_short in calc.py, in a new file tests/test_calc.py.")
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	var who []string
	for _, r := range got.Rounds {
		who = append(who, r.Who+":"+r.Verdict)
	}
	if strings.Join(who, " ") != "qwen: tests:pass mutants:revise qwen: tests:pass review:pass" || got.Verdict != "pass" {
		t.Fatalf("%v %+v", who, got)
	}
	if len(steps.claudePrompts) != 1 || !strings.Contains(steps.claudePrompts[0], "<mutation-check>") {
		t.Fatalf("%d reviews, or the review wasn't told what the check found", len(steps.claudePrompts))
	}
	if b := run(t, repo, "show", got.Branch+":calc.py"); b != calc {
		t.Fatalf("a mutant reached the branch:\n%s", b)
	}
}

// 10-20: quotes in these lines reached Python escaped on Windows, and the
// check never ran. The script and file paths have no spaces, so no quotes.
func TestMutantCommandsHaveNoQuotes(t *testing.T) {
	for _, line := range []string{
		mutantCommand(`python C:\Users\Admin\apps\homebase\logs\away\mutants.py`, "list", "src/groupme.py", "blocked_reason", -1),
		mutantCommand("python3 /tmp/mutants.py", "apply", "src/groupme.py", "blocked_reason", 3),
	} {
		if strings.ContainsAny(line, `"'`) {
			t.Errorf("quoted: %s", line)
		}
	}
	if got := mutantCommand("py m.py", "apply", "a.py", "f", 3); got != "py m.py apply a.py f 3" {
		t.Fatal(got)
	}
}

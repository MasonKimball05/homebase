package away

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func suggestRepo(t *testing.T) (*Service, string) {
	t.Helper()
	repo := t.TempDir()
	write := func(rel, text string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
	}
	write("src/util.py", "def covered(x):\n    return x\n\n\ndef lonely(token):\n    \"\"\"Warnings for a short token.\"\"\"\n    if token and len(token) < 32:\n        return ['short']\n    return []\n\n\ndef _private():\n    pass\n\n\nclass Thing:\n    def method(self):\n        pass\n")
	write("src/big.py", "def huge(a):\n"+strings.Repeat("    a += 1\n", 50)+"    return a\n\n\ndef enormous(a):\n"+strings.Repeat("    a += 1\n", 150)+"    return a\n\n\ndef tiny(a):\n    return a\n")
	write("src/tests/security/test_util.py", "from src.util import covered\n\ndef test_covered():\n    assert covered(1) == 1\n")
	write("src/migrations/0001_initial.py", "def forwards(apps, schema_editor):\n    pass\n")
	write("Parliament/settings.py", "def env_list(name):\n    return []\n")
	s := New(Settings{Tests: map[string]string{"proj": "python manage.py test --noinput"}}, fakeProjects{"proj": repo}, &fakeSteps{}, nil, t.Logf)
	return s, repo
}

func TestSuggestFindsUntestedPublicFunctions(t *testing.T) {
	s, _ := suggestRepo(t)
	list, err := s.Suggest("proj")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sg := range list {
		names = append(names, sg.Path+":"+sg.Function)
	}
	// Untested, public, 5 to 120 lines; 8-60 lines first; tests, migrations and settings skipped.
	if strings.Join(names, " ") != "src/big.py:huge src/util.py:lonely" {
		t.Fatalf("%v", names)
	}
	if list[1].Doc != "Warnings for a short token." || list[1].Line != 5 || list[1].Lines < 5 {
		t.Fatalf("%+v", list[1])
	}
	if _, err := s.Suggest("nope"); err == nil {
		t.Fatal("unknown project")
	}
}

func TestDraftWritesATaskInTheShapeThatWorks(t *testing.T) {
	s, _ := suggestRepo(t)
	var asked string
	s.generate = func(ctx context.Context, prompt string) (string, error) {
		asked = prompt
		return `{"test_file": "src/tests/security/test_lonely.py", "db": false, "cases": ["None gives no warnings.", "a 31-character token gives one warning", "a 32-character token gives none"]}`, nil
	}
	d, err := s.DraftTask(context.Background(), "proj", "src/util.py", "lonely")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked, "len(token) < 32") || !strings.Contains(asked, "src/tests/security") {
		t.Fatalf("the model wasn't shown the function and the test folders:\n%s", asked)
	}
	for _, want := range []string{
		"Add tests for lonely in src/util.py, in a new file src/tests/security/test_lonely.py, using Django's SimpleTestCase (no database).",
		"one test each: None gives no warnings; a 31-character token gives one warning; a 32-character token gives none.",
		"Run run_tests with the target src.tests.security.test_lonely until it passes.",
		"Tests only: don't change any other file.",
	} {
		if !strings.Contains(d.Task, want) {
			t.Errorf("task lacks %q:\n%s", want, d.Task)
		}
	}
}

func TestDraftHandlesBadAnswersAndPaths(t *testing.T) {
	s, _ := suggestRepo(t)
	s.generate = func(ctx context.Context, prompt string) (string, error) {
		return `{"test_file": "../../etc/test_x.py", "db": true, "cases": ["it works"]}`, nil
	}
	d, err := s.DraftTask(context.Background(), "proj", "src/util.py", "lonely")
	if err != nil || d.TestFile != "src/tests/security/test_util.py" || !strings.Contains(d.Task, "Django's TestCase.") {
		t.Fatalf("a test file outside the project should fall back to a test folder: %+v %v", d, err)
	}
	s.generate = func(ctx context.Context, prompt string) (string, error) { return "not json", nil }
	if _, err := s.DraftTask(context.Background(), "proj", "src/util.py", "lonely"); err == nil {
		t.Fatal("an unusable answer should be an error")
	}
	for _, bad := range [][2]string{{"../outside.py", "x"}, {"/etc/passwd", "x"}, {"C:/x.py", "x"}, {"src/util.py", "nope"}, {"README.md", "x"}} {
		if _, err := s.DraftTask(context.Background(), "proj", bad[0], bad[1]); err == nil {
			t.Errorf("drafted %v", bad)
		}
	}
}

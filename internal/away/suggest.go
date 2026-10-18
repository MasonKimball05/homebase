package away

// Task suggestions: candidates for the queue, so there's always good work to
// give the local model (and more examples to learn from). For now: Python
// functions no test mentions. Drafting one asks the local model to read the
// function and list what to test; Homebase wraps that in the shape that worked
// on real tasks (the test file, every case spelled out, run_tests on that
// module, tests only), for you to edit and add.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Suggestion is one untested function.
type Suggestion struct {
	Path     string `json:"path"` // relative, with /
	Function string `json:"function"`
	Line     int    `json:"line"`
	Doc      string `json:"doc,omitempty"` // its docstring's first line
	Lines    int    `json:"lines"`         // how long it is
}

var (
	pyDef      = regexp.MustCompile(`^def ([A-Za-z][A-Za-z0-9_]*)\(`)
	pySkipDirs = map[string]bool{"tests": true, "test": true, "migrations": true, "node_modules": true, ".venv": true, "venv": true, "env": true, "__pycache__": true, ".git": true, "static": true, "staticfiles": true, "media": true}
	pySkipFile = regexp.MustCompile(`(^|/)(test_[^/]*|[^/]*_test|conftest|manage|settings[^/]*|wsgi|asgi|setup)\.py$`)
)

const (
	maxSuggestions = 40
	// The size of function that made a good task on real runs: big enough to
	// have behaviour worth a test (one- and two-line wrappers aren't), small
	// enough for one task.
	minLines = 5
	maxLines = 120
)

// Suggest lists a project's top-level Python functions that no test file
// mentions by name: 8-60 lines first, documented ones first within that
// (the docstring says what "right" is), then smaller before bigger.
func (s *Service) Suggest(project string) ([]Suggestion, error) {
	repo, ok := s.projects.Path(project)
	if !ok {
		return nil, errors.New("no such project")
	}
	var tests bytes.Buffer
	var sources []string
	_ = filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(repo, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == ".venv" || d.Name() == "venv" || d.Name() == "__pycache__") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".py") {
			return nil
		}
		if testFile.MatchString(rel) {
			if b, err := os.ReadFile(p); err == nil {
				tests.Write(b)
				tests.WriteByte('\n')
			}
			return nil
		}
		for _, part := range strings.Split(rel, "/")[:strings.Count(rel, "/")] {
			if pySkipDirs[part] {
				return nil
			}
		}
		// Files at the top are one-off scripts (manage.py, migrate_*.py), not the app's code.
		if strings.Contains(rel, "/") && !pySkipFile.MatchString(rel) {
			sources = append(sources, rel)
		}
		return nil
	})
	// Every identifier the tests mention, once (a regex per function over all
	// the tests took 25 s on Parliament).
	tested := map[string]bool{}
	for _, w := range identifier.FindAllString(tests.String(), -1) {
		tested[w] = true
	}
	var out []Suggestion
	for _, rel := range sources {
		for _, sg := range topLevelFunctions(filepath.Join(repo, filepath.FromSlash(rel))) {
			if strings.HasPrefix(sg.Function, "_") || sg.Lines < minLines || sg.Lines > maxLines || tested[sg.Function] {
				continue
			}
			sg.Path = rel
			out = append(out, sg)
		}
	}
	// Today's good tasks were 8-60 lines: enough logic for several cases.
	sweet := func(sg Suggestion) bool { return sg.Lines >= 8 && sg.Lines <= 60 }
	sort.SliceStable(out, func(i, j int) bool {
		if sweet(out[i]) != sweet(out[j]) {
			return sweet(out[i])
		}
		if (out[i].Doc != "") != (out[j].Doc != "") {
			return out[i].Doc != ""
		}
		if out[i].Lines != out[j].Lines {
			return out[i].Lines < out[j].Lines
		}
		return out[i].Path+out[i].Function < out[j].Path+out[j].Function
	})
	if len(out) > maxSuggestions {
		out = out[:maxSuggestions]
	}
	if out == nil {
		out = []Suggestion{}
	}
	return out, nil
}

var identifier = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// topLevelFunctions: `def name(` at column 0, with its length and docstring line.
func topLevelFunctions(path string) []Suggestion {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	var out []Suggestion
	for i, l := range lines {
		m := pyDef.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if lines[j] != "" && !strings.HasPrefix(lines[j], " ") && !strings.HasPrefix(lines[j], "\t") && !strings.HasPrefix(lines[j], ")") {
				end = j
				break
			}
		}
		out = append(out, Suggestion{Function: m[1], Line: i + 1, Lines: end - i, Doc: docLine(lines[i+1 : end])})
	}
	return out
}

func docLine(body []string) string {
	for _, l := range body {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		for _, q := range []string{`"""`, `'''`} {
			if strings.HasPrefix(t, q) {
				t = strings.TrimSpace(strings.Trim(strings.TrimPrefix(t, q), `"'`))
				return firstLine(t, 160)
			}
		}
		return ""
	}
	return ""
}

// ---- drafting ----

// Generator asks a model for a JSON answer (Ollama's /api/generate in production).
type Generator func(ctx context.Context, prompt string) (string, error)

// OllamaGenerator uses Ollama's /api/generate with JSON output.
func OllamaGenerator(host, model string) Generator {
	client := &http.Client{Timeout: 5 * time.Minute}
	base := strings.TrimRight(host, "/")
	return func(ctx context.Context, prompt string) (string, error) {
		body, _ := json.Marshal(map[string]any{"model": model, "prompt": prompt, "stream": false, "format": "json", "options": map[string]any{"temperature": 0.2}})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/generate", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var out struct {
			Response string `json:"response"`
			Error    string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", err
		}
		if out.Error != "" {
			return "", errors.New(out.Error)
		}
		return out.Response, nil
	}
}

// Draft is a task written for one suggestion.
type Draft struct {
	Task     string   `json:"task"`
	TestFile string   `json:"testFile"`
	Cases    []string `json:"cases"`
}

const maxDraftSource = 6000

// DraftTask asks the local model what to test in a function and writes the task.
func (s *Service) DraftTask(ctx context.Context, project, path, function string) (Draft, error) {
	if s.generate == nil {
		return Draft{}, errors.New("drafting needs the local model (ai.ollama in homebase.json)")
	}
	repo, ok := s.projects.Path(project)
	if !ok {
		return Draft{}, errors.New("no such project")
	}
	full, rel, ok := insideRepo(repo, path)
	if !ok || !strings.HasSuffix(rel, ".py") {
		return Draft{}, errors.New("not a Python file in that project")
	}
	var src string
	for _, sg := range topLevelFunctions(full) {
		if sg.Function == function {
			b, _ := os.ReadFile(full)
			lines := strings.Split(string(b), "\n")
			src = strings.Join(lines[sg.Line-1:min(len(lines), sg.Line-1+sg.Lines)], "\n")
		}
	}
	if src == "" {
		return Draft{}, fmt.Errorf("no top-level function %s in %s", function, rel)
	}
	areas := testAreas(repo)
	prompt := fmt.Sprintf(`You are planning unit tests for one Python function in a Django project. Here is %s from %s:

%s

Existing test folders: %s

Answer with JSON only: {"test_file": "...", "db": true|false, "cases": ["...", "..."]}
- test_file: a new file path in the existing test folder that fits best, named test_<something>.py.
- db: true if the tests need the database (models, querysets, the ORM), false if plain objects will do.
- cases: what to test, one case per distinct behaviour. Each case is a concrete input and its expected result, in one sentence. For every comparison in the code (<, <=, >=, ==, len, a limit), include the value exactly at the boundary. Include None/empty/missing inputs only where the code handles them. Only behaviour the code really has: never a case for handling it doesn't do (if it would raise, say it raises). No two cases may test the same thing. At most 6.

A good list, for a check that warns when a token is set but shorter than MIN_TOKEN_LENGTH:
["no token (None) gives no warnings", "an empty token gives no warnings", "a token of MIN_TOKEN_LENGTH - 1 characters gives exactly one warning with id src.W005", "a token of exactly MIN_TOKEN_LENGTH characters gives no warnings"]`, function, rel, clipText(src, maxDraftSource), strings.Join(areas, ", "))
	gctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	raw, err := s.generate(gctx, prompt)
	if err != nil {
		return Draft{}, fmt.Errorf("the local model: %v", err)
	}
	var ans struct {
		TestFile string   `json:"test_file"`
		DB       bool     `json:"db"`
		Cases    []string `json:"cases"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &ans); err != nil || len(ans.Cases) == 0 {
		return Draft{}, fmt.Errorf("the local model's answer wasn't usable: %s", firstLine(raw, 200))
	}
	testRel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(ans.TestFile)), "/")
	if !testFile.MatchString(testRel) || strings.Contains(testRel, "..") {
		testRel = "tests/test_" + strings.TrimSuffix(filepath.Base(rel), ".py") + ".py"
		if len(areas) > 0 {
			testRel = areas[0] + "/test_" + strings.TrimSuffix(filepath.Base(rel), ".py") + ".py"
		}
	}
	if len(ans.Cases) > 6 {
		ans.Cases = ans.Cases[:6]
	}
	// "A new file": never one that's already there.
	if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(testRel))); err == nil {
		testRel = strings.TrimSuffix(testRel, ".py") + "_" + strings.ToLower(function) + ".py"
	}
	return Draft{Task: draftText(function, rel, testRel, ans.DB, ans.Cases, s.set.Tests[project]), TestFile: testRel, Cases: ans.Cases}, nil
}

// insideRepo: a relative path that stays in the project (no "..", absolute paths or drive letters).
func insideRepo(repo, rel string) (string, string, bool) {
	rel = strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/")
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, ":") {
		return "", "", false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", "", false
		}
	}
	full := filepath.Join(repo, filepath.FromSlash(rel))
	r, err := filepath.Rel(repo, full)
	if err != nil || strings.HasPrefix(r, "..") || filepath.IsAbs(r) {
		return "", "", false
	}
	return full, filepath.ToSlash(r), true
}

// draftText: the shape that worked on real tasks.
func draftText(function, rel, testRel string, db bool, cases []string, testCommand string) string {
	kind := "Django's SimpleTestCase (no database)"
	if db {
		kind = "Django's TestCase"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Add tests for %s in %s, in a new file %s, using %s. Read the function first. Cover every case, one test each:", function, rel, testRel, kind)
	for _, c := range cases {
		fmt.Fprintf(&b, " %s;", strings.TrimRight(strings.TrimSpace(c), ".;"))
	}
	text := strings.TrimSuffix(b.String(), ";") + "."
	if targets := testTargets(testCommand, []string{testRel}); len(targets) > 0 {
		text += fmt.Sprintf(" Run run_tests with the target %s until it passes.", targets[0])
	}
	// The cases come from a model's reading of the code, which can get a
	// boundary backwards: the code decides, and a disagreement gets said.
	return text + " If a case's expected result disagrees with what the code actually does, test what the code does and say so in your reply." +
		" Use the code's own names for constants, not their values. Tests only: don't change any other file."
}

// testAreas: the folders that hold test files, most-used first (at most 15).
func testAreas(repo string) []string {
	count := map[string]int{}
	_ = filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == ".venv" || d.Name() == "venv") {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(repo, p)
		rel = filepath.ToSlash(rel)
		if !d.IsDir() && testFile.MatchString(rel) && strings.HasSuffix(rel, ".py") {
			count[filepath.ToSlash(filepath.Dir(rel))]++
		}
		return nil
	})
	var areas []string
	for a := range count {
		areas = append(areas, a)
	}
	sort.Slice(areas, func(i, j int) bool {
		if count[areas[i]] != count[areas[j]] {
			return count[areas[i]] > count[areas[j]]
		}
		return areas[i] < areas[j]
	})
	if len(areas) > 15 {
		areas = areas[:15]
	}
	return areas
}

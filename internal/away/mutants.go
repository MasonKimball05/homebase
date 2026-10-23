package away

// The mutation check: do Qwen's tests notice when the code they test is
// wrong? On 10-19 three of six tasks went to Claude for tests that "pass
// without checking anything" (an officer who was really a plain member, a
// start state the toggle never left). So before a review, Away breaks the
// function under test a few small ways (mutants.py: the whole function
// replaced by one that raises, a comparison flipped, a `not` dropped, an
// `if` turned around, a returned constant changed, a login or permission
// decorator removed), runs the new test file against each, and puts the
// file back. A change no test notices goes back to Qwen once, like the
// quick check (check.go). Python test commands only (Django's or unittest's,
// whose -v 2 output names each test).

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

//go:embed mutants.py
var mutantsPy []byte

var (
	underTest = regexp.MustCompile(`(?i)\btests for (\w+) in ([\w./-]+\.py)\b`)
	// A test's name, anywhere in the line: Django prints the names and the
	// results on different streams, and merged, a name can land mid-line
	// (10-23: five of six tests found, three refills in a row turned down).
	testLine   = regexp.MustCompile(`\(([\w.]+)\.(test\w*)\)`) // the name as unittest qualifies it: (module.Class.test_x)
	oldLine    = regexp.MustCompile(`^(test\w*) \(`)           // before Python 3.11: "test_x (module.Class)"
	ranLine    = regexp.MustCompile(`(?m)^Ran (\d+) tests? in `)
	testStatus = regexp.MustCompile(`\.\.\. (ok|FAIL|ERROR|skipped|expected failure|unexpected success)`)
)

type mutant struct {
	ID   int    `json:"id"`
	Kind string `json:"kind"`
	Line int    `json:"line"`
	What string `json:"what"`
}

// verbose: the project's test command with each test named in its output.
// "" when Away doesn't know how to ask that of it.
func verbose(command string) string {
	switch {
	case strings.Contains(command, "manage.py test"):
		return command + " -v 2"
	case strings.Contains(command, "unittest"):
		return command + " -v"
	}
	return ""
}

// testResults: each test's name and whether it passed, from -v output.
// Django and unittest print "test_x (module.Class.test_x) ... ok", with the
// docstring's first line in between when there is one.
func testResults(out string) map[string]bool {
	res := map[string]bool{}
	name := ""
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if ms := testLine.FindAllStringSubmatch(l, -1); ms != nil {
			name = ms[len(ms)-1][2]
		} else if m := oldLine.FindStringSubmatch(l); m != nil {
			name = m[1]
		}
		if m := testStatus.FindStringSubmatch(l); m != nil && name != "" {
			res[name] = m[1] == "ok" || m[1] == "skipped" || m[1] == "expected failure"
			name = ""
		}
	}
	return res
}

func python() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}

// mutationCheck runs the check for task t in worktree wt, whose new test
// files are targets (as the project's command names them). It returns notes
// for Qwen ("" when every change was noticed, or there was nothing to check)
// and a line for the round.
func (s *Service) mutationCheck(ctx context.Context, t Task, wt, command string, targets []string, log *os.File) (notes, summary string) {
	m := underTest.FindStringSubmatch(t.Text)
	run := verbose(command)
	if m == nil || run == "" || len(targets) == 0 {
		return "", ""
	}
	fn, rel := m[1], m[2]
	file := filepath.Join(wt, filepath.FromSlash(rel))
	orig, err := os.ReadFile(file)
	if err != nil {
		return "", ""
	}
	py, err := s.mutantsCommand()
	if err != nil {
		return "", ""
	}
	ok, out := s.steps.Tests(ctx, wt, mutantCommand(py, "list", rel, fn, -1), log)
	var list []mutant
	if !ok || json.Unmarshal([]byte(lastLineWith(out, "[")), &list) != nil || len(list) == 0 {
		return "", ""
	}
	// Whatever happens below (a failed apply, a cancelled task), the real file goes back.
	defer os.WriteFile(file, orig, 0o644)

	line := run + " " + strings.Join(targets, " ")
	var never []string     // tests that pass with the function gone
	var unnoticed []string // changes no test noticed
	ran := 0
	for _, mu := range list {
		if ctx.Err() != nil {
			return "", ""
		}
		if err := os.WriteFile(file, orig, 0o644); err != nil {
			return "", ""
		}
		if ok, _ := s.steps.Tests(ctx, wt, mutantCommand(py, "apply", rel, fn, mu.ID), log); !ok {
			continue
		}
		passed, out := s.steps.Tests(ctx, wt, line, log)
		results := testResults(out)
		if len(results) == 0 && !passed {
			continue // it didn't even get to the tests (an import broke): says nothing about them
		}
		ran++
		if mu.Kind == "stub" {
			for name, ok := range results {
				if ok {
					never = append(never, name)
				}
			}
			continue
		}
		if passed {
			unnoticed = append(unnoticed, fmt.Sprintf("%s (line %d)", mu.What, mu.Line))
		}
	}
	if ran == 0 {
		return "", ""
	}
	summary = fmt.Sprintf("%d change(s) to %s, %d not noticed by any test", ran, fn, len(unnoticed))
	if len(never) > 0 {
		summary += fmt.Sprintf("; %d test(s) pass even without it", len(never))
	}
	if len(never) == 0 && len(unnoticed) == 0 {
		return "", summary
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Before review, Away broke %s in %s on purpose, one small change at a time, and ran your tests against each change. A good test fails when the code it tests is wrong.\n", fn, rel)
	if len(never) > 0 {
		slices.Sort(never)
		fmt.Fprintf(&b, "\n- With %s replaced by a function that only raises an error, these tests still pass, so they never really call it: %s. Make each one call %s and check what it does.\n", fn, strings.Join(never, ", "), fn)
	}
	if len(unnoticed) > 0 {
		if len(unnoticed) > 4 {
			unnoticed = unnoticed[:4]
		}
		b.WriteString("\n- No test noticed these changes, so a bug like them would get through:\n")
		for _, u := range unnoticed {
			b.WriteString("  - " + u + "\n")
		}
		b.WriteString("  For each one a listed case covers, make that case's test fail on it (check the result exactly, check that nothing changed when nothing should, use the right kind of user). If no listed case covers one, leave it and say so in your reply.\n")
	}
	b.WriteString("\nThe real code is back as it was. Don't change it; change your tests, run them, then stop.")
	return b.String(), summary
}

// mutantCommand: a mutants.py command line, with no quotes in it (see mutationCheck).
func mutantCommand(py, verb, rel, fn string, id int) string {
	line := fmt.Sprintf("%s %s %s %s", py, verb, rel, fn)
	if id >= 0 {
		line += fmt.Sprintf(" %d", id)
	}
	return line
}

// ranCount: how many tests the runner says it ran ("Ran 6 tests in 4.0s"),
// or -1. Surer than counting the names, which can be split across lines.
func ranCount(out string) int {
	m := ranLine.FindAllStringSubmatch(out, -1)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

// lastLineWith: the last line of out that starts with prefix (the script's
// JSON, after any warnings Python printed first).
func lastLineWith(out, prefix string) string {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

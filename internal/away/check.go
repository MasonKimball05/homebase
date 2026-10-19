package away

// The pre-review check: Qwen's diff against the task's own words, before a
// Claude review is spent on it. It catches the two slips Qwen made most in
// the Away batches (QWEN.md): leaving out something the task named
// (`HttpRequest()` where the task said `RequestFactory`) and typing a
// constant's value where the code has a name (`900` for LOCKOUT_DURATION,
// `'A' * 32` for MIN_TOKEN_LENGTH). Findings go back to Qwen once; it may
// also answer that one doesn't apply, and the review then decides.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	// 'quoted text': examples and expected values, not names to use. A quote
	// between two letters is an apostrophe (Django's), so it can't open one.
	quoted     = regexp.MustCompile(`(^|[^\w])('[^'\n]*'|"[^"\n]*")`)
	pyPath     = regexp.MustCompile(`[\w./-]+\.py\b`)
	nameToken  = regexp.MustCompile(`[A-Za-z_][\w]*(\.[A-Za-z_][\w]*)*`)
	camel      = regexp.MustCompile(`[a-z][A-Z]`)
	pyConstant = regexp.MustCompile(`(?m)^([A-Z_][A-Z0-9_]*)\s*=\s*([^#\n]+?)\s*(#.*)?$`)
	intExpr    = regexp.MustCompile(`^\d+(\s*[*+]\s*\d+)*$`)
	strLiteral = regexp.MustCompile(`^('[^'\\]*'|"[^"\\]*")$`)
	numberTok  = regexp.MustCompile(`(^|[^\w.])(\d+)(\.\d+)?\b`)
	importLine = regexp.MustCompile(`(?m)^\s*(from\s+\S+\s+)?import\s.*$`)
	allCaps    = regexp.MustCompile(`^[A-Z][A-Z0-9]{2,}$`)
	caseList   = regexp.MustCompile(`(?s)one test each:(.*?)\.(\s+[A-Z]|\s*$)`)
	testDef    = regexp.MustCompile(`(?m)^\s*(?:async\s+)?def (test\w*)\s*\(`)
)

// Names Away's own task text uses that aren't things to use in the code.
var notNames = map[string]bool{"run_tests": true}

// taskNames: the code names a task mentions, leaving out quoted examples,
// file paths and the test target. CamelCase, UPPER_SNAKE and dotted names
// (RequestFactory, LOCKOUT_DURATION, timezone.make_aware) and calls
// (get_lockout_key(ip)) always count. The rest come back as maybe: lowercase
// snake_case (the task may only be describing data: "duration_ms") and
// ALLCAPS words (FIELDS, but also HTTP) count only where the code under test
// defines them.
func taskNames(task string) (names, maybe []string) {
	text := quoted.ReplaceAllString(task, "$1")
	text = pyPath.ReplaceAllString(text, "")
	seen := map[string]bool{}
	for _, loc := range nameToken.FindAllStringIndex(text, -1) {
		tok := text[loc[0]:loc[1]]
		called := strings.HasPrefix(text[loc[1]:], "(")
		if strings.HasPrefix(tok, "src.") || strings.HasPrefix(tok, "tests.") {
			continue // a run_tests target
		}
		name := tok
		if i := strings.LastIndexByte(tok, '.'); i >= 0 {
			name = tok[i+1:] // types.SimpleNamespace: SimpleNamespace
		}
		if name == "" || notNames[name] || seen[name] {
			continue
		}
		lower := strings.ToLower(name) == name
		switch {
		case camel.MatchString(name), strings.Contains(name, "_") && (!lower || called || strings.Contains(tok, ".")):
			names = append(names, name)
		case strings.Contains(name, "_") || (allCaps.MatchString(name) && !strings.Contains(tok, ".")):
			maybe = append(maybe, name)
		default:
			continue // a plain word, or out.txt, datetime.time.max
		}
		seen[name] = true
	}
	return names, maybe
}

// listedCases: how many cases a task lists after "one test each:" (Away's
// drafted tasks, and the ones written like them), or 0.
func listedCases(task string) int {
	m := caseList.FindStringSubmatch(quoted.ReplaceAllString(task, "$1''"))
	if m == nil {
		return 0
	}
	n := 0
	for _, c := range strings.Split(m[1], ";") {
		if strings.TrimSpace(c) != "" {
			n++
		}
	}
	return n
}

// defines: whether a Python file has name at module level (a def, a class,
// or NAME = ...).
func defines(src, name string) bool {
	q := regexp.QuoteMeta(name)
	return regexp.MustCompile(`(?m)^(` + q + `\s*=|(async\s+)?def\s+` + q + `\b|class\s+` + q + `\b)`).MatchString(src)
}

// addedLines: the lines a unified diff adds.
func addedLines(diff string) string {
	var b strings.Builder
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
			b.WriteString(l[1:])
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func usesWord(text, word string) bool {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`).MatchString(text)
}

// constants: a Python file's module-level NAME = value, for whole numbers of
// 16 and up (smaller ones are everywhere) and strings of 4 or more
// characters. Values are keyed by how they'd be written in a test.
func constants(src string) map[string][]string {
	out := map[string][]string{}
	for _, m := range pyConstant.FindAllStringSubmatch(src, -1) {
		name, expr := m[1], strings.TrimSpace(m[2])
		key := ""
		switch {
		case intExpr.MatchString(expr):
			if n, ok := evalInts(expr); ok && n >= 16 {
				key = strconv.Itoa(n)
			}
		case strLiteral.MatchString(expr) && len(expr) >= 6:
			key = expr[1 : len(expr)-1]
		}
		if key != "" && strings.Trim(name, "_") != "" {
			out[key] = append(out[key], name)
		}
	}
	return out
}

// evalInts: "15 * 60" and the like (products, then sums).
func evalInts(expr string) (int, bool) {
	total := 0
	for _, term := range strings.Split(expr, "+") {
		prod := 1
		for _, f := range strings.Split(term, "*") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				return 0, false
			}
			prod *= n
		}
		total += prod
	}
	return total, true
}

// hardCoded: values from consts that the added lines type out while using
// none of the names that hold them.
func hardCoded(added string, consts map[string][]string) map[string][]string {
	found := map[string][]string{}
	nums := map[string]bool{}
	for _, m := range numberTok.FindAllStringSubmatch(added, -1) {
		if m[3] == "" { // 20.0 isn't SAMPLE_ONE_IN
			nums[m[2]] = true
		}
	}
	for val, names := range consts {
		used := false
		for _, n := range names {
			used = used || usesWord(added, n)
		}
		if used {
			continue
		}
		_, isNum := strconv.Atoi(val)
		if (isNum == nil && nums[val]) || (isNum != nil && (strings.Contains(added, "'"+val+"'") || strings.Contains(added, `"`+val+`"`))) {
			found[val] = names
		}
	}
	return found
}

// preReview: what the check found in a diff, as notes for Qwen ("" when
// nothing). dir is the worktree, to read the Python files the task names.
func preReview(task, diff, dir string) string {
	added := addedLines(diff)
	if strings.TrimSpace(added) == "" {
		return ""
	}
	var findings []string

	changed := map[string]bool{}
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "+++ b/") {
			changed[strings.TrimPrefix(l, "+++ b/")] = true
		}
	}
	sources := map[string]string{} // the code under test: the Python files the task names
	var order []string
	for _, rel := range uniq(pyPath.FindAllString(task, -1)) {
		if changed[rel] {
			continue // the file being written
		}
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
			sources[rel] = string(b)
			order = append(order, rel)
		}
	}

	// Imports don't count as using a name (10-19: FIELDS imported, then
	// 'title' typed everywhere).
	used := importLine.ReplaceAllString(added, "")
	names, maybe := taskNames(task)
	for _, c := range maybe {
		for _, src := range sources {
			if defines(src, c) {
				names = append(names, c)
				break
			}
		}
	}
	var missing []string
	for _, n := range names {
		if !usesWord(used, n) {
			missing = append(missing, "`"+n+"`")
		}
	}
	if len(missing) > 0 {
		it := "it"
		if len(missing) > 1 {
			it = "them"
		}
		findings = append(findings, fmt.Sprintf("The task names %s, but your changes never use %s. Use what the task names, the way it says.", joinAnd(missing), it))
	}

	if want := listedCases(task); want > 0 {
		var tests []string
		for _, m := range testDef.FindAllStringSubmatch(added, -1) {
			tests = append(tests, m[1])
		}
		if len(tests) > 0 && len(tests) != want {
			findings = append(findings, fmt.Sprintf("The task lists %d cases, one test each, but your changes add %d tests (%s). Write exactly one test per listed case: no extras, no duplicates, none missing.", want, len(tests), strings.Join(tests, ", ")))
		}
	}

	for _, rel := range order {
		hits := hardCoded(used, constants(sources[rel]))
		vals := make([]string, 0, len(hits))
		for v := range hits {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		for _, v := range vals {
			shown := v
			if _, err := strconv.Atoi(v); err != nil {
				shown = "'" + v + "'"
			}
			findings = append(findings, fmt.Sprintf("Your changes type %s, but %s has %s = %s. Use the name.", shown, rel, joinOr(hits[v]), shown))
		}
	}

	if len(findings) == 0 {
		return ""
	}
	return "A quick check of your changes against the task, before review, found:\n\n- " + strings.Join(findings, "\n- ") +
		"\n\nFix these and run the tests again. If one doesn't apply, leave it and say why in your reply."
}

func joinAnd(items []string) string { return joinWith(items, "and") }
func joinOr(items []string) string  { return joinWith(items, "or") }

func joinWith(items []string, word string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + word + " " + items[len(items)-1]
}

func uniq(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

package away

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFile(path, text string) string {
	return "diff --git a/" + path + " b/" + path + "\nnew file mode 100644\n--- /dev/null\n+++ b/" + path + "\n@@ -0,0 +1 @@\n+" +
		strings.ReplaceAll(strings.TrimSuffix(text, "\n"), "\n", "\n+") + "\n"
}

// The names a task asks for, without its examples, paths and test target.
func TestTaskNames(t *testing.T) {
	task := "Add tests for consume_passkey_reauth_grant in src/view/webauthn.py, in a new file src/tests/security/test_x.py, " +
		"using Django's SimpleTestCase (no database). A stamp that isn't a date ('garbage_value') gives False. " +
		"Build the request with RequestFactory, keyed by _SESSION_REAUTH_GRANT_AT; use types.SimpleNamespace and " +
		"datetime.time.max; write out.txt. Run run_tests with the target src.tests.security.test_x until it passes."
	names, maybe := taskNames(task + " Take FIELDS from the module, over HTTP, with get_key(ip); entries are (timestamp, duration_ms) tuples.")
	if got := strings.Join(names, " "); got != "SimpleTestCase RequestFactory _SESSION_REAUTH_GRANT_AT SimpleNamespace get_key" {
		t.Fatalf("%q", got)
	}
	if got := strings.Join(maybe, " "); got != "consume_passkey_reauth_grant FIELDS HTTP duration_ms" {
		t.Fatalf("maybe: %q", got)
	}
}

func TestConstants(t *testing.T) {
	c := constants("MAX_LOGIN_ATTEMPTS = 5\nLOCKOUT_DURATION = 15 * 60  # seconds\nATTEMPT_WINDOW = 15 * 60\n" +
		"CACHE_KEY = 'perf_requests'\nAPI = 'x'\n_ = call(\nMIN_TOKEN_LENGTH = 32\nRATIO = 0.5\n")
	if strings.Join(c["900"], " ") != "LOCKOUT_DURATION ATTEMPT_WINDOW" || len(c["perf_requests"]) != 1 || len(c["32"]) != 1 || len(c) != 3 {
		t.Fatalf("%v", c)
	}
}

// 10-18's two slips, from the real attempts: both are caught, and the
// accepted versions pass untouched.
func TestPreReviewCatchesTheRealSlips(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src", "view"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "view", "login_view.py"), []byte("LOCKOUT_DURATION = 15 * 60\nATTEMPT_WINDOW = 15 * 60\n\ndef is_rate_limited(ip):\n    pass\n"), 0o644)

	limit := "Add tests for is_rate_limited in src/view/login_view.py, in a new file src/tests/security/test_rl.py, using Django's TestCase."
	bad := newFile("src/tests/security/test_rl.py", "from django.test import TestCase\nfrom src.view.login_view import is_rate_limited\nclass T(TestCase):\n    cache.set(key, value, 900)  # 15 minutes\n    is_rate_limited(ip)\n")
	got := preReview(limit, bad, dir)
	if !strings.Contains(got, "Your changes type 900, but src/view/login_view.py has LOCKOUT_DURATION or ATTEMPT_WINDOW = 900. Use the name.") {
		t.Fatalf("%q", got)
	}
	good := strings.ReplaceAll(bad, "900", "LOCKOUT_DURATION")
	if got := preReview(limit, good, dir); got != "" {
		t.Fatalf("flagged the fixed version: %q", got)
	}

	grant := "Add tests for consume_grant in src/view/webauthn.py, in a new file src/tests/security/test_g.py, using Django's SimpleTestCase. Build the request with RequestFactory."
	bad = newFile("src/tests/security/test_g.py", "from django.http import HttpRequest\nfrom django.test import SimpleTestCase\nclass T(SimpleTestCase):\n    request = HttpRequest()\n    consume_grant(request)\n")
	if got := preReview(grant, bad, dir); !strings.Contains(got, "The task names `RequestFactory`, but your changes never use it.") {
		t.Fatalf("%q", got)
	}
	good = strings.ReplaceAll(bad, "HttpRequest()", "RequestFactory().get('/')")
	if got := preReview(grant, good, dir); got != "" {
		t.Fatalf("flagged the fixed version: %q", got)
	}
}

func TestPreReviewLeavesNoChangesAlone(t *testing.T) {
	if got := preReview("Use RequestFactory.", "", t.TempDir()); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestTranscriptTools(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.json")
	// 10-18: a 4,096-token context; Qwen answered in prose and called nothing.
	os.WriteFile(p, []byte(`{"usage":{"input":2050},"items":[{"kind":"user"},{"kind":"note"},{"kind":"assistant"}]}`), 0o644)
	if calls, input := transcriptTools(p); calls != 0 || input != 2050 {
		t.Fatalf("%d %d", calls, input)
	}
	os.WriteFile(p, []byte(`{"usage":{"input":45267},"items":[{"kind":"user"},{"kind":"tool"},{"kind":"assistant"},{"kind":"tool"}]}`), 0o644)
	if calls, _ := transcriptTools(p); calls != 2 {
		t.Fatal(calls)
	}
	if calls, _ := transcriptTools(filepath.Join(dir, "missing.json")); calls != -1 {
		t.Fatal("a missing transcript counted as no tool calls")
	}
}

// A check finding goes back to Qwen before any review, without using up one
// of its rounds, and only once.
func TestCheckSendsItBackBeforeReview(t *testing.T) {
	steps := &fakeSteps{reviews: []string{`{"verdict":"pass","notes":"ok"}`}}
	steps.qwen = func(dir, task string) error {
		text := "request = HttpRequest()\n"
		if strings.Contains(task, "quick check") {
			text = "request = RequestFactory().get('/')\n"
		}
		return os.WriteFile(filepath.Join(dir, "test_grant.py"), []byte(text), 0o644)
	}
	s, _, _ := setup(t, steps)
	task, _ := s.Add("proj", "Write test_grant.py, building the request with RequestFactory.")
	s.SetMode(ModeOn)
	got := waitFor(t, s, task.ID, finished)
	var who []string
	for _, r := range got.Rounds {
		who = append(who, r.Who+":"+r.Verdict)
	}
	if strings.Join(who, " ") != "qwen: tests:pass check:revise qwen: tests:pass review:pass" || got.Verdict != "pass" {
		t.Fatalf("%v %+v", who, got)
	}
	if len(steps.claudePrompts) != 1 || !strings.Contains(steps.prompts[1], "`RequestFactory`") {
		t.Fatalf("reviews: %d, second prompt: %q", len(steps.claudePrompts), steps.prompts[1])
	}
}

func TestListedCases(t *testing.T) {
	task := "Add tests for f in a.py. Read the function first. Cover every case, one test each: 'a; b' gives 1; None gives ''; 0.5 gives 2.0. Use types.SimpleNamespace. Run run_tests."
	if n := listedCases(task); n != 3 {
		t.Fatal(n)
	}
	if n := listedCases("Fix the bug in a.py."); n != 0 {
		t.Fatal(n)
	}
}

// 10-19's two slips: 'title' where the task said to use FIELDS (which the
// module defines), and a sixth test for a five-case task.
func TestPreReviewFieldsAndCaseCount(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cnb_live.py"), []byte("FIELDS = (\n    'title',\n)\nSAMPLE_ONE_IN = 20\n\ndef force_release(rid, field):\n    pass\n"), 0o644)
	task := "Add tests for force_release in cnb_live.py, in a new file test_fr.py. Cover every case, one test each: no lock gives None; a lock gives its holder. " +
		"Take locks with acquire_lock, using field names from the module's FIELDS, over HTTP."
	bad := newFile("test_fr.py", "from cnb_live import FIELDS, acquire_lock\ndef test_none(self):\n    acquire_lock(1, 'title', a)\nforce_release(1, 'title')\nentry = (now, 200, '/p', 10, 20.0)\ndef test_holder(self):\n    pass\ndef test_extra(self):\n    pass\n")
	got := preReview(task, bad, dir)
	if !strings.Contains(got, "The task names `FIELDS`, but your changes never use it.") || strings.Contains(got, "HTTP") {
		t.Fatalf("%q", got)
	}
	if !strings.Contains(got, "The task lists 2 cases, one test each, but your changes add 3 tests (test_none, test_holder, test_extra).") {
		t.Fatalf("%q", got)
	}
	good := strings.ReplaceAll(strings.ReplaceAll(bad, "'title'", "FIELDS[0]"), "def test_extra(self):\n+    pass\n", "")
	if got := preReview(task, good, dir); got != "" {
		t.Fatalf("flagged the fixed version: %q", got)
	}
}

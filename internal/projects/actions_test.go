package projects

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// remoteFixture: a bare "GitHub" repo, the desktop's clone (in root, as
// "app"), and a second clone ("the Mac") that can push new commits.
func remoteFixture(t *testing.T) (root, desk, mac, bare string) {
	t.Helper()
	base := t.TempDir()
	bare = filepath.Join(base, "remote.git")
	git(t, base, "init", "-q", "--bare", "-b", "main", bare)
	seed := filepath.Join(base, "seed")
	git(t, base, "clone", "-q", bare, seed)
	git(t, seed, "config", "core.autocrlf", "false")
	write(t, filepath.Join(seed, "a.txt"), "one\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "Start")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	root = filepath.Join(base, "root")
	os.MkdirAll(root, 0o755)
	desk = filepath.Join(root, "app")
	git(t, base, "clone", "-q", bare, desk)
	mac = filepath.Join(base, "mac")
	git(t, base, "clone", "-q", bare, mac)
	for _, d := range []string{desk, mac} {
		git(t, d, "config", "user.name", "T")
		git(t, d, "config", "user.email", "t@example.com")
		git(t, d, "config", "core.autocrlf", "false")
	}
	return
}

func actHandler(roots []string, allowed bool) http.Handler {
	mux := http.NewServeMux()
	New(roots).Register(mux, func(*http.Request) bool { return allowed })
	return mux
}

func post(t *testing.T, h http.Handler, path, body string) (int, ActionResult) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	var res ActionResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Summary == "" {
		var e struct{ Error string }
		json.Unmarshal(rec.Body.Bytes(), &e)
		res.Summary = e.Error
	}
	return rec.Code, res
}

func headOf(t *testing.T, dir string) string {
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestButtonsNeedTheDashboard(t *testing.T) {
	root, _, _, _ := remoteFixture(t)
	if code, _ := post(t, actHandler([]string{root}, false), "/api/projects/app/git/fetch", ""); code != http.StatusForbidden {
		t.Fatalf("from elsewhere: %d", code)
	}
	if code, _ := post(t, actHandler([]string{root}, true), "/api/projects/app/git/reset", ""); code != http.StatusNotFound {
		t.Fatalf("unknown action: %d", code)
	}
}

func TestCommitThenPush(t *testing.T) {
	root, desk, _, bare := remoteFixture(t)
	h := actHandler([]string{root}, true)
	write(t, filepath.Join(desk, "a.txt"), "one\ntwo\n")
	write(t, filepath.Join(desk, ".env"), "TOKEN=secret\n")

	code, res := post(t, h, "/api/projects/app/git/commit", `{"message": "Add two"}`)
	if code != http.StatusConflict || !strings.Contains(res.Summary, ".env") {
		t.Fatalf("a secret file should stop the commit: %d %q", code, res.Summary)
	}
	if headOf(t, desk) != "Start" {
		t.Fatal("committed anyway")
	}
	os.Remove(filepath.Join(desk, ".env"))
	if code, res := post(t, h, "/api/projects/app/git/commit", `{"message": ""}`); code != http.StatusConflict || !strings.Contains(res.Summary, "message") {
		t.Fatalf("empty message: %d %q", code, res.Summary)
	}
	code, res = post(t, h, "/api/projects/app/git/commit", `{"message": "Add two"}`)
	if code != 200 || headOf(t, desk) != "Add two" || res.Project.Ahead != 1 || res.Project.Changed != 0 {
		t.Fatalf("commit: %d %+v", code, res)
	}
	code, res = post(t, h, "/api/projects/app/git/push", "")
	if code != 200 || !strings.Contains(res.Summary, "Pushed 1 commit to origin/main") || headOf(t, bare) != "Add two" || res.Project.Ahead != 0 {
		t.Fatalf("push: %d %+v (remote at %q)", code, res, headOf(t, bare))
	}
	if code, res := post(t, h, "/api/projects/app/git/push", ""); code != 200 || res.Summary != "Nothing to push." {
		t.Fatalf("second push: %d %q", code, res.Summary)
	}
}

func TestFetchThenPull(t *testing.T) {
	root, desk, mac, _ := remoteFixture(t)
	h := actHandler([]string{root}, true)
	write(t, filepath.Join(mac, "b.txt"), "from the mac\n")
	git(t, mac, "add", ".")
	git(t, mac, "commit", "-q", "-m", "From the Mac")
	git(t, mac, "push", "-q")

	code, res := post(t, h, "/api/projects/app/git/fetch", "")
	if code != 200 || res.Project.Behind != 1 || !strings.Contains(res.Summary, "1 new commit to pull") {
		t.Fatalf("fetch: %d %+v", code, res)
	}
	code, res = post(t, h, "/api/projects/app/git/pull", "")
	if code != 200 || headOf(t, desk) != "From the Mac" || res.Project.Behind != 0 {
		t.Fatalf("pull: %d %+v", code, res)
	}
}

func TestPullWontMergeDivergedBranches(t *testing.T) {
	root, desk, mac, _ := remoteFixture(t)
	h := actHandler([]string{root}, true)
	write(t, filepath.Join(mac, "m.txt"), "mac\n")
	git(t, mac, "add", ".")
	git(t, mac, "commit", "-q", "-m", "Mac side")
	git(t, mac, "push", "-q")
	write(t, filepath.Join(desk, "d.txt"), "desk\n")
	git(t, desk, "add", ".")
	git(t, desk, "commit", "-q", "-m", "Desk side")

	code, res := post(t, h, "/api/projects/app/git/pull", "")
	if code != http.StatusConflict || !strings.Contains(res.Summary, "both have new commits") || headOf(t, desk) != "Desk side" {
		t.Fatalf("diverged pull: %d %q (head %q)", code, res.Summary, headOf(t, desk))
	}
	code, res = post(t, h, "/api/projects/app/git/push", "")
	if code != http.StatusConflict || !strings.Contains(res.Summary, "Pull first") {
		t.Fatalf("rejected push: %d %q", code, res.Summary)
	}
}

func TestPushNeedsAnUpstream(t *testing.T) {
	root, desk, _, _ := remoteFixture(t)
	git(t, desk, "checkout", "-q", "-b", "local-only")
	write(t, filepath.Join(desk, "x.txt"), "x\n")
	git(t, desk, "add", ".")
	git(t, desk, "commit", "-q", "-m", "Local")
	code, res := post(t, actHandler([]string{root}, true), "/api/projects/app/git/push", "")
	if code != http.StatusConflict || !strings.Contains(res.Summary, "doesn't track a remote branch") {
		t.Fatalf("push without upstream: %d %q", code, res.Summary)
	}
}

func TestOneActionAtATime(t *testing.T) {
	var l locks
	if !l.take("a") || l.take("a") || !l.take("b") {
		t.Fatal("lock")
	}
	l.release("a")
	if !l.take("a") {
		t.Fatal("released lock not reusable")
	}
}

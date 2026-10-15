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

func TestParseBranches(t *testing.T) {
	out := strings.Join([]string{
		"refs/heads/main\torigin/main\t2026-10-10T10:00:00-05:00",
		"refs/heads/old\t\t2026-01-01T10:00:00-05:00",
		"refs/heads/wip\t\t2026-10-12T10:00:00-05:00",
		"refs/remotes/origin/HEAD\t\t2026-10-10T10:00:00-05:00",
		"refs/remotes/origin/main\t\t2026-10-10T10:00:00-05:00",
		"refs/remotes/origin/feature\t\t2026-10-14T10:00:00-05:00",
		"refs/remotes/fork/feature\t\t2026-10-13T10:00:00-05:00",
	}, "\n")
	got := parseBranches(out, "main")
	var names []string
	for _, b := range got {
		names = append(names, b.Name+"@"+b.Remote)
	}
	// The current branch first, then the other local ones (newest first), then
	// what only the remote has (once, even if two remotes have it; no HEAD).
	if strings.Join(names, " ") != "main@ wip@ old@ feature@origin" {
		t.Fatalf("got %v", names)
	}
	if !got[0].Current || got[0].Upstream != "origin/main" || got[3].Upstream != "origin/feature" {
		t.Fatalf("details: %+v", got)
	}
}

func getBranches(t *testing.T, h http.Handler) (string, []Branch) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/projects/app/branches", nil))
	var res struct {
		Current  string   `json:"current"`
		Branches []Branch `json:"branches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != 200 {
		t.Fatalf("branches: %d %s", rec.Code, rec.Body.String())
	}
	return res.Current, res.Branches
}

func currentBranch(t *testing.T, dir string) string {
	out, err := exec.Command("git", "-C", dir, "branch", "--show-current").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestSwitchBranch(t *testing.T) {
	root, desk, mac, _ := remoteFixture(t)
	h := actHandler([]string{root}, true)

	// The Mac pushes a new branch; the desktop hears about it on fetch.
	git(t, mac, "switch", "-q", "-c", "feature")
	write(t, filepath.Join(mac, "f.txt"), "f\n")
	git(t, mac, "add", ".")
	git(t, mac, "commit", "-q", "-m", "Feature")
	git(t, mac, "push", "-q", "origin", "feature")
	if code, _ := post(t, h, "/api/projects/app/git/fetch", ""); code != 200 {
		t.Fatal("fetch")
	}
	cur, list := getBranches(t, h)
	if cur != "main" || len(list) != 2 || list[1].Name != "feature" || list[1].Remote != "origin" {
		t.Fatalf("listed: %q %+v", cur, list)
	}

	// Uncommitted work: refused, and nothing moves.
	write(t, filepath.Join(desk, "a.txt"), "changed\n")
	code, res := post(t, h, "/api/projects/app/git/switch", `{"branch": "feature"}`)
	if code != http.StatusConflict || !strings.Contains(res.Summary, "uncommitted") || currentBranch(t, desk) != "main" {
		t.Fatalf("dirty: %d %q", code, res.Summary)
	}
	git(t, desk, "checkout", "--", "a.txt")

	// Only listed branches, never something that reads like a flag.
	for _, bad := range []string{"nope", "--orphan", ""} {
		if code, res := post(t, h, "/api/projects/app/git/switch", `{"branch": "`+bad+`"}`); code != http.StatusConflict {
			t.Fatalf("%q: %d %q", bad, code, res.Summary)
		}
	}

	// A remote-only branch: created here, tracking the remote.
	code, res = post(t, h, "/api/projects/app/git/switch", `{"branch": "feature"}`)
	if code != 200 || currentBranch(t, desk) != "feature" || res.Project.Branch != "feature" || res.Project.Upstream != "origin/feature" {
		t.Fatalf("switch to feature: %d %+v", code, res)
	}
	if _, err := os.Stat(filepath.Join(desk, "f.txt")); err != nil {
		t.Fatal("feature's file isn't there")
	}
	// And back to a local one.
	if code, res := post(t, h, "/api/projects/app/git/switch", `{"branch": "main"}`); code != 200 || currentBranch(t, desk) != "main" || res.Summary != "Switched to main." {
		t.Fatalf("back to main: %d %q", code, res.Summary)
	}
	if code, res := post(t, h, "/api/projects/app/git/switch", `{"branch": "main"}`); code != 200 || res.Summary != "Already on main." {
		t.Fatalf("again: %d %q", code, res.Summary)
	}
	// Like the other buttons, only from the dashboard.
	if code, _ := post(t, actHandler([]string{root}, false), "/api/projects/app/git/switch", `{"branch": "feature"}`); code != http.StatusForbidden {
		t.Fatalf("from elsewhere: %d", code)
	}
}

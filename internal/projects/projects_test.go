package projects

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A root with one repo ("shop") that has a commit and then some changes, a
// plain folder (not listed), and a secret file outside every project.
func fixture(t *testing.T) (root, repo, outside string) {
	root = t.TempDir()
	outside = t.TempDir()
	write(t, filepath.Join(outside, "private.txt"), "not yours")
	repo = filepath.Join(root, "shop")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "core.autocrlf", "false")
	write(t, filepath.Join(repo, "app.py"), "def total(items):\n    return sum(items)\n")
	write(t, filepath.Join(repo, "old.txt"), "bye\n")
	write(t, filepath.Join(repo, "keep.md"), "hello\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "First")
	// Changes: an edit, a deletion, a new file in a new folder, a secret.
	write(t, filepath.Join(repo, "app.py"), "def total(items, tax=0):\n    return sum(items) * (1 + tax)\n")
	os.Remove(filepath.Join(repo, "old.txt"))
	write(t, filepath.Join(repo, "src", "new.py"), "print('new')\n")
	write(t, filepath.Join(repo, ".env"), "API_KEY=sk-secret\n")
	os.MkdirAll(filepath.Join(root, "notes"), 0o755) // not a repo
	if runtime.GOOS != "windows" {
		os.Symlink(outside, filepath.Join(repo, "escape"))
	}
	return root, repo, outside
}

func get(t *testing.T, h http.Handler, path string, into any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if into != nil {
		json.Unmarshal(rec.Body.Bytes(), into)
	}
	return rec.Code
}

func handler(roots ...string) http.Handler {
	mux := http.NewServeMux()
	New(roots).Register(mux, nil)
	return mux
}

func TestListsRepositoriesWithTheirChanges(t *testing.T) {
	root, _, _ := fixture(t)
	var res struct{ Projects []Project }
	get(t, handler(root), "/api/projects", &res)
	if len(res.Projects) != 1 || res.Projects[0].ID != "shop" {
		t.Fatalf("projects: %+v", res.Projects)
	}
	p := res.Projects[0]
	// app.py, old.txt, src/new.py, .env (+ the escape symlink on Unix)
	if p.Branch != "main" || p.Changed < 4 || p.LastCommit == nil || p.LastCommit.Subject != "First" {
		t.Fatalf("project: %+v", p)
	}
}

func TestChangesShowDiffsAndHideSecrets(t *testing.T) {
	root, _, _ := fixture(t)
	var res struct{ Files []FileChange }
	if code := get(t, handler(root), "/api/projects/shop/changes", &res); code != 200 {
		t.Fatalf("code %d", code)
	}
	by := map[string]FileChange{}
	for _, f := range res.Files {
		by[f.Path] = f
	}
	if f := by["app.py"]; f.Kind != "modified" || !strings.Contains(f.Diff, "+def total(items, tax=0):") || !strings.Contains(f.Diff, "-def total(items):") || !strings.HasPrefix(f.Diff, "--- a/app.py") {
		t.Errorf("app.py: %+v", f)
	}
	if f := by["old.txt"]; f.Kind != "deleted" || !strings.Contains(f.Diff, "-bye") {
		t.Errorf("old.txt: %+v", f)
	}
	if f := by["src/new.py"]; f.Kind != "new" || f.Diff != "@@ -0,0 +1,1 @@\n+print('new')" {
		t.Errorf("src/new.py: %+v", f)
	}
	if f := by[".env"]; f.Diff != "" || !strings.Contains(f.Note, "keys or passwords") {
		t.Errorf(".env leaked: %+v", f)
	}
	body, _ := json.Marshal(res)
	if strings.Contains(string(body), "sk-secret") {
		t.Fatal("the secret's value was sent")
	}
}

func TestTreeAndFile(t *testing.T) {
	root, _, _ := fixture(t)
	h := handler(root)
	var tree struct{ Entries []Entry }
	get(t, h, "/api/projects/shop/tree", &tree)
	names := map[string]Entry{}
	for _, e := range tree.Entries {
		names[e.Name] = e
	}
	if _, ok := names[".git"]; ok {
		t.Error(".git listed")
	}
	if names["src"].Status != "changed" || !names["src"].Dir || names["app.py"].Status != " M" || names["keep.md"].Status != "" {
		t.Errorf("tree: %+v", tree.Entries)
	}
	var f struct{ Text, Note, Status string }
	get(t, h, "/api/projects/shop/file?path=src/new.py", &f)
	if f.Text != "print('new')\n" || f.Status != "??" {
		t.Errorf("file: %+v", f)
	}
	f.Text = ""
	get(t, h, "/api/projects/shop/file?path=.env", &f)
	if f.Text != "" || f.Note == "" {
		t.Errorf(".env content sent: %+v", f)
	}
}

func TestPathsCantLeaveTheProject(t *testing.T) {
	root, repo, outside := fixture(t)
	h := handler(root)
	bad := []string{"../notes", "..%2F..%2Fetc", "/etc/passwd", "C:/Windows/win.ini", ".git/config", "src/../../shop2", outside + "/private.txt"}
	if runtime.GOOS != "windows" {
		bad = append(bad, "escape/private.txt", "escape")
	}
	for _, p := range bad {
		var f struct{ Text string }
		code := get(t, h, "/api/projects/shop/file?path="+p, &f)
		if code == 200 || f.Text != "" {
			t.Errorf("file %q: %d %q", p, code, f.Text)
		}
		if code := get(t, h, "/api/projects/shop/tree?path="+p, nil); code == 200 {
			t.Errorf("tree %q: %d", p, code)
		}
	}
	if code := get(t, h, "/api/projects/nope/changes", nil); code != 404 {
		t.Errorf("unknown project: %d", code)
	}
	_ = repo
}

func TestSplitDiffKeysByPath(t *testing.T) {
	out := "diff --git a/a.py b/a.py\nindex 1..2 100644\n--- a/a.py\n+++ b/a.py\n@@ -1 +1 @@\n-x\n+y\n" +
		"diff --git a/img.png b/img.png\nindex 1..2 100644\nBinary files a/img.png and b/img.png differ\n" +
		"diff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n"
	d := splitDiff(out)
	if d["a.py"] != "--- a/a.py\n+++ b/a.py\n@@ -1 +1 @@\n-x\n+y" {
		t.Errorf("a.py: %q", d["a.py"])
	}
	if !strings.HasPrefix(d["img.png"], "Binary files") || !strings.HasPrefix(d["gone.txt"], "--- a/gone.txt") {
		t.Errorf("got %q", d)
	}
}

func TestListingIsCachedBriefly(t *testing.T) {
	root, _, _ := fixture(t)
	b := New([]string{root})
	calls := 0
	b.git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		calls++
		return runGit(ctx, dir, args...)
	}
	b.Projects(context.Background())
	first := calls
	b.Projects(context.Background())
	if calls != first {
		t.Errorf("git ran again within the cache window (%d -> %d)", first, calls)
	}
}

func TestACancelledRequestDoesntSpoilTheList(t *testing.T) {
	root, _, _ := fixture(t)
	b := New([]string{root})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the browser went away before the listing ran
	list := b.Projects(ctx)
	if len(list) != 1 || list[0].LastCommit == nil || list[0].Branch != "main" {
		t.Fatalf("listing with a cancelled request: %+v", list)
	}
}

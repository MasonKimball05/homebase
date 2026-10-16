package projects

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef"

func remoteHandler(t *testing.T, root string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	New([]string{root}).EnableRemote(testToken).Register(mux, func(r *http.Request) bool { return r.Header.Get("X-Homebase") == "1" })
	return mux
}

func call(t *testing.T, h http.Handler, method, path, body string, token bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token {
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Homebase", "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestRemoteIsOffWithoutAToken(t *testing.T) {
	root, _, _, _ := remoteFixture(t)
	mux := http.NewServeMux()
	New([]string{root}).Register(mux, func(*http.Request) bool { return true })
	if code, _ := call(t, mux, "GET", "/api/remote/projects", "", true); code != http.StatusNotFound {
		t.Fatalf("remote routes exist without remote_edit: %d", code)
	}
}

func TestRemoteNeedsTheToken(t *testing.T) {
	root, _, _, _ := remoteFixture(t)
	h := remoteHandler(t, root)
	if code, _ := call(t, h, "GET", "/api/remote/app/read?path=a.txt", "", false); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	req := httptest.NewRequest("GET", "/api/remote/app/read?path=a.txt", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("0", len(testToken)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	// A change needs the CSRF header as well as the token.
	req = httptest.NewRequest("PUT", "/api/remote/app/write?path=a.txt", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("write without X-Homebase: %d", rec.Code)
	}
}

func TestRemoteEditsAFolder(t *testing.T) {
	root, desk, _, _ := remoteFixture(t)
	h := remoteHandler(t, root)

	code, body := call(t, h, "GET", "/api/remote/projects", "", true)
	if code != 200 || !strings.Contains(body, `"id":"app"`) {
		t.Fatalf("projects: %d %s", code, body)
	}
	code, body = call(t, h, "GET", "/api/remote/app/read?path=a.txt", "", true)
	if code != 200 || !strings.Contains(body, `"text":"one\n"`) {
		t.Fatalf("read: %d %s", code, body)
	}
	if code, body = call(t, h, "PUT", "/api/remote/app/write?path=src/new.go", "package x\n", true); code != 200 {
		t.Fatalf("write: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(desk, "src", "new.go")); string(b) != "package x\n" {
		t.Fatalf("written: %q", b)
	}
	if _, err := os.Stat(filepath.Join(desk, "src", "new.go.homebase-tmp")); err == nil {
		t.Fatal("left the temp file behind")
	}

	code, body = call(t, h, "GET", "/api/remote/app/list?path=src", "", true)
	var entries []RemoteEntry
	json.Unmarshal([]byte(body), &entries)
	if code != 200 || len(entries) != 1 || entries[0].Path != "src/new.go" || entries[0].IsDir {
		t.Fatalf("list: %d %s", code, body)
	}
	_, body = call(t, h, "GET", "/api/remote/app/list", "", true)
	if strings.Contains(body, `".git"`) {
		t.Fatalf(".git listed: %s", body)
	}
	_, body = call(t, h, "GET", "/api/remote/app/files", "", true)
	if !strings.Contains(body, "src/new.go") || strings.Contains(body, ".git/") {
		t.Fatalf("files: %s", body)
	}

	if code, _ = call(t, h, "POST", "/api/remote/app/create?path=docs&dir=1", "", true); code != 200 {
		t.Fatalf("mkdir: %d", code)
	}
	if code, _ = call(t, h, "POST", "/api/remote/app/create?path=docs", "", true); code != http.StatusConflict {
		t.Fatalf("create over existing: %d", code)
	}
	if code, _ = call(t, h, "POST", "/api/remote/app/rename?from=src/new.go&to=docs/moved.go", "", true); code != 200 {
		t.Fatalf("rename: %d", code)
	}
	if _, err := os.Stat(filepath.Join(desk, "docs", "moved.go")); err != nil {
		t.Fatal("not moved")
	}

	_, body = call(t, h, "GET", "/api/remote/app/search?q=PACKAGE", "", true)
	if !strings.Contains(body, `"path":"docs/moved.go"`) || !strings.Contains(body, `"line":1`) {
		t.Fatalf("search: %s", body)
	}
	_, body = call(t, h, "GET", "/api/remote/app/search?q=PACKAGE&case=1", "", true)
	if strings.Contains(body, "moved.go") {
		t.Fatalf("case-sensitive search matched: %s", body)
	}
	if code, _ = call(t, h, "GET", "/api/remote/app/search?q=(&regex=1", "", true); code != http.StatusBadRequest {
		t.Fatalf("bad regex: %d", code)
	}

	held := t.TempDir()
	old := HoldingDir
	HoldingDir = func() string { return held }
	defer func() { HoldingDir = old }()
	if code, body = call(t, h, "POST", "/api/remote/app/delete?path=docs/moved.go", "", true); code != 200 {
		t.Fatalf("delete: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(desk, "docs", "moved.go")); err == nil {
		t.Fatal("still there")
	}
	if !strings.Contains(body, held) {
		t.Fatalf("not moved to the holding folder: %s", body)
	}
	if code, _ = call(t, h, "POST", "/api/remote/app/delete?path=", "", true); code != http.StatusBadRequest {
		t.Fatalf("deleting the project itself: %d", code)
	}
}

func TestRemoteStaysInsideTheProject(t *testing.T) {
	root, desk, _, _ := remoteFixture(t)
	h := remoteHandler(t, root)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)
	if err := os.Symlink(outside, filepath.Join(desk, "link")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/remote/app/read?path=../app/a.txt"},
		{"GET", "/api/remote/app/read?path=link/secret.txt"},
		{"GET", "/api/remote/app/read?path=.git/config"},
		{"PUT", "/api/remote/app/write?path=../escape.txt"},
		{"PUT", "/api/remote/app/write?path=link/new.txt"},
		{"PUT", "/api/remote/app/write?path=.git/hooks/pre-commit"},
		{"PUT", "/api/remote/app/write?path=/etc/x"},
		{"PUT", "/api/remote/app/write?path=C:/x"},
		{"POST", "/api/remote/app/create?path=link/x"},
		{"POST", "/api/remote/app/rename?from=a.txt&to=../a.txt"},
		{"POST", "/api/remote/app/rename?from=a.txt&to=link/a.txt"},
		{"POST", "/api/remote/app/delete?path=link/secret.txt"},
		{"POST", "/api/remote/app/delete?path=.git"},
	} {
		if code, body := call(t, h, c.method, c.path, "x", true); code < 400 {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("wrote outside the project")
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatal("deleted outside the project")
	}
	if _, err := os.Stat(filepath.Join(desk, "a.txt")); err != nil {
		t.Fatal("a.txt moved out")
	}
}

func TestTokenFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "remote-token.txt")
	a, err := LoadOrCreateToken(p)
	if err != nil || len(a) != 64 {
		t.Fatalf("%q %v", a, err)
	}
	b, _ := LoadOrCreateToken(p)
	if a != b {
		t.Fatal("token changed on the second start")
	}
}

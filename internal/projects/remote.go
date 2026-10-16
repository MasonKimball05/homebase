package projects

// Remote editing: Loom on the Mac opens a project on this PC and works on its
// files (list, read, write, create, rename, delete, search). Off unless
// "projects": {"remote_edit": true} is set; then every route needs the token
// in remote-token.txt next to homebase.json (Authorization: Bearer <token>),
// and the ones that change something need the dashboard's CSRF check too.
// Paths stay inside their project (the same checks as the read-only pages),
// and a deleted file goes to a holding folder in %TEMP%, not away for good.
//
//	GET  /api/remote/projects
//	GET  /api/remote/{id}/list?path=      one folder
//	GET  /api/remote/{id}/files           every file (for Go to File)
//	GET  /api/remote/{id}/read?path=
//	PUT  /api/remote/{id}/write?path=     body: the new text
//	POST /api/remote/{id}/create?path=&dir=1
//	POST /api/remote/{id}/rename?from=&to=
//	POST /api/remote/{id}/delete?path=
//	GET  /api/remote/{id}/search?q=&regex=1&case=1

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	remoteMaxRead   = 5 << 20
	remoteMaxWrite  = 10 << 20
	remoteMaxFiles  = 20000
	remoteMaxHits   = 2000
	remoteMaxSearch = 2 << 20 // files bigger than this aren't searched
)

var skipDirs = map[string]bool{".git": true, "node_modules": true, "target": true, "dist": true, "__pycache__": true, ".venv": true, "venv": true, ".next": true}

// LoadOrCreateToken reads the token file, or writes a new random token to it.
func LoadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// EnableRemote turns the remote routes on, with this token.
func (b *Browser) EnableRemote(token string) *Browser {
	b.remoteToken = token
	return b
}

func (b *Browser) registerRemote(mux *http.ServeMux) {
	if b.remoteToken == "" {
		return
	}
	mux.HandleFunc("GET /api/remote/projects", b.remote(false, b.remoteProjects))
	mux.HandleFunc("GET /api/remote/{id}/list", b.remote(false, b.remoteList))
	mux.HandleFunc("GET /api/remote/{id}/files", b.remote(false, b.remoteFiles))
	mux.HandleFunc("GET /api/remote/{id}/read", b.remote(false, b.remoteRead))
	mux.HandleFunc("PUT /api/remote/{id}/write", b.remote(true, b.remoteWrite))
	mux.HandleFunc("POST /api/remote/{id}/create", b.remote(true, b.remoteCreate))
	mux.HandleFunc("POST /api/remote/{id}/rename", b.remote(true, b.remoteRename))
	mux.HandleFunc("POST /api/remote/{id}/delete", b.remote(true, b.remoteDelete))
	mux.HandleFunc("GET /api/remote/{id}/search", b.remote(false, b.remoteSearch))
}

// remote wraps a handler with the token check (and the CSRF check for changes).
func (b *Browser) remote(changes bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sent := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(sent), []byte(b.remoteToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "wrong or missing remote token (remote-token.txt next to homebase.json)")
			return
		}
		if changes && (b.allowed == nil || !b.allowed(r)) {
			writeError(w, http.StatusForbidden, "request not from Loom or the dashboard")
			return
		}
		h(w, r)
	}
}

func (b *Browser) remoteProject(w http.ResponseWriter, r *http.Request) (Project, bool) {
	// Just find the folder: no git status in every repo on each file request.
	for _, p := range b.discover() {
		if p.ID == r.PathValue("id") {
			return p, true
		}
	}
	writeError(w, http.StatusNotFound, "no such project")
	return Project{}, false
}

func (b *Browser) remoteProjects(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Path   string `json:"path"`
		Branch string `json:"branch,omitempty"`
	}
	out := []item{}
	for _, p := range b.Projects(r.Context()) {
		out = append(out, item{p.ID, p.Name, p.path, p.Branch})
	}
	writeJSON(w, http.StatusOK, out)
}

// RemoteEntry matches Loom's api.Entry (path is relative to the project).
type RemoteEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

func (b *Browser) remoteList(w http.ResponseWriter, r *http.Request) {
	p, ok := b.remoteProject(w, r)
	if !ok {
		return
	}
	full, rel, err := resolve(p.path, r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := os.ReadDir(full)
	if err != nil {
		writeError(w, http.StatusBadRequest, "not a folder")
		return
	}
	out := []RemoteEntry{}
	for _, it := range items {
		if it.Name() == ".git" {
			continue
		}
		dir := it.IsDir() || (it.Type()&fs.ModeSymlink != 0 && isDir(full, it.Name()))
		out = append(out, RemoteEntry{Name: it.Name(), Path: joinRel(rel, it.Name()), IsDir: dir})
	}
	writeJSON(w, http.StatusOK, out)
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func (b *Browser) remoteFiles(w http.ResponseWriter, r *http.Request) {
	p, ok := b.remoteProject(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, walkFiles(p.path, remoteMaxFiles))
}

func walkFiles(root string, max int) []string {
	out := []string{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= max {
			return filepath.SkipAll
		}
		if rel, err := filepath.Rel(root, path); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func (b *Browser) remoteRead(w http.ResponseWriter, r *http.Request) {
	p, ok := b.remoteProject(w, r)
	if !ok {
		return
	}
	full, _, err := resolve(p.path, r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "not a file")
		return
	}
	// The same shape as Loom's own read_file: big and binary files open read-only with a note.
	type opened struct {
		Text     string  `json:"text"`
		ReadOnly bool    `json:"readOnly"`
		Note     *string `json:"note"`
	}
	note := func(s string) *string { return &s }
	if info.Size() > remoteMaxRead {
		writeJSON(w, http.StatusOK, opened{ReadOnly: true, Note: note("Too large to open over the network (over 5 MB).")})
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "couldn't read it")
		return
	}
	if isBinary(data) {
		writeJSON(w, http.StatusOK, opened{ReadOnly: true, Note: note("A binary file.")})
		return
	}
	writeJSON(w, http.StatusOK, opened{Text: string(data)})
}

// resolveNew is resolve for a path that may not exist yet: its nearest
// existing folder must be inside the project.
func resolveNew(project, rel string) (string, string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, ":") {
		return "", "", errOutside
	}
	var parts []string
	for _, seg := range strings.Split(rel, "/") {
		switch {
		case seg == "" || seg == ".":
			continue
		case seg == ".." || strings.EqualFold(seg, ".git"):
			return "", "", errOutside
		}
		parts = append(parts, seg)
	}
	if len(parts) == 0 {
		return "", "", errOutside
	}
	clean := strings.Join(parts, "/")
	full := filepath.Join(project, filepath.FromSlash(clean))
	realRoot, err := filepath.EvalSymlinks(project)
	if err != nil {
		return "", "", err
	}
	at := full
	for {
		if real, err := filepath.EvalSymlinks(at); err == nil {
			if !within(realRoot, real) && real != realRoot {
				return "", "", errOutside
			}
			break
		}
		parent := filepath.Dir(at)
		if parent == at {
			return "", "", errOutside
		}
		at = parent
	}
	return full, clean, nil
}

func (b *Browser) remoteWrite(w http.ResponseWriter, r *http.Request) {
	p, ok := b.remoteProject(w, r)
	if !ok {
		return
	}
	full, _, err := resolveNew(p.path, r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, remoteMaxWrite))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too big to save over the network (10 MB at most)")
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Write then rename, so a dropped connection never leaves half a file.
	tmp := full + ".homebase-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (b *Browser) remoteCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := b.remoteProject(w, r)
	if !ok {
		return
	}
	full, _, err := resolveNew(p.path, r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(full); err == nil {
		writeError(w, http.StatusConflict, "it already exists")
		return
	}
	if r.URL.Query().Get("dir") == "1" {
		err = os.MkdirAll(full, 0o755)
	} else if err = os.MkdirAll(filepath.Dir(full), 0o755); err == nil {
		err = os.WriteFile(full, nil, 0o644)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (b *Browser) remoteRename(w http.ResponseWriter, r *http.Request) {
	p, ok := b.remoteProject(w, r)
	if !ok {
		return
	}
	from, _, err := resolve(p.path, r.URL.Query().Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	to, _, err := resolveNew(p.path, r.URL.Query().Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(to); err == nil {
		writeError(w, http.StatusConflict, "something with that name already exists")
		return
	}
	if err := os.Rename(from, to); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// HoldingDir is where deleted files go (os.TempDir()/homebase-deleted/<time>/<project>/…).
var HoldingDir = func() string { return filepath.Join(os.TempDir(), "homebase-deleted") }

func (b *Browser) remoteDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := b.remoteProject(w, r)
	if !ok {
		return
	}
	full, rel, err := resolve(p.path, r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		writeError(w, http.StatusBadRequest, "can't delete that")
		return
	}
	stamp := filepath.Join(time.Now().Format("20060102-150405.000"), p.ID, filepath.FromSlash(rel))
	// %TEMP% first; a project on another drive can't be renamed there, so
	// then a .homebase-deleted folder beside the project.
	var dest string
	var err2 error
	for _, holding := range []string{HoldingDir(), filepath.Join(filepath.Dir(p.path), ".homebase-deleted")} {
		dest = filepath.Join(holding, stamp)
		if err2 = os.MkdirAll(filepath.Dir(dest), 0o755); err2 == nil {
			if err2 = os.Rename(full, dest); err2 == nil {
				break
			}
		}
	}
	if err2 != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("couldn't move it aside: %v", err2))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "movedTo": dest})
}

// RemoteHit matches Loom's api.SearchHit (byte offsets in the line).
type RemoteHit struct {
	Path  string `json:"path"`
	Line  int    `json:"line"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

func (b *Browser) remoteSearch(w http.ResponseWriter, r *http.Request) {
	p, ok := b.remoteProject(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	hits, truncated, err := searchFiles(p.path, q.Get("q"), q.Get("regex") == "1", q.Get("case") == "1")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits, "truncated": truncated})
}

func searchFiles(root, query string, isRegex, caseSensitive bool) ([]RemoteHit, bool, error) {
	if query == "" {
		return []RemoteHit{}, false, nil
	}
	pattern := query
	if !isRegex {
		pattern = regexp.QuoteMeta(query)
	}
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, errors.New("not a valid regular expression")
	}
	hits := []RemoteHit{}
	for _, rel := range walkFiles(root, remoteMaxFiles) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(full)
		if err != nil || info.Size() > remoteMaxSearch {
			continue
		}
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		head := make([]byte, 8000)
		n, _ := f.Read(head)
		if isBinary(head[:n]) {
			f.Close()
			continue
		}
		f.Seek(0, io.SeekStart)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		line := 0
		for sc.Scan() {
			line++
			text := sc.Text()
			if loc := re.FindStringIndex(text); loc != nil {
				hits = append(hits, RemoteHit{Path: rel, Line: line, Start: loc[0], End: loc[1], Text: cutLine(text)})
				if len(hits) >= remoteMaxHits {
					f.Close()
					return hits, true, nil
				}
			}
		}
		f.Close()
	}
	return hits, false, nil
}

func cutLine(s string) string {
	if len(s) > 400 {
		return s[:400]
	}
	return s
}

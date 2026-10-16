// Package projects lets you look at the git projects on this PC from another
// machine: which ones changed lately, what changed (diffs since the last
// commit), and the files themselves. It's for checking on work done here
// while you're away, e.g. what Qwen edited through Loom on the desktop.
//
// Looking is read-only: those routes are GETs and write nothing. The only
// exceptions are four git buttons (fetch, pull, push, commit) in actions.go,
// in fixed forms and behind the dashboard's CSRF check. Also:
//
//   - only git repositories directly inside the configured roots are listed,
//     and a path can't leave its project ("..", absolute paths, drive
//     letters and symlinks out are refused; .git itself is off limits)
//   - files that usually hold secrets (.env files, private keys, homebase's
//     own *.env files) are listed but their contents are never sent
//   - big or binary files are described, not sent
//
// Routes:
//
//	GET  /api/projects                    the projects, most recently changed first
//	GET  /api/projects/{id}/changes       uncommitted changes, as unified diffs
//	GET  /api/projects/{id}/tree?path=    one folder's entries, with git status
//	GET  /api/projects/{id}/file?path=    one file's text
//	POST /api/projects/{id}/git/{action}  fetch, pull, push or commit (actions.go)
package projects

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Browser serves the routes above for the repos under its roots.
type Browser struct {
	roots     []string
	git       gitRunner                // reads (status, log, diff)
	gitAction gitRunner                // the buttons (fetch, pull, push, commit): see actions.go
	allowed   func(*http.Request) bool // is a button press from the dashboard itself?
	locks     locks
	// Remote editing from Loom (remote.go); empty: off.
	remoteToken string

	mu       sync.Mutex
	cached   []Project
	cachedAt time.Time
}

// Project is one repository, as listed.
type Project struct {
	ID         string    `json:"id"`   // the folder name (made unique across roots)
	Name       string    `json:"name"` // the folder name
	Root       string    `json:"root"` // which configured root it's in
	Branch     string    `json:"branch,omitempty"`
	Upstream   string    `json:"upstream,omitempty"` // what it pushes to and pulls from, e.g. origin/main
	Ahead      int       `json:"ahead"`              // commits to push
	Behind     int       `json:"behind"`             // commits to pull (as of the last fetch)
	Gone       bool      `json:"gone,omitempty"`     // the upstream branch no longer exists
	LastFetch  time.Time `json:"last_fetch,omitzero"`
	Changed    int       `json:"changed"`              // files with uncommitted changes
	LastChange time.Time `json:"last_change,omitzero"` // newest changed file, else the last commit
	LastCommit *Commit   `json:"last_commit,omitempty"`
	path       string
}

type Commit struct {
	Hash    string    `json:"hash"`
	Subject string    `json:"subject"`
	When    time.Time `json:"when"`
}

// listFor is how long a listing is reused: it runs git in every repo.
const listFor = 4 * time.Second

func New(roots []string) *Browser {
	return &Browser{roots: roots, git: runGit, gitAction: runGitAction}
}

// Register adds the routes. The GETs change nothing; the git buttons
// (POST, actions.go) need allowed(r): the dashboard's own CSRF check.
func (b *Browser) Register(mux *http.ServeMux, allowed func(*http.Request) bool) {
	b.allowed = allowed
	mux.HandleFunc("POST /api/projects/{id}/git/{action}", b.act)
	mux.HandleFunc("GET /api/projects", b.list)
	mux.HandleFunc("GET /api/projects/{id}/changes", b.changes)
	mux.HandleFunc("GET /api/projects/{id}/branches", b.branches)
	mux.HandleFunc("GET /api/projects/{id}/tree", b.tree)
	mux.HandleFunc("GET /api/projects/{id}/file", b.file)
	b.registerRemote(mux)
}

// Projects lists the repos under the roots (cached briefly). The listing
// runs to the end even if the request that asked for it goes away: it's
// cached and shared, so a half-finished one must never be.
func (b *Browser) Projects(ctx context.Context) []Project {
	ctx = context.WithoutCancel(ctx)
	b.mu.Lock()
	if b.cached != nil && time.Since(b.cachedAt) < listFor {
		out := b.cached
		b.mu.Unlock()
		return out
	}
	b.mu.Unlock()

	found := b.discover()

	// git status in every repo: a few at a time, they're independent.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range found {
		wg.Add(1)
		go func(p *Project) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			b.describe(ctx, p)
		}(&found[i])
	}
	wg.Wait()
	sort.SliceStable(found, func(i, j int) bool { return found[i].LastChange.After(found[j].LastChange) })

	b.mu.Lock()
	b.cached, b.cachedAt = found, time.Now()
	b.mu.Unlock()
	return found
}

// Path is where a listed project lives (for internal/away), without asking git anything.
func (b *Browser) Path(id string) (string, bool) {
	for _, p := range b.discover() {
		if p.ID == id {
			return p.path, true
		}
	}
	return "", false
}

// discover finds the repos under the roots, without asking git anything.
func (b *Browser) discover() []Project {
	var found []Project
	taken := map[string]int{}
	for _, root := range b.roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
				continue // only git repositories
			}
			id := e.Name()
			if n := taken[id]; n > 0 {
				id = e.Name() + "~" + strconv.Itoa(n+1)
			}
			taken[e.Name()]++
			found = append(found, Project{ID: id, Name: e.Name(), Root: root, path: dir})
		}
	}
	return found
}

func (b *Browser) describe(ctx context.Context, p *Project) {
	st, err := b.status(ctx, p.path)
	if err == nil {
		p.Branch, p.Changed = st.branch, len(st.files)
		p.Upstream, p.Ahead, p.Behind, p.Gone = st.upstream, st.ahead, st.behind, st.gone
		if info, err := os.Stat(filepath.Join(p.path, ".git", "FETCH_HEAD")); err == nil {
			p.LastFetch = info.ModTime() // "behind" is only as fresh as this
		}
		for rel := range st.files {
			if info, err := os.Stat(filepath.Join(p.path, filepath.FromSlash(rel))); err == nil && info.ModTime().After(p.LastChange) {
				p.LastChange = info.ModTime()
			}
		}
	}
	if c := b.lastCommit(ctx, p.path); c != nil {
		p.LastCommit = c
		if p.LastChange.IsZero() || (p.Changed == 0 && c.When.After(p.LastChange)) {
			p.LastChange = c.When
		}
	}
}

// project finds a listed project by id.
func (b *Browser) project(ctx context.Context, id string) (Project, bool) {
	for _, p := range b.Projects(ctx) {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

// ---------------- paths ----------------

var errOutside = errors.New("that path is outside the project")

// resolve turns a "/"-separated path from the page into a real path inside
// the project, or refuses it. Symlinks are followed and must stay inside.
func resolve(project, rel string) (string, string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "" || rel == "." || rel == "/" {
		return project, "", nil
	}
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, ":") {
		return "", "", errOutside
	}
	var parts []string
	for _, seg := range strings.Split(rel, "/") {
		switch {
		case seg == "" || seg == ".":
			continue
		case seg == "..":
			return "", "", errOutside
		case strings.EqualFold(seg, ".git"):
			return "", "", errors.New(".git isn't shown")
		}
		parts = append(parts, seg)
	}
	clean := strings.Join(parts, "/")
	full := filepath.Join(project, filepath.FromSlash(clean))
	realRoot, err := filepath.EvalSymlinks(project)
	if err != nil {
		return "", "", err
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", "", err // includes "doesn't exist"
	}
	if !within(realRoot, realFull) {
		return "", "", errOutside
	}
	return full, clean, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// secret reports files whose contents are never sent: they usually hold
// keys and passwords. They're still listed, so you can see they exist.
func secret(rel string) bool {
	name := strings.ToLower(rel[strings.LastIndex(rel, "/")+1:])
	switch {
	case name == ".env" || strings.HasPrefix(name, ".env.") || strings.HasSuffix(name, ".env"):
		return true
	case strings.HasPrefix(name, "id_rsa") || strings.HasPrefix(name, "id_ed25519") || strings.HasPrefix(name, "id_ecdsa"):
		return true
	case strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".key") || strings.HasSuffix(name, ".p12") || strings.HasSuffix(name, ".pfx"):
		return true
	case name == ".netrc" || name == ".npmrc" || name == ".pypirc" || name == "credentials.json" || name == "secrets.json":
		return true
	}
	return false
}

package projects

// Branches: the list behind the Projects page's branch picker, and the
// "switch" button (actions.go). Switching only goes to a branch that's in this
// list: one of the project's own, or one the remote has that isn't here yet
// (it's then created, tracking the remote one). It refuses while there are
// uncommitted changes, so work never quietly moves to another branch.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Branch is one entry in the picker.
type Branch struct {
	Name string `json:"name"`
	// Remote is set for a branch only the remote has ("origin"); switching
	// to it creates the local branch.
	Remote   string    `json:"remote,omitempty"`
	Upstream string    `json:"upstream,omitempty"`
	Current  bool      `json:"current,omitempty"`
	When     time.Time `json:"when"`
}

const maxRemoteBranches = 100

// parseBranches reads `git for-each-ref --format=%(refname)%09%(upstream:short)%09%(committerdate:iso-strict) refs/heads refs/remotes`.
// Local branches come first (the current one, then newest first), then the
// remote's branches that have no local copy (newest first).
func parseBranches(out, current string) []Branch {
	var local, remote []Branch
	have := map[string]bool{}
	type ref struct {
		name string
		when time.Time
	}
	var remotes []ref
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		when, _ := time.Parse(time.RFC3339, strings.TrimSpace(parts[2]))
		switch {
		case strings.HasPrefix(parts[0], "refs/heads/"):
			name := strings.TrimPrefix(parts[0], "refs/heads/")
			have[name] = true
			local = append(local, Branch{Name: name, Upstream: parts[1], Current: name == current, When: when})
		case strings.HasPrefix(parts[0], "refs/remotes/"):
			remotes = append(remotes, ref{name: strings.TrimPrefix(parts[0], "refs/remotes/"), when: when})
		}
	}
	for _, r := range remotes {
		rem, name, ok := strings.Cut(r.name, "/")
		if !ok || name == "HEAD" || have[name] {
			continue
		}
		have[name] = true // the same branch on two remotes: list it once
		remote = append(remote, Branch{Name: name, Remote: rem, Upstream: r.name, When: r.when})
	}
	sort.SliceStable(local, func(i, j int) bool {
		if local[i].Current != local[j].Current {
			return local[i].Current
		}
		return local[i].When.After(local[j].When)
	})
	sort.SliceStable(remote, func(i, j int) bool { return remote[i].When.After(remote[j].When) })
	if len(remote) > maxRemoteBranches {
		remote = remote[:maxRemoteBranches]
	}
	return append(local, remote...)
}

func (b *Browser) branchList(ctx context.Context, dir, current string) ([]Branch, error) {
	out, err := b.git(ctx, dir, "for-each-ref", "--format=%(refname)%09%(upstream:short)%09%(committerdate:iso-strict)", "refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}
	return parseBranches(string(out), current), nil
}

func (b *Browser) branches(w http.ResponseWriter, r *http.Request) {
	p, ok := b.project(r.Context(), r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such project")
		return
	}
	st, err := b.status(r.Context(), p.path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	list, err := b.branchList(r.Context(), p.path, st.branch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []Branch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": st.branch, "branches": list})
}

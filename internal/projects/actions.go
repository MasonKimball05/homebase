package projects

// The Projects page's git buttons: Fetch, Pull, Push and Commit, and nothing
// else. Everything else in this package only reads; these four are the
// exceptions, and they're kept to fixed, safe forms:
//
//	fetch    git fetch --prune
//	pull     git pull --ff-only          (never merges or rebases on its own)
//	push     git push                    (the branch to its own upstream; never --force)
//	commit   git add -A; git commit -m   (refused if a secret-looking file would go in)
//	switch   git switch <branch>         (only a listed branch, never with uncommitted changes; branches.go)
//
// Like the dashboard's Start/Stop buttons they need the X-Homebase header from
// the dashboard itself (no other website can press them), one runs at a time
// per project, and git is never allowed to stop and ask for a password: with
// no stored credentials it fails with a message instead of hanging.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	actionTimeout = 2 * time.Minute
	maxMessage    = 5000
	maxOutput     = 20 << 10
)

// runGitAction runs git and returns everything it printed (stdout and
// stderr together, as you'd see in a terminal).
func runGitAction(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-c", "core.quotepath=off", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",                // https: fail rather than ask for a password
		"GCM_INTERACTIVE=never",                // Git Credential Manager: no login windows
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes", // ssh remotes: no passphrase prompts
		"GIT_EDITOR=true",                      // nothing may open an editor and wait
	)
	hideWindow(cmd)
	return cmd.CombinedOutput()
}

type locks struct {
	mu   sync.Mutex
	busy map[string]bool
}

// take marks a project busy, or reports that something's already running there.
func (l *locks) take(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy == nil {
		l.busy = map[string]bool{}
	}
	if l.busy[id] {
		return false
	}
	l.busy[id] = true
	return true
}

func (l *locks) release(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.busy, id)
}

// ActionResult is what the page shows after a button.
type ActionResult struct {
	OK      bool    `json:"ok"`
	Summary string  `json:"summary"`          // one line: "Pushed 2 commits to origin/main."
	Output  string  `json:"output,omitempty"` // what git printed
	Project Project `json:"project"`          // the project as it is now
}

func (b *Browser) act(w http.ResponseWriter, r *http.Request) {
	if b.allowed == nil || !b.allowed(r) {
		writeError(w, http.StatusForbidden, "request not from the dashboard")
		return
	}
	action := r.PathValue("action")
	if action != "fetch" && action != "pull" && action != "push" && action != "commit" && action != "switch" {
		writeError(w, http.StatusNotFound, "unknown action")
		return
	}
	var body struct {
		Message string `json:"message"`
		Branch  string `json:"branch"`
	}
	if action == "commit" || action == "switch" {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "send it as JSON: {\"message\": \"...\"} to commit, {\"branch\": \"...\"} to switch")
			return
		}
	}
	p, ok := b.project(r.Context(), r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such project")
		return
	}
	if !b.locks.take(p.ID) {
		writeError(w, http.StatusConflict, "another git action is still running in this project")
		return
	}
	defer b.locks.release(p.ID)

	// Finish what was started even if the page closes: a half-done push helps nobody.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), actionTimeout)
	defer cancel()
	arg := strings.TrimSpace(body.Message)
	if action == "switch" {
		arg = strings.TrimSpace(body.Branch)
	}
	summary, out, err := b.run(ctx, p, action, arg)

	b.forget() // the next listing must show the new state
	now, _ := b.project(ctx, p.ID)
	res := ActionResult{OK: err == nil, Summary: summary, Output: clipOutput(out), Project: now}
	if err != nil {
		res.Summary = err.Error()
		writeJSON(w, http.StatusConflict, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// run does one action; the error, if any, is written for a person to read.
// arg is the commit message, or the branch to switch to.
func (b *Browser) run(ctx context.Context, p Project, action, arg string) (string, string, error) {
	message := arg
	st, err := b.status(ctx, p.path)
	if err != nil {
		return "", "", fmt.Errorf("couldn't read the project's status: %v", err)
	}
	git := func(args ...string) (string, error) {
		out, err := b.gitAction(ctx, p.path, args...)
		if ctx.Err() != nil {
			return string(out), errors.New("git took too long and was stopped")
		}
		return string(out), err
	}

	switch action {
	case "fetch":
		out, err := git("fetch", "--prune")
		if err != nil {
			return "", out, fmt.Errorf("fetch failed: %s", firstLine(out, err))
		}
		after, _ := b.status(ctx, p.path)
		return fetchSummary(after), out, nil

	case "pull":
		if st.upstream == "" {
			return "", "", fmt.Errorf("%s doesn't track a remote branch, so there's nothing to pull from", st.branch)
		}
		out, err := git("pull", "--ff-only")
		if err != nil {
			if strings.Contains(out, "Not possible to fast-forward") || strings.Contains(out, "diverg") {
				return "", out, errors.New("can't pull: this branch and the remote both have new commits. Sort it out in Loom (merge or rebase), where you can see the conflicts")
			}
			if strings.Contains(out, "would be overwritten") {
				return "", out, errors.New("can't pull: it would overwrite uncommitted changes. Commit them first")
			}
			return "", out, fmt.Errorf("pull failed: %s", firstLine(out, err))
		}
		if strings.Contains(out, "Already up to date") {
			return "Already up to date.", out, nil
		}
		return fmt.Sprintf("Pulled from %s.", st.upstream), out, nil

	case "push":
		if st.upstream == "" {
			return "", "", fmt.Errorf("%s doesn't track a remote branch yet. Push it once from Loom (or `git push -u origin %s` on the desktop) to set that up", st.branch, st.branch)
		}
		if st.gone {
			return "", "", fmt.Errorf("the remote branch %s was deleted; push from Loom to decide where it should go", st.upstream)
		}
		if st.ahead == 0 {
			return "Nothing to push.", "", nil
		}
		out, err := git("push")
		if err != nil {
			if strings.Contains(out, "rejected") || strings.Contains(out, "fetch first") || strings.Contains(out, "non-fast-forward") {
				return "", out, errors.New("push rejected: the remote has commits this copy doesn't. Pull first")
			}
			if strings.Contains(out, "Authentication failed") || strings.Contains(out, "could not read Username") || strings.Contains(out, "Permission denied") {
				return "", out, errors.New("push failed: git on the desktop has no saved login for this remote. Push once by hand on the desktop so it's remembered")
			}
			return "", out, fmt.Errorf("push failed: %s", firstLine(out, err))
		}
		return fmt.Sprintf("Pushed %d commit%s to %s.", st.ahead, plural(st.ahead), st.upstream), out, nil

	case "commit":
		if message == "" {
			return "", "", errors.New("write a commit message first")
		}
		if len(message) > maxMessage {
			return "", "", errors.New("that commit message is too long")
		}
		if len(st.files) == 0 {
			return "Nothing to commit.", "", nil
		}
		var secrets []string
		for path := range st.files {
			if secret(path) {
				secrets = append(secrets, path)
			}
		}
		if len(secrets) > 0 {
			sort.Strings(secrets)
			return "", "", fmt.Errorf("not committing: these look like they hold secrets: %s. Add them to .gitignore, or commit on the desktop if they're really meant to be shared", strings.Join(secrets, ", "))
		}
		if out, err := git("add", "-A"); err != nil {
			return "", out, fmt.Errorf("couldn't stage the changes: %s", firstLine(out, err))
		}
		out, err := git("commit", "-m", message)
		if err != nil {
			if strings.Contains(out, "Please tell me who you are") || strings.Contains(out, "user.email") {
				return "", out, errors.New("git on the desktop doesn't know your name and email yet. Set them once there: git config --global user.name / user.email")
			}
			return "", out, fmt.Errorf("commit failed: %s", firstLine(out, err))
		}
		return fmt.Sprintf("Committed %d file%s.", len(st.files), plural(len(st.files))), out, nil
	case "switch":
		return b.switchBranch(ctx, p, st, arg, git)
	}
	return "", "", errors.New("unknown action")
}

func (b *Browser) switchBranch(ctx context.Context, p Project, st repoStatus, name string, git func(...string) (string, error)) (string, string, error) {
	if name == "" {
		return "", "", errors.New("pick a branch to switch to")
	}
	if name == st.branch {
		return fmt.Sprintf("Already on %s.", name), "", nil
	}
	if n := len(st.files); n > 0 {
		return "", "", fmt.Errorf("%s has %d uncommitted change%s. Commit them first (or stash them on the desktop), then switch, so nothing moves to the other branch by accident", p.Name, n, plural(n))
	}
	list, err := b.branchList(ctx, p.path, st.branch)
	if err != nil {
		return "", "", fmt.Errorf("couldn't list the branches: %v", err)
	}
	for _, br := range list {
		if br.Name != name {
			continue
		}
		var out string
		if br.Remote == "" {
			out, err = git("switch", br.Name)
		} else {
			out, err = git("switch", "-c", br.Name, "--track", br.Upstream)
		}
		if err != nil {
			if strings.Contains(out, "would be overwritten") {
				return "", out, errors.New("can't switch: it would overwrite files that aren't committed. Commit or remove them first")
			}
			return "", out, fmt.Errorf("switch failed: %s", firstLine(out, err))
		}
		if br.Remote != "" {
			return fmt.Sprintf("Switched to %s (new here, tracking %s).", br.Name, br.Upstream), out, nil
		}
		return fmt.Sprintf("Switched to %s.", br.Name), out, nil
	}
	return "", "", fmt.Errorf("there's no branch called %q here or on the remote. Fetch first if it's new", name)
}

func fetchSummary(st repoStatus) string {
	switch {
	case st.upstream == "":
		return "Fetched. This branch doesn't track a remote one."
	case st.behind > 0 && st.ahead > 0:
		return fmt.Sprintf("Fetched: %d new commit%s to pull, and %d commit%s of yours to push.", st.behind, plural(st.behind), st.ahead, plural(st.ahead))
	case st.behind > 0:
		return fmt.Sprintf("Fetched: %d new commit%s to pull.", st.behind, plural(st.behind))
	default:
		return "Fetched: nothing new on the remote."
	}
}

func (b *Browser) forget() {
	b.mu.Lock()
	b.cached = nil
	b.mu.Unlock()
}

func firstLine(out string, err error) string {
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "fatal:") || strings.HasPrefix(l, "error:") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(l, "fatal:"), "error:"))
		}
	}
	if s := strings.TrimSpace(out); s != "" {
		return strings.SplitN(s, "\n", 2)[0]
	}
	return err.Error()
}

func clipOutput(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "\n… (cut off)"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

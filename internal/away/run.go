package away

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// runTask does one task start to finish (see the package comment).
func (s *Service) runTask(ctx context.Context, id string) {
	s.mu.Lock()
	task := *s.find(id)
	s.mu.Unlock()

	_ = os.MkdirAll(s.set.LogDir, 0o755)
	log, _ := os.OpenFile(filepath.Join(s.set.LogDir, id+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if log != nil {
		defer log.Close()
		fmt.Fprintf(log, "\n==== %s  %s: %s\n", s.now().Format(time.RFC3339), task.Project, task.Text)
	}

	rec := newRecording()
	outcome, err := s.work(ctx, &task, log, rec)
	// A failure before anything was committed leaves no branch behind.
	if err != nil && ctx.Err() == nil && task.Branch != "" && task.BaseCommit != "" {
		if repo, ok := s.projects.Path(task.Project); ok {
			if out, gerr := git(context.Background(), repo, "rev-list", "--count", task.BaseCommit+".."+task.Branch); gerr == nil && strings.TrimSpace(out) == "0" {
				dropBranch(context.Background(), repo, s.worktree(task), task.Branch)
				task.Branch = ""
			}
		}
	}

	// Clean up before the queue changes, so a paused task can't start again
	// while its old branch is still there (same name).
	interrupted := ctx.Err() != nil
	repo, _ := s.projects.Path(task.Project)
	if interrupted {
		// Leave nothing behind: the next attempt makes a fresh branch.
		dropBranch(context.Background(), repo, s.worktree(task), task.Branch)
	} else {
		// The branch keeps the work; the worktree isn't needed any more.
		removeWorktree(context.Background(), repo, s.worktree(task))
		// The example is written before the task shows as finished, so an
		// Accept straight away always finds it to record the outcome on.
		fin := task
		fin.Finished = s.now()
		if err != nil {
			fin.Status, fin.Error = StatusFailed, err.Error()
		} else {
			fin.Status = outcome
		}
		s.saveExample(context.Background(), repo, fin, rec)
	}

	s.mu.Lock()
	_, stopped := cancelledIDs.LoadAndDelete(id)
	s.cancel, s.running = nil, ""
	t := s.find(id)
	if t != nil {
		switch {
		case interrupted && !stopped:
			// Paused (you came back, or switched it off): start over later, same place in the queue.
			t.Status, t.Stage = StatusQueued, ""
			t.Branch, t.Base, t.BaseCommit, t.Rounds, t.Changed, t.Shown = "", "", "", []Round{}, 0, nil
		case interrupted:
			t.Status, t.Stage, t.Finished = StatusCancelled, "", s.now()
		case err != nil:
			t.Status, t.Stage, t.Error, t.Finished = StatusFailed, "", err.Error(), s.now()
			t.Rounds, t.Branch, t.Base, t.BaseCommit = task.Rounds, task.Branch, task.Base, task.BaseCommit
		default:
			t.Status, t.Stage, t.Finished = outcome, "", s.now()
			t.Rounds, t.Branch, t.Base, t.BaseCommit = task.Rounds, task.Branch, task.Base, task.BaseCommit
			t.Verdict, t.Notes, t.Changed = task.Verdict, task.Notes, task.Changed
		}
	}
	s.saveLocked()
	s.mu.Unlock()

	if interrupted {
		if log != nil {
			fmt.Fprintf(log, "---- stopped (%s)\n", map[bool]string{true: "cancelled", false: "paused: back at the PC, or Away switched off"}[stopped])
		}
		s.poke()
		return
	}
	var final Task
	if t != nil {
		s.mu.Lock()
		final = *t
		s.mu.Unlock()
	}
	if final.Branch != "" && (final.Status == StatusDone || final.Status == StatusNeedsYou) && s.pushes(final.Project) {
		if err := pushBranch(context.Background(), repo, final.Branch); err != nil {
			s.update(id, func(x *Task) { x.Error = "not pushed: " + err.Error() })
		} else {
			s.update(id, func(x *Task) { x.Pushed = true })
		}
	}
	if t != nil {
		s.notify(notice(final))
	}
	s.poke()
}

func notice(t Task) (string, string) {
	short := firstLine(t.Text, 80)
	switch {
	case t.Status == StatusFailed:
		return "Away: failed", fmt.Sprintf("%s: %s\n%s", t.Project, short, firstLine(t.Error, 200))
	case t.Status == StatusNeedsYou:
		return "Away: needs you", fmt.Sprintf("%s: %s\nClaude: %s", t.Project, short, firstLine(t.Notes, 300))
	case t.Verdict == "fixed":
		return "Away: done (Claude fixed it)", fmt.Sprintf("%s: %s\n%d file(s) on %s", t.Project, short, t.Changed, t.Branch)
	default:
		return "Away: done", fmt.Sprintf("%s: %s\n%d file(s) on %s, passed Claude's review", t.Project, short, t.Changed, t.Branch)
	}
}

// work: the worktree, the rounds, Claude's fix. Returns the final status.
func (s *Service) work(ctx context.Context, t *Task, log *os.File, rec *recording) (string, error) {
	repo, ok := s.projects.Path(t.Project)
	if !ok {
		return "", errors.New("that project isn't listed any more")
	}
	stage := func(st string) {
		s.update(t.ID, func(x *Task) { x.Stage = st; x.Rounds = append([]Round(nil), t.Rounds...) })
	}

	stage("making a worktree")
	base, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git: %v", err)
	}
	commit, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git: %v", err)
	}
	t.Base, t.BaseCommit, t.Branch = strings.TrimSpace(base), strings.TrimSpace(commit), "away/"+t.ID
	s.update(t.ID, func(x *Task) { x.Base, x.BaseCommit, x.Branch = t.Base, t.BaseCommit, t.Branch })
	wt := s.worktree(*t)
	_ = os.MkdirAll(s.set.Worktrees, 0o755)
	// A branch by this name left over from an attempt that never cleaned up: clear it.
	if _, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+t.Branch); err == nil {
		dropBranch(ctx, repo, wt, t.Branch)
	}
	if _, err := git(ctx, repo, "worktree", "add", "-b", t.Branch, wt, t.BaseCommit); err != nil {
		return "", fmt.Errorf("couldn't make a worktree: %v", err)
	}
	local := linkDeps(repo, wt, log)

	// How similar tasks went before (recall.go), shown with the task in every round.
	stage("looking for similar past tasks")
	var examples string
	var shown []string
	held := heldOut(t.ID, s.set.RecallHoldout)
	if !held {
		examples, shown = s.recall(ctx, *t)
	} else if log != nil {
		fmt.Fprintf(log, "---- held out: no earlier examples this time, for comparison\n")
	}
	t.Shown, t.HeldOut = shown, held
	s.update(t.ID, func(x *Task) { x.Shown, x.HeldOut = shown, held })
	if len(shown) > 0 && log != nil {
		fmt.Fprintf(log, "---- shown %d earlier example(s): %s\n", len(shown), strings.Join(shown, ", "))
	}

	notes := ""
	verdict := ""
	for round := 1; round <= s.set.Rounds; round++ {
		stage(fmt.Sprintf("local model, round %d of %d", round, s.set.Rounds))
		started := time.Now()
		prompt := t.Text + examples
		if notes != "" {
			prompt += "\n\nYour previous attempt is already committed on this branch. A reviewer looked at it and asked for these changes; make them:\n\n" + notes
		}
		transcript := filepath.Join(s.set.LogDir, fmt.Sprintf("%s.round%d.qwen.json", t.ID, round))
		rec.transcripts[len(t.Rounds)] = transcript // the qwen round about to be added
		qerr := s.steps.Qwen(ctx, wt, prompt, s.set.Tests[t.Project], transcript, log)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var me *ModelError
		if errors.As(qerr, &me) {
			return "", fmt.Errorf("the local model couldn't run: %s", me.Msg)
		}
		sha, n, cerr := commitAll(ctx, wt, fmt.Sprintf("Away: %s (local model, round %d)", firstLine(t.Text, 60), round), local)
		if cerr != nil {
			return "", cerr
		}
		if qerr != nil && n == 0 && round == 1 {
			return "", fmt.Errorf("the local model didn't run: %v", qerr)
		}
		t.Rounds = append(t.Rounds, Round{Who: "qwen", Seconds: int(time.Since(started).Seconds()), Commit: sha, Notes: errText(qerr)})

		// Its own tests first: a failing round goes straight back (or to
		// Claude's fix), without spending a review on it.
		ran := ""
		if base := s.set.Tests[t.Project]; base != "" {
			if targets := testTargets(base, changedTestFiles(ctx, wt, t.BaseCommit)); len(targets) > 0 {
				line := base + " " + strings.Join(targets, " ")
				stage(fmt.Sprintf("running its tests, round %d", round))
				started = time.Now()
				ok, out := s.steps.Tests(ctx, wt, line, log)
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				if !ok {
					t.Rounds = append(t.Rounds, Round{Who: "tests", Verdict: "fail", Notes: tailText(out, 1500), Seconds: int(time.Since(started).Seconds())})
					verdict, notes = "revise", fmt.Sprintf("Your tests fail. `%s` says:\n\n%s\n\nMake them pass, then stop.", line, tailText(out, 3000))
					continue
				}
				t.Rounds = append(t.Rounds, Round{Who: "tests", Verdict: "pass", Seconds: int(time.Since(started).Seconds())})
				ran = line
			}
		}

		stage(fmt.Sprintf("Claude reviewing round %d", round))
		started = time.Now()
		reply, err := s.steps.Claude(ctx, wt, reviewPrompt(*t, ran), false, log)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			return "", fmt.Errorf("Claude Code: %v", err)
		}
		verdict, notes = parseVerdict(reply)
		t.Rounds = append(t.Rounds, Round{Who: "review", Verdict: verdict, Notes: notes, Seconds: int(time.Since(started).Seconds())})
		if verdict == "pass" || verdict == "fail" {
			break
		}
	}

	switch verdict {
	case "pass":
		t.Verdict, t.Notes = "pass", notes
	case "fail":
		t.Verdict, t.Notes = "fail", notes
	default:
		stage("Claude fixing it")
		started := time.Now()
		reply, err := s.steps.Claude(ctx, wt, fixPrompt(*t, notes), true, log)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			return "", fmt.Errorf("Claude Code: %v", err)
		}
		v, n := parseVerdict(reply)
		sha, _, cerr := commitAll(ctx, wt, fmt.Sprintf("Away: %s (Claude's fix)", firstLine(t.Text, 60)), local)
		if cerr != nil {
			return "", cerr
		}
		t.Rounds = append(t.Rounds, Round{Who: "claude-fix", Verdict: v, Notes: n, Seconds: int(time.Since(started).Seconds()), Commit: sha})
		if v == "fail" {
			t.Verdict, t.Notes = "fail", n
		} else {
			t.Verdict, t.Notes = "fixed", n
		}
	}
	if out, err := git(ctx, wt, "diff", "--name-only", t.BaseCommit+"..HEAD"); err == nil {
		t.Changed = len(strings.Fields(out))
	}
	if t.Verdict == "fail" {
		return StatusNeedsYou, nil
	}
	return StatusDone, nil
}

var testFile = regexp.MustCompile(`(^|/)(test_[^/]*\.py|[^/]*_test\.(py|go)|[^/]*\.(test|spec)\.(ts|tsx|js|jsx|mjs|cjs))$`)

// changedTestFiles: test files changed on the branch so far.
func changedTestFiles(ctx context.Context, wt, base string) []string {
	out, err := git(ctx, wt, "diff", "--name-only", "--diff-filter=AMR", base+"..HEAD")
	if err != nil {
		return nil
	}
	var files []string
	for _, f := range strings.Fields(out) {
		if testFile.MatchString(f) {
			files = append(files, f)
		}
	}
	return files
}

// testTargets: how the project's test command names those files. Django's
// and unittest's want dotted modules; others take paths.
func testTargets(command string, files []string) []string {
	modules := strings.Contains(command, "manage.py test") || strings.Contains(command, "unittest")
	var out []string
	for _, f := range files {
		if modules {
			if !strings.HasSuffix(f, ".py") {
				continue
			}
			f = strings.ReplaceAll(strings.TrimSuffix(f, ".py"), "/", ".")
		}
		out = append(out, f)
	}
	return out
}

func tailText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return "…" + s[len(s)-max:]
	}
	return s
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return firstLine(err.Error(), 300)
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func reviewPrompt(t Task, testsPassed string) string {
	ran := ""
	if testsPassed != "" {
		ran = " Away already ran `" + testsPassed + "` on this branch and it passed, so judge the tests' quality and coverage rather than whether they run."
	}
	return fmt.Sprintf(`You are checking work done unattended by a local model (Qwen) on branch %s of this repository, while its owner is away. The task it was given:

<task>
%s
</task>

See the change with `+"`git diff %s...HEAD`"+` and read whatever files you need. You may run the project's tests (for a Django project: `+"`python manage.py test <module>`"+`). Do not edit anything.%s

Judge it like a careful reviewer: does it do the whole task, correctly; does it stay in scope (no unrelated changes, no "improvements" nobody asked for); could it break something (look at callers, tests, migrations)?

End your reply with exactly one line of JSON and nothing after it:
{"verdict": "pass" | "revise" | "fail", "notes": "..."}
- pass: good to merge as it is. notes: one or two sentences on what it did.
- revise: notes are concrete, numbered instructions for the model to fix it (file and what to change).
- fail: the task is impossible, unclear or a bad idea as written. notes explain why, for the owner.`, t.Branch, t.Text, t.BaseCommit, ran)
}

func fixPrompt(t Task, notes string) string {
	return fmt.Sprintf(`A local model worked on this task unattended on branch %s and its attempts didn't pass review. The task:

<task>
%s
</task>

The last review's notes:
<notes>
%s
</notes>

See what's there with `+"`git diff %s...HEAD`"+`. Finish the task yourself: edit the files so it is done correctly and completely, staying within the task, and run the relevant tests (for a Django project: `+"`python manage.py test <module>`"+`) until they pass. Don't commit, push or switch branches (that's handled for you).

End your reply with exactly one line of JSON and nothing after it:
{"verdict": "fixed" | "fail", "notes": "..."}
- fixed: notes summarize the final change for the owner.
- fail: the task can't be done as written; notes explain why.`, t.Branch, t.Text, notes, t.BaseCommit)
}

var verdictLine = regexp.MustCompile(`\{[^{}]*"verdict"[^{}]*\}`)

// parseVerdict finds the last {"verdict": …, "notes": …} in Claude's reply.
// No verdict at all counts as "revise" with the whole reply as notes.
func parseVerdict(reply string) (string, string) {
	all := verdictLine.FindAllString(reply, -1)
	for i := len(all) - 1; i >= 0; i-- {
		var v struct {
			Verdict string `json:"verdict"`
			Notes   string `json:"notes"`
		}
		if json.Unmarshal([]byte(all[i]), &v) == nil {
			switch v.Verdict {
			case "pass", "revise", "fail", "fixed":
				return v.Verdict, strings.TrimSpace(v.Notes)
			}
		}
	}
	return "revise", firstLine(reply, 4000)
}

// ---------------- git ----------------

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	hide(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %s", args[0], firstLine(strings.TrimSpace(string(out)), 300))
	}
	return string(out), nil
}

// Folders a project needs to run its checks, shared from your checkout
// into the worktree (a link, never committed).
var depDirs = []string{"node_modules", ".venv", "venv", "env"}

// Instruction files you keep in your checkout without committing them (a
// private AGENTS.md, QWEN.md or CLAUDE.local.md): copied into the worktree so
// the model and Claude read them, and never committed from there.
var localFiles = []string{"AGENTS.md", "QWEN.md", "CLAUDE.md", "CLAUDE.local.md"}

// linkDeps shares the dependency folders and copies the local-only files.
// Returns the files it copied (to keep out of commits).
func linkDeps(repo, wt string, log *os.File) []string {
	var copied []string
	for _, d := range depDirs {
		src := filepath.Join(repo, d)
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
			continue
		}
		if err := linkDir(src, filepath.Join(wt, d)); err != nil && log != nil {
			fmt.Fprintf(log, "couldn't share %s with the worktree: %v\n", d, err)
		}
	}
	// .env files hold settings the project needs to run; copied, never committed.
	if entries, err := os.ReadDir(repo); err == nil {
		for _, e := range entries {
			if !e.IsDir() && (e.Name() == ".env" || strings.HasPrefix(e.Name(), ".env.")) && !strings.HasSuffix(e.Name(), ".example") {
				if b, err := os.ReadFile(filepath.Join(repo, e.Name())); err == nil {
					_ = os.WriteFile(filepath.Join(wt, e.Name()), b, 0o600)
				}
			}
		}
	}
	for _, name := range localFiles {
		dst := filepath.Join(wt, name)
		if _, err := os.Stat(dst); err == nil {
			continue // tracked: the branch has its own copy
		}
		if b, err := os.ReadFile(filepath.Join(repo, name)); err == nil {
			if os.WriteFile(dst, b, 0o644) == nil {
				copied = append(copied, name)
			}
		}
	}
	return copied
}

// commitAll commits what changed in the worktree, leaving out the shared
// folders and anything secret-looking. Returns the commit and how many files.
func commitAll(ctx context.Context, wt, msg string, local []string) (string, int, error) {
	out, err := git(ctx, wt, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return "", 0, err
	}
	var paths []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		p := e[3:]
		if e[0] == 'R' || e[0] == 'C' {
			i++ // the old name follows
		}
		if skipCommit(p) || contains(local, p) {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return "", 0, nil
	}
	if _, err := git(ctx, wt, append([]string{"add", "-A", "--"}, paths...)...); err != nil {
		return "", 0, err
	}
	if _, err := git(ctx, wt, "-c", "user.name=Away (homebase)", "-c", "user.email=away@homebase.local", "commit", "-q", "--no-verify", "-m", msg); err != nil {
		return "", 0, err
	}
	sha, _ := git(ctx, wt, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(sha), len(paths), nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func skipCommit(p string) bool {
	p = strings.TrimSuffix(filepath.ToSlash(p), "/")
	first := strings.SplitN(p, "/", 2)[0]
	for _, d := range depDirs {
		if first == d {
			return true
		}
	}
	name := p[strings.LastIndex(p, "/")+1:]
	switch {
	case name == ".qwen-run.json":
		return true
	case name == ".env" || (strings.HasPrefix(name, ".env.") && !strings.HasSuffix(name, ".example")) || strings.HasSuffix(name, ".env"):
		return true
	case strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".key") || strings.HasPrefix(name, "id_rsa") || strings.HasPrefix(name, "id_ed25519"):
		return true
	case name == "credentials.json" || name == "secrets.json" || name == ".netrc" || name == ".npmrc" || name == ".pypirc":
		return true
	}
	return false
}

func removeWorktree(ctx context.Context, repo, wt string) {
	if repo == "" {
		return
	}
	// The shared folders are links: remove them first so nothing behind them is touched.
	for _, d := range depDirs {
		unlinkDir(filepath.Join(wt, d))
	}
	if _, err := os.Stat(wt); err == nil {
		_, _ = git(ctx, repo, "worktree", "remove", "--force", wt)
	}
	_, _ = git(ctx, repo, "worktree", "prune")
}

func (s *Service) pushes(project string) bool {
	for _, p := range s.set.Push {
		if p == project {
			return true
		}
	}
	return false
}

// pushBranch sends an away/… branch to origin, and nothing else.
func pushBranch(ctx context.Context, repo, branch string) error {
	if !strings.HasPrefix(branch, "away/") {
		return errors.New("only away/… branches are pushed")
	}
	ref := "refs/heads/" + branch
	_, err := git(ctx, repo, "push", "--no-verify", "origin", ref+":"+ref)
	return err
}

// unpush deletes the GitHub (origin) copy of a pushed away/… branch once
// you've accepted, discarded or retried it. A branch already gone there is fine.
func (s *Service) unpush(ctx context.Context, repo string, t Task) {
	if !t.Pushed || repo == "" || !strings.HasPrefix(t.Branch, "away/") {
		return
	}
	if _, err := git(ctx, repo, "push", "--no-verify", "origin", "--delete", t.Branch); err != nil && !strings.Contains(err.Error(), "remote ref does not exist") {
		s.logf("away: deleting %s from origin: %v", t.Branch, err)
	}
}

func dropBranch(ctx context.Context, repo, wt, branch string) {
	removeWorktree(ctx, repo, wt)
	if repo != "" && strings.HasPrefix(branch, "away/") {
		_, _ = git(ctx, repo, "branch", "-D", branch)
	}
}

// accept merges the branch into the one it started from, in your checkout.
// Only when that checkout is on that branch with no uncommitted changes,
// so nothing of yours is ever mixed in or overwritten.
func accept(ctx context.Context, repo string, t Task) error {
	if t.Branch == "" {
		return errors.New("there's no branch to merge")
	}
	cur, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(cur) != t.Base {
		return fmt.Errorf("the project is on %s now; switch it back to %s to accept", strings.TrimSpace(cur), t.Base)
	}
	if out, _ := git(ctx, repo, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(out) != "" {
		return errors.New("the project has uncommitted changes: commit or stash them first")
	}
	if _, err := git(ctx, repo, "-c", "user.name=Away (homebase)", "-c", "user.email=away@homebase.local", "merge", "--no-ff", "--no-edit", "-m", "Merge "+t.Branch+": "+firstLine(t.Text, 60), t.Branch); err != nil {
		_, _ = git(context.Background(), repo, "merge", "--abort")
		return fmt.Errorf("it doesn't merge cleanly (the project changed since): %v", err)
	}
	_, _ = git(ctx, repo, "branch", "-D", t.Branch)
	return nil
}

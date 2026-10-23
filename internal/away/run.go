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
	// The local model being broken isn't this task's fault: it goes back in
	// the queue and Away waits, then tries again (or, set up wrong or still
	// not answering, switches itself off), rather than failing every task
	// after it the same way (or spending Claude on empty attempts).
	var down *modelDown
	isDown := errors.As(err, &down)
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
	if interrupted || isDown {
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
	var wait *Wait // set when Away waits for the model rather than switching off
	switch {
	case isDown && !interrupted:
		// 10-21: a task started two minutes after the PC was switched on,
		// while Ollama was still installing an update, and Away stayed off
		// for hours. Wait, then try again.
		now, w := s.now(), s.st.Waiting
		if w == nil {
			w = &Wait{Since: now, Max: modelTries}
		}
		w.Reason, w.At, w.Retry, w.Tries = down.Error(), now, now.Add(modelWait), w.Tries+1
		if down.setup || w.Tries >= w.Max {
			s.st.Mode, s.st.Stopped, s.st.Waiting = ModeOff, down.Error(), nil
			if w.Tries > 1 {
				s.st.Stopped += fmt.Sprintf(" (no answer in %d tries over %d min)", w.Tries, int(now.Sub(w.Since).Minutes()))
			}
		} else {
			s.st.Waiting, wait = w, w
		}
	case !interrupted:
		s.st.Waiting = nil // it got past the model
	}
	t := s.find(id)
	if t != nil {
		switch {
		case isDown && !interrupted:
			t.Status, t.Stage, t.Error = StatusQueued, "", ""
			t.Branch, t.Base, t.BaseCommit, t.Rounds, t.Changed, t.Shown = "", "", "", []Round{}, 0, nil
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
	stoppedWhy := s.st.Stopped
	var retry string
	var try, of int
	if wait != nil {
		retry, try, of = wait.Retry.Format("15:04"), wait.Tries+1, wait.Max
	}
	s.saveLocked()
	s.mu.Unlock()

	if isDown && !interrupted && wait != nil {
		if log != nil {
			fmt.Fprintf(log, "---- %s: back in the queue; Away tries again at %s (try %d of %d)\n", down.Error(), retry, try, of)
		}
		return
	}
	if isDown && !interrupted {
		if log != nil {
			fmt.Fprintf(log, "---- %s: back in the queue, and Away is off until it's fixed\n", stoppedWhy)
		}
		s.notify("Away: switched off", fmt.Sprintf("%s\n%s is back in the queue. Fix it, then switch Away back on.", stoppedWhy, firstLine(task.Text, 80)))
		return
	}
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

// modelAnswers: whether the local model answers a one-line question, to
// tell a broken model from one bad answer. No way to ask (tests, or no
// Ollama configured) counts as no.
func (s *Service) modelAnswers(ctx context.Context) bool {
	if s.generate == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := s.generate(ctx, "Reply with the single word OK.")
	return err == nil && strings.TrimSpace(out) != ""
}

// modelDown: the local model (or its runner) can't be used, so nothing can be.
// setup: it's set up wrong (ModelError.Setup), so waiting won't help.
type modelDown struct {
	msg   string
	setup bool
}

func (e *modelDown) Error() string { return "the local model couldn't run: " + e.msg }

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

	// A drill (refill.go): plant the bug as a commit of its own, and count
	// the attempt from there, so the diff and the review show only the fix.
	if d := t.Drill; d != nil {
		stage("planting the bug")
		py, err := s.mutantsCommand()
		if err != nil {
			return "", err
		}
		if ok, out := s.steps.Tests(ctx, wt, mutantCommand(py, "apply", d.File, d.Function, d.Mutant), log); !ok {
			return "", fmt.Errorf("couldn't plant the drill's bug: %s", tailText(out, 300))
		}
		if _, n, err := commitAll(ctx, wt, "Away drill: a bug planted in "+d.Function, local, t.BaseCommit); err != nil || n == 0 {
			return "", fmt.Errorf("couldn't commit the drill's bug: %v", err)
		}
		head, err := git(ctx, wt, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		t.BaseCommit = strings.TrimSpace(head)
		s.update(t.ID, func(x *Task) { x.BaseCommit = t.BaseCommit })
	}

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
	attempt := 0      // Qwen runs so far: the rounds, plus one if the check sent it back
	checked := false  // the pre-review check sends work back at most once
	mutated := false  // so does the mutation check (mutants.go)
	mutantNotes := "" // what it sent back, for the review to follow up
	retried := false  // a round whose answer broke gets one more go
	for round := 1; round <= s.set.Rounds; round++ {
		attempt++
		stage(fmt.Sprintf("local model, round %d of %d", round, s.set.Rounds))
		started := time.Now()
		prompt := t.Text + examples
		if notes != "" {
			prompt += "\n\nYour previous attempt is already committed on this branch. It was sent back with these notes; act on them:\n\n" + notes
		}
		transcript := filepath.Join(s.set.LogDir, fmt.Sprintf("%s.round%d.qwen.json", t.ID, attempt))
		rec.transcripts[len(t.Rounds)] = transcript // the qwen round about to be added
		qerr := s.steps.Qwen(ctx, wt, prompt, s.set.Tests[t.Project], transcript, log)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var me *ModelError
		broke := false // the model works, but this answer went wrong
		if errors.As(qerr, &me) {
			// 10-20: one garbled tool call ("expected element type <function>
			// but have <parameter>") switched Away off for the night. Ask the
			// model something first: if it answers, only this answer broke.
			if me.Setup || !s.modelAnswers(ctx) {
				return "", &modelDown{msg: me.Msg, setup: me.Setup}
			}
			broke = true
			qerr = fmt.Errorf("its answer broke off: %s", me.Msg)
			if log != nil {
				fmt.Fprintf(log, "---- %v (the model itself answers, so Away carries on)\n", qerr)
			}
		}
		sha, n, cerr := commitAll(ctx, wt, fmt.Sprintf("Away: %s (local model, round %d)", firstLine(t.Text, 60), attempt), local, t.BaseCommit)
		if cerr != nil {
			return "", cerr
		}
		if qerr != nil && n == 0 {
			switch {
			case broke && !retried:
				retried = true
				round--
				continue
			case broke:
				return "", qerr // this task's problem, not Away's: the queue goes on
			case attempt == 1:
				return "", &modelDown{msg: "it didn't run: " + qerr.Error()}
			}
		}
		t.Rounds = append(t.Rounds, Round{Who: "qwen", Seconds: int(time.Since(started).Seconds()), Commit: sha, Notes: errText(qerr)})

		// Its own tests first: a failing round goes straight back (or to
		// Claude's fix), without spending a review on it.
		ran := ""
		if base := s.set.Tests[t.Project]; base != "" {
			targets := testTargets(base, changedTestFiles(ctx, wt, t.BaseCommit))
			if t.Drill != nil {
				targets = []string{t.Drill.Target} // the tests the bug broke, which it mustn't change
			}
			if len(targets) > 0 {
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

		// A quick look for what the task named and the code's constants
		// (check.go), before spending a review. It doesn't use up a round.
		if !checked && t.Drill == nil {
			if diff, err := git(ctx, wt, "diff", t.BaseCommit+"..HEAD"); err == nil {
				if found := preReview(t.Text, diff, wt); found != "" {
					checked = true
					t.Rounds = append(t.Rounds, Round{Who: "check", Verdict: "revise", Notes: found})
					verdict, notes = "revise", found
					round--
					continue
				}
			}
		}

		// Do its tests notice when the code is wrong? Once per task, after
		// they pass; also without using up a round.
		if !mutated && ran != "" {
			base := s.set.Tests[t.Project]
			if targets := testTargets(base, changedTestFiles(ctx, wt, t.BaseCommit)); len(targets) > 0 {
				stage("checking its tests catch bugs")
				started = time.Now()
				found, summary := s.mutationCheck(ctx, *t, wt, base, targets, log)
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				if summary != "" {
					mutated = true
					v := "pass"
					if found != "" {
						v = "revise"
					}
					t.Rounds = append(t.Rounds, Round{Who: "mutants", Verdict: v, Notes: summary, Seconds: int(time.Since(started).Seconds())})
				}
				if found != "" {
					verdict, notes, mutantNotes = "revise", found, found
					round--
					continue
				}
			}
		}

		stage(fmt.Sprintf("Claude reviewing round %d", round))
		started = time.Now()
		reply, err := s.steps.Claude(ctx, wt, reviewPrompt(*t, ran, s.set.Tests[t.Project], mutantNotes), false, log)
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
		reply, err := s.steps.Claude(ctx, wt, fixPrompt(*t, notes, s.set.Tests[t.Project]), true, log)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			return "", fmt.Errorf("Claude Code: %v", err)
		}
		v, n := parseVerdict(reply)
		if v != "fail" {
			// A fix ends fixed or failed. Without a verdict line parseVerdict
			// says "revise", which showed on the round as if it went back.
			v = "fixed"
		}
		sha, _, cerr := commitAll(ctx, wt, fmt.Sprintf("Away: %s (Claude's fix)", firstLine(t.Text, 60)), local, t.BaseCommit)
		if cerr != nil {
			return "", cerr
		}
		t.Rounds = append(t.Rounds, Round{Who: "claude-fix", Verdict: v, Notes: n, Seconds: int(time.Since(started).Seconds()), Commit: sha})
		t.Verdict, t.Notes = v, n
	}
	if out, err := git(ctx, wt, "diff", "--name-only", t.BaseCommit+"..HEAD"); err == nil {
		t.Changed = len(strings.Fields(out))
	}
	if t.Verdict == "fail" {
		return StatusNeedsYou, nil
	}
	return StatusDone, nil
}

var testFile = regexp.MustCompile(`(^|/)(test_[^/]*\.(py|gd)|[^/]*_test\.(py|go)|[^/]*\.(test|spec)\.(ts|tsx|js|jsx|mjs|cjs))$`)

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

// testHint: how to run the tests, in the project's own words.
func testHint(command string) string {
	if command == "" {
		return "for a Django project: `python manage.py test <module>`"
	}
	return "here: `" + command + " <test file or module>`"
}

func reviewPrompt(t Task, testsPassed, testCommand, mutantNotes string) string {
	ran := ""
	if testsPassed != "" {
		ran = " Away already ran `" + testsPassed + "` on this branch and it passed, so judge the tests' quality and coverage rather than whether they run."
	}
	ran += drillNote(t)
	if mutantNotes != "" {
		ran += "\n\nBefore this review, Away's mutation check (the code under test broken on purpose, one change at a time) sent the work back once with these notes. Check whether the tests now catch each change a listed case covers, or whether the model's reply rightly says no listed case covers it:\n\n<mutation-check>\n" + mutantNotes + "\n</mutation-check>"
	}
	return fmt.Sprintf(`You are checking work done unattended by a local model (Qwen) on branch %s of this repository, while its owner is away. The task it was given:

<task>
%s
</task>

See the change with `+"`git diff %s...HEAD`"+` and read whatever files you need. You may run the project's tests (%s). Do not edit anything.%s

Judge it like a careful reviewer: does it do the whole task, correctly; does it stay in scope (no unrelated changes, no "improvements" nobody asked for); could it break something (look at callers, tests, migrations)?

End your reply with exactly one line of JSON and nothing after it:
{"verdict": "pass" | "revise" | "fail", "notes": "..."}
- pass: good to merge as it is. notes: one or two sentences on what it did.
- revise: notes are concrete, numbered instructions for the model to fix it (file and what to change).
- fail: the task is impossible, unclear or a bad idea as written. notes explain why, for the owner.`, t.Branch, t.Text, t.BaseCommit, testHint(testCommand), ran)
}

// drillNote: for a drill, what the review needs to know and the model wasn't told.
func drillNote(t Task) string {
	if t.Drill == nil {
		return ""
	}
	return fmt.Sprintf("\n\nThis task is a drill: before the model started, Away planted this bug on purpose (its commit is the base of the diff): %s, in %s in %s. The right fix undoes it, or fixes it in an equivalent way. Changing any test, or anything outside %s, is wrong.", t.Drill.What, t.Drill.Function, t.Drill.File, t.Drill.File)
}

func fixPrompt(t Task, notes, testCommand string) string {
	return fmt.Sprintf(`A local model worked on this task unattended on branch %s and its attempts didn't pass review. The task:

<task>
%s
</task>

The last review's notes:
<notes>
%s
</notes>

See what's there with `+"`git diff %s...HEAD`"+`. Finish the task yourself: edit the files so it is done correctly and completely, staying within the task, and run the relevant tests (%s) until they pass. Don't commit, push or switch branches (that's handled for you).

End your reply with exactly one line of JSON and nothing after it:
{"verdict": "fixed" | "fail", "notes": "..."}
- fixed: notes summarize the final change for the owner.
- fail: the task can't be done as written; notes explain why.`, t.Branch, t.Text+drillNote(t), notes, t.BaseCommit, testHint(testCommand))
}

// parseVerdict finds the last {"verdict": …, "notes": …} in Claude's reply.
// No verdict at all counts as "revise" with the whole reply as notes.
//
// It decodes from each "{" backwards with a real JSON decoder. A pattern
// without braces inside missed verdicts whose notes quoted code: on 10-23 a
// drill's review said "Pass" about `json={'bot': {...}}` and was read as
// "revise", so Claude redid work the local model had got right.
func parseVerdict(reply string) (string, string) {
	for end := len(reply); end > 0; {
		i := strings.LastIndex(reply[:end], "{")
		if i < 0 {
			break
		}
		end = i
		var v struct {
			Verdict string `json:"verdict"`
			Notes   string `json:"notes"`
		}
		if json.NewDecoder(strings.NewReader(reply[i:])).Decode(&v) == nil {
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
// commitAll commits everything changed in the worktree except secrets,
// dependencies and the copied local files. base is the commit the task
// started from (see strayUID).
func commitAll(ctx context.Context, wt, msg string, local []string, base string) (string, int, error) {
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
		if skipCommit(p) || contains(local, p) || (e[:2] == "??" && strayUID(ctx, wt, base, p)) {
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

// strayUID: a new Godot .uid file for a script that was already there when
// the task started. Godot writes those for any script missing one when it
// imports a fresh worktree (10-20: debug_perk.gd.uid rode along on a tests
// task). A .uid for a script this branch added is kept, as Skirmish wants.
func strayUID(ctx context.Context, wt, base, p string) bool {
	script, ok := strings.CutSuffix(p, ".uid")
	if !ok || base == "" {
		return false
	}
	_, err := git(ctx, wt, "cat-file", "-e", base+":"+filepath.ToSlash(script))
	return err == nil
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

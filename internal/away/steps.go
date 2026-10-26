package away

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// CLI is the real Steps: Loom's runner for the local model, and the Claude Code CLI.
type CLI struct {
	Runner, ClaudePath, Ollama, Model string
	LogDir                            string
	// Path: folders put in front of PATH for both steps.
	Path []string
	// TestCommands: the projects' own test commands (Settings.Tests), which Claude
	// may run too, besides the usual ones in testTools: Skirmish's Godot
	// wrapper was blocked, so Claude's fix couldn't check its own work.
	TestCommands []string
}

// env is this process's environment with Path in front of PATH, plus extra.
// Python runs in UTF-8 mode: on Windows its default code page can't print
// (or pip read) the Unicode that projects' output and files contain.
func (c CLI) env(extra ...string) []string {
	extra = append([]string{"PYTHONUTF8=1"}, extra...)
	out := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.EqualFold(kv[:i], "PATH") && len(c.Path) > 0 {
			kv = kv[:i+1] + strings.Join(append(append([]string{}, c.Path...), kv[i+1:]), string(os.PathListSeparator))
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

const (
	qwenTimeout   = 60 * time.Minute
	claudeTimeout = 30 * time.Minute
)

// Qwen: `npx vitest run --config tools/qwen/vitest.config.ts` in the Loom
// checkout, pointed at the worktree (see tools/qwen/run.test.ts there).
func (c CLI) Qwen(ctx context.Context, dir, task, tests, transcript string, log *os.File) error {
	if _, err := os.Stat(filepath.Join(c.Runner, "tools", "qwen", "run.test.ts")); err != nil {
		return fmt.Errorf("no tools/qwen/run.test.ts in %s: pull Loom there", c.Runner)
	}
	ctx, cancel := context.WithTimeout(ctx, qwenTimeout)
	defer cancel()
	npx, err := exec.LookPath("npx")
	if err != nil {
		return errors.New("npx isn't on PATH")
	}
	cmd := exec.CommandContext(ctx, npx, "vitest", "run", "--config", "tools/qwen/vitest.config.ts")
	cmd.Dir = c.Runner
	cmd.Env = c.env(
		"QWEN_DIR="+dir, "QWEN_TASK="+task, "QWEN_HOST="+c.Ollama, "QWEN_MODEL="+c.Model,
		"QWEN_OUT="+transcript, "QWEN_MAX_CALLS=60", "CI=1", "NO_COLOR=1", "FORCE_COLOR=0", "QWEN_TESTS="+tests)
	cmd.Stdout, cmd.Stderr = writer(log), writer(log)
	cmd.WaitDelay = 10 * time.Second
	stoppable(cmd)
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("the local model took over an hour and was stopped")
		}
		return fmt.Errorf("the runner exited: %v", err)
	}
	// The runner exits cleanly even when the model never answered (Ollama
	// down or broken): its transcript then ends in an error item.
	if msg := transcriptError(transcript); msg != "" {
		return &ModelError{Msg: msg}
	}
	// No tool call at all means it never got to work on the task: on 10-18
	// Ollama ran with a 4,096-token context, cut the prompt to its last few
	// lines, and Qwen answered those in prose, five tasks in a row.
	if tools, input := transcriptTools(transcript); tools == 0 {
		return &ModelError{Setup: true, Msg: fmt.Sprintf("it answered without using a single tool (its prompt was %d tokens), so it probably never saw the task: check the model, and Ollama's context length", input)}
	}
	return nil
}

// transcriptTools: how many tools the model called, and its prompt's size
// in tokens; -1 calls when the transcript can't be read.
func transcriptTools(path string) (calls, input int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1, 0
	}
	var t struct {
		Usage struct {
			Input int `json:"input"`
		} `json:"usage"`
		Items []struct {
			Kind string `json:"kind"`
		} `json:"items"`
	}
	if json.Unmarshal(b, &t) != nil || len(t.Items) == 0 {
		return -1, 0
	}
	for _, it := range t.Items {
		if it.Kind == "tool" {
			calls++
		}
	}
	return calls, t.Usage.Input
}

// narration: what the model said as it worked, each turn in order with the
// tools it then called, for the review to grade (reasoningGrades). Long
// turns (its summaries) are clipped; so is the whole, keeping its start and
// end. "" when the transcript can't be read.
func narration(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var t struct {
		Items []struct {
			Kind     string `json:"kind"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
			Label    string `json:"label"`
			Name     string `json:"name"`
		} `json:"items"`
	}
	if json.Unmarshal(b, &t) != nil {
		return ""
	}
	var lines []string
	for _, it := range t.Items {
		switch it.Kind {
		case "assistant":
			if s := strings.TrimSpace(it.Thinking); s != "" {
				lines = append(lines, "Qwen (thinking): "+clipText(oneLine(s), 600))
			}
			if s := strings.TrimSpace(it.Text); s != "" {
				lines = append(lines, "Qwen: "+clipText(oneLine(s), 600))
			}
		case "tool":
			name := it.Label
			if name == "" {
				name = it.Name
			}
			lines = append(lines, "  [tool] "+name)
		case "check":
			lines = append(lines, "  [Loom ran the project's check]")
		}
	}
	const max = 12000
	all := strings.Join(lines, "\n")
	if len(all) > max {
		all = all[:max/2] + "\n  [… middle left out …]\n" + all[len(all)-max/2:]
	}
	return all
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ModelError: the local model couldn't be used at all (the task stops there,
// rather than sending an empty attempt to review).
//
// Setup: a sign the model is set up wrong rather than one answer going bad
// (no tool calls at all: it never saw the task). A quick question can't tell
// that from working, so Away stops without asking.
type ModelError struct {
	Msg   string
	Setup bool
}

func (e *ModelError) Error() string { return e.Msg }

// transcriptError is the text of the transcript's last item when that's an error.
func transcriptError(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var t struct {
		Items []struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"items"`
	}
	if json.Unmarshal(b, &t) != nil || len(t.Items) == 0 {
		return ""
	}
	if last := t.Items[len(t.Items)-1]; last.Kind == "error" {
		return firstLine(last.Text, 400)
	}
	return ""
}

// Claude: `claude -p` in the worktree, the prompt on stdin. Reviewing it
// may only read (and look at git); fixing it may also edit files. It can't
// run other commands, commit or push in either case.
func (c CLI) Claude(ctx context.Context, dir, prompt string, edit bool, log *os.File) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, claudeTimeout)
	defer cancel()
	tools, mode := c.claudeTools(edit)
	cmd := exec.CommandContext(ctx, c.ClaudePath, "-p", "--output-format", "json", "--allowedTools", tools, "--permission-mode", mode)
	cmd.Dir = dir
	cmd.Env = c.env()
	cmd.Stdin = strings.NewReader(prompt)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = writer(log)
	cmd.WaitDelay = 10 * time.Second
	stoppable(cmd)
	if log != nil {
		fmt.Fprintf(log, "---- Claude Code (%s)\n", map[bool]string{true: "fixing", false: "reviewing"}[edit])
	}
	err := cmd.Run()
	var res struct {
		Result  string  `json:"result"`
		IsError bool    `json:"is_error"`
		Cost    float64 `json:"total_cost_usd"`
		Turns   int     `json:"num_turns"`
	}
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &res); jerr != nil {
		if err != nil {
			return "", fmt.Errorf("%v: %s", err, firstLine(out.String(), 300))
		}
		return "", fmt.Errorf("unexpected output: %s", firstLine(out.String(), 300))
	}
	if log != nil {
		fmt.Fprintf(log, "%s\n---- (%d turns)\n", res.Result, res.Turns)
	}
	if res.IsError {
		return "", errors.New(firstLine(res.Result, 300))
	}
	return res.Result, nil
}

// claudeTools: reading, git's read commands, the usual test commands and
// the projects' own; nothing else runs. Fixing may also edit files.
func (c CLI) claudeTools(edit bool) (tools, mode string) {
	tools = "Read,Grep,Glob,Bash(git diff:*),Bash(git log:*),Bash(git show:*),Bash(git status:*)," + testTools
	for _, t := range c.TestCommands {
		// A command that chains or redirects would let a prefix match run more than tests.
		if t = strings.TrimSpace(t); t != "" && !strings.ContainsAny(t, "&|;<>`$") {
			tools += ",Bash(" + t + ":*)"
		}
	}
	mode = "default"
	if edit {
		tools += ",Edit,MultiEdit,Write"
		mode = "acceptEdits"
	}
	return tools, mode
}

const testTools = "Bash(python manage.py test:*),Bash(python -m pytest:*),Bash(npm test:*),Bash(npx vitest run:*),Bash(go test:*),Bash(cargo test:*)"

// Tests runs a test command line in the worktree (a shell line, like run_tests).
func (c CLI) Tests(ctx context.Context, dir, command string, log *os.File) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	cmd.Env = c.env("CI=1", "NO_COLOR=1")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 10 * time.Second
	stoppable(cmd)
	if log != nil {
		fmt.Fprintf(log, "---- running %s\n", command)
	}
	err := cmd.Run()
	if log != nil {
		fmt.Fprintf(log, "%s\n---- %s\n", tailText(out.String(), 4000), map[bool]string{true: "passed", false: "failed"}[err == nil])
	}
	if ctx.Err() == context.DeadlineExceeded {
		return false, out.String() + "\n(stopped after 15 minutes)"
	}
	return err == nil, out.String()
}

func writer(f *os.File) io.Writer {
	if f == nil {
		return io.Discard
	}
	return f
}

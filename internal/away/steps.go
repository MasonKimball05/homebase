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
	"strings"
	"time"
)

// CLI is the real Steps: Loom's runner for the local model, and the Claude Code CLI.
type CLI struct {
	Runner, ClaudePath, Ollama, Model string
	LogDir                            string
}

const (
	qwenTimeout   = 60 * time.Minute
	claudeTimeout = 30 * time.Minute
)

// Qwen: `npx vitest run --config tools/qwen/vitest.config.ts` in the Loom
// checkout, pointed at the worktree (see tools/qwen/run.test.ts there).
func (c CLI) Qwen(ctx context.Context, dir, task string, log *os.File) error {
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
	transcript := filepath.Join(c.LogDir, filepath.Base(dir)+".qwen.json")
	cmd.Env = append(os.Environ(),
		"QWEN_DIR="+dir, "QWEN_TASK="+task, "QWEN_HOST="+c.Ollama, "QWEN_MODEL="+c.Model,
		"QWEN_OUT="+transcript, "QWEN_MAX_CALLS=60", "CI=1", "NO_COLOR=1", "FORCE_COLOR=0")
	cmd.Stdout, cmd.Stderr = writer(log), writer(log)
	cmd.WaitDelay = 10 * time.Second
	stoppable(cmd)
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("the local model took over an hour and was stopped")
		}
		return fmt.Errorf("the runner exited: %v", err)
	}
	return nil
}

// Claude: `claude -p` in the worktree, the prompt on stdin. Reviewing it
// may only read (and look at git); fixing it may also edit files. It can't
// run other commands, commit or push in either case.
func (c CLI) Claude(ctx context.Context, dir, prompt string, edit bool, log *os.File) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, claudeTimeout)
	defer cancel()
	tools := "Read,Grep,Glob,Bash(git diff:*),Bash(git log:*),Bash(git show:*),Bash(git status:*)"
	mode := "default"
	if edit {
		tools += ",Edit,MultiEdit,Write"
		mode = "acceptEdits"
	}
	cmd := exec.CommandContext(ctx, c.ClaudePath, "-p", "--output-format", "json", "--allowedTools", tools, "--permission-mode", mode)
	cmd.Dir = dir
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

func writer(f *os.File) io.Writer {
	if f == nil {
		return io.Discard
	}
	return f
}

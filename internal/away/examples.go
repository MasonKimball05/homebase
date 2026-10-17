package away

// Examples: a record of every finished attempt, kept so the local model can
// learn from them. Next, the most similar accepted ones get shown to it when
// a new task starts; later, they're the data for fine-tuning it. One JSON
// file per attempt in ExamplesDir/<project>/, written when the attempt
// finishes and updated with what you did with it (Accept, Discard, Try again).
//
// Author says whose work the final branch is: "qwen" (passed review as the
// local model wrote it), "qwen+claude" (Claude fixed it), or "none" (no
// changes). Each round keeps its own diff, so the local model's own work can
// always be told apart from Claude's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const exampleVersion = 1

type Example struct {
	Version  int       `json:"version"`
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Task     string    `json:"task"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Status and Verdict as the task ended (done/needs-you/failed; pass/fixed/fail).
	Status  string `json:"status"`
	Verdict string `json:"verdict,omitempty"`
	Notes   string `json:"notes,omitempty"`
	Error   string `json:"error,omitempty"`
	Author  string `json:"author"`
	// Outcome is what you did with it: "" (not yet), accepted, discarded, retried, removed.
	Outcome   string    `json:"outcome"`
	OutcomeAt time.Time `json:"outcomeAt,omitzero"`

	Base       string   `json:"base,omitempty"`
	BaseCommit string   `json:"baseCommit,omitempty"`
	Branch     string   `json:"branch,omitempty"`
	Files      []string `json:"files"`
	// Shown: the earlier examples the local model was shown (recall.go).
	Shown     []string       `json:"shown,omitempty"`
	Rounds    []ExampleRound `json:"rounds"`
	FinalDiff string         `json:"finalDiff"`
}

type ExampleRound struct {
	Who     string `json:"who"` // qwen, review, claude-fix
	Verdict string `json:"verdict,omitempty"`
	Notes   string `json:"notes,omitempty"`
	Seconds int    `json:"seconds"`
	Commit  string `json:"commit,omitempty"`
	// Diff is this round's own change (its commit), for qwen and claude-fix rounds.
	Diff string `json:"diff,omitempty"`
	// Transcript is the local model's conversation for qwen rounds (trimmed).
	Transcript json.RawMessage `json:"transcript,omitempty"`
}

// recording collects what an attempt produces beyond the Task itself.
type recording struct {
	transcripts map[int]string // Rounds index -> the runner's transcript file
}

func newRecording() *recording { return &recording{transcripts: map[int]string{}} }

const (
	maxRoundDiff = 200 << 10
	maxFinalDiff = 1 << 20
	maxToolText  = 2000
)

// exampleFile: ExamplesDir/<project>/<id>@<started>.json (Try again makes a new attempt, a new file).
func (s *Service) exampleFile(t Task) string {
	return filepath.Join(s.set.ExamplesDir, safeName(t.Project), fmt.Sprintf("%s@%s.json", t.ID, t.Started.Format("20060102-150405")))
}

// saveExample writes the attempt's example. Called when it finishes, while its branch still exists.
func (s *Service) saveExample(ctx context.Context, repo string, t Task, rec *recording) {
	// Nothing ran (a failure setting up, before the model's first round): nothing to learn from.
	if s.set.ExamplesDir == "" || t.Started.IsZero() || len(t.Rounds) == 0 {
		return
	}
	ex := Example{
		Version: exampleVersion, ID: t.ID, Project: t.Project, Task: t.Text, Started: t.Started, Finished: t.Finished,
		Status: t.Status, Verdict: t.Verdict, Notes: t.Notes, Error: t.Error,
		Base: t.Base, BaseCommit: t.BaseCommit, Branch: t.Branch, Files: []string{}, Rounds: []ExampleRound{}, Shown: t.Shown,
	}
	author := "none"
	for i, r := range t.Rounds {
		er := ExampleRound{Who: r.Who, Verdict: r.Verdict, Notes: r.Notes, Seconds: r.Seconds, Commit: r.Commit}
		if r.Commit != "" && repo != "" {
			if d, err := git(ctx, repo, "show", "--format=", "--no-color", r.Commit); err == nil {
				er.Diff = clipText(d, maxRoundDiff)
			}
			switch {
			case r.Who == "claude-fix":
				author = "qwen+claude"
			case author == "none":
				author = "qwen"
			}
		}
		if p, ok := rec.transcripts[i]; ok {
			if b, err := os.ReadFile(p); err == nil {
				er.Transcript = trimTranscript(b)
			}
		}
		ex.Rounds = append(ex.Rounds, er)
	}
	ex.Author = author
	if repo != "" && t.Branch != "" && t.BaseCommit != "" {
		if d, err := git(ctx, repo, "diff", "--no-color", t.BaseCommit+"..."+t.Branch); err == nil {
			ex.FinalDiff = clipText(d, maxFinalDiff)
		}
		if out, err := git(ctx, repo, "diff", "--name-only", t.BaseCommit+"..."+t.Branch); err == nil {
			ex.Files = strings.Fields(out)
		}
	}
	if err := writeExample(s.exampleFile(t), ex); err != nil {
		s.logf("away: saving the example for %s: %v", t.ID, err)
	}
}

// markExample records what you did with an attempt.
func (s *Service) markExample(t Task, outcome string) {
	if s.set.ExamplesDir == "" || t.Started.IsZero() {
		return
	}
	p := s.exampleFile(t)
	b, err := os.ReadFile(p)
	if err != nil {
		return // never finished (cancelled, paused): no example to mark
	}
	var ex Example
	if json.Unmarshal(b, &ex) != nil {
		return
	}
	ex.Outcome, ex.OutcomeAt = outcome, s.now()
	if err := writeExample(p, ex); err != nil {
		s.logf("away: updating the example for %s: %v", t.ID, err)
	}
}

func writeExample(p string, ex Example) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ex, "", " ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func clipText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n… (cut)\n"
}

// trimTranscript keeps what a reader (or a training set) needs from the
// runner's transcript: what was said, which tools were called with what,
// and their results (clipped), without the model's thinking or huge files.
func trimTranscript(b []byte) json.RawMessage {
	var in struct {
		Model string            `json:"model"`
		Ms    int64             `json:"ms"`
		Usage json.RawMessage   `json:"usage"`
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(b, &in) != nil {
		return nil
	}
	items := make([]map[string]any, 0, len(in.Items))
	for _, raw := range in.Items {
		var it map[string]any
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		kind, _ := it["kind"].(string)
		out := map[string]any{"kind": kind}
		switch kind {
		case "user", "assistant", "note", "error":
			out["text"] = clipAny(it["text"], 8000)
		case "tool":
			out["name"], out["status"] = it["name"], it["status"]
			if a, ok := it["args"].(map[string]any); ok {
				args := map[string]any{}
				for k, v := range a {
					args[k] = clipAny(v, 8000)
				}
				out["args"] = args
			}
			if r, ok := it["result"]; ok {
				out["result"] = clipAny(r, maxToolText)
			}
		case "check":
			out["command"], out["status"] = it["command"], it["status"]
			out["output"] = clipAny(it["output"], maxToolText)
		default:
			continue
		}
		items = append(items, out)
	}
	res, err := json.Marshal(map[string]any{"model": in.Model, "ms": in.Ms, "usage": in.Usage, "items": items})
	if err != nil {
		return nil
	}
	return res
}

func clipAny(v any, max int) any {
	if s, ok := v.(string); ok && len(s) > max {
		return s[:max] + "… (cut)"
	}
	return v
}

// ---- reading them back (the API) ----

// ExampleSummary is one line of the list.
type ExampleSummary struct {
	Name     string    `json:"name"` // <project>/<file>, for /api/away/examples/{name}
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Task     string    `json:"task"`
	Status   string    `json:"status"`
	Verdict  string    `json:"verdict,omitempty"`
	Author   string    `json:"author"`
	Outcome  string    `json:"outcome"`
	Finished time.Time `json:"finished"`
	Files    []string  `json:"files"`
	Shown    int       `json:"shown"` // earlier examples the model was shown (recall.go)
	Stats    Stats     `json:"stats"`
}

// Stats: how an attempt went, worked out from its rounds and transcripts,
// so old examples get them too. For comparing (with examples shown or
// not, one model or another) on real tasks.
type Stats struct {
	QwenRounds  int `json:"qwenRounds"`
	QwenSeconds int `json:"qwenSeconds"`
	ToolCalls   int `json:"toolCalls"` // the local model's, all rounds
	TestRuns    int `json:"testRuns"`  // its run_tests calls
	// PassedFirstReview: Claude passed the local model's first attempt as it was.
	PassedFirstReview bool `json:"passedFirstReview"`
	Reviewed          bool `json:"reviewed"` // a review happened at all
	ClaudeFixed       bool `json:"claudeFixed"`
}

func statsOf(ex Example) Stats {
	var st Stats
	firstReview := true
	for _, r := range ex.Rounds {
		switch r.Who {
		case "qwen":
			st.QwenRounds++
			st.QwenSeconds += r.Seconds
			var tr struct {
				Items []struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"items"`
			}
			if json.Unmarshal(r.Transcript, &tr) == nil {
				for _, it := range tr.Items {
					if it.Kind == "tool" {
						st.ToolCalls++
						if it.Name == "run_tests" {
							st.TestRuns++
						}
					}
				}
			}
		case "review":
			st.Reviewed = true
			if firstReview {
				st.PassedFirstReview = r.Verdict == "pass"
				firstReview = false
			}
		case "claude-fix":
			st.ClaudeFixed = true
		}
	}
	return st
}

// Examples lists the saved attempts, newest first (one project's, or all).
func (s *Service) Examples(project string) ([]ExampleSummary, error) {
	out := []ExampleSummary{}
	if s.set.ExamplesDir == "" {
		return out, nil
	}
	dirs, err := os.ReadDir(s.set.ExamplesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() || (project != "" && d.Name() != safeName(project)) {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(s.set.ExamplesDir, d.Name()))
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.set.ExamplesDir, d.Name(), f.Name()))
			if err != nil {
				continue
			}
			var ex Example
			if json.Unmarshal(b, &ex) != nil {
				continue
			}
			out = append(out, ExampleSummary{Name: d.Name() + "/" + f.Name(), ID: ex.ID, Project: ex.Project, Task: ex.Task, Status: ex.Status, Verdict: ex.Verdict, Author: ex.Author, Outcome: ex.Outcome, Finished: ex.Finished, Files: ex.Files, Shown: len(ex.Shown), Stats: statsOf(ex)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Finished.After(out[j].Finished) })
	return out, nil
}

var exampleName = regexp.MustCompile(`^[A-Za-z0-9._-]+/[0-9]{4}-[0-9]{6}(-[0-9]+)?@[0-9]{8}-[0-9]{6}\.json$`)

// ExampleJSON is one saved attempt, as stored.
func (s *Service) ExampleJSON(name string) ([]byte, error) {
	if s.set.ExamplesDir == "" || !exampleName.MatchString(name) || strings.Contains(name, "..") {
		return nil, errNoTask
	}
	return os.ReadFile(filepath.Join(s.set.ExamplesDir, filepath.FromSlash(name)))
}

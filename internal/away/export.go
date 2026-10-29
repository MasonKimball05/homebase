package away

// Training data: `homebase -export-training <dir>` writes the local model's
// own accepted work as chats to fine-tune it on (docs/fine-tune-plan.md).
//
// In: attempts you accepted that the local model did alone (author "qwen",
// verdict "pass"), drills included; out: anything Claude finished, and any
// attempt a review found misleading (it said it ran or checked something it
// didn't), so the model isn't taught to claim what it didn't do. Each attempt
// is its last local-model round, whose prompt carries the notes it was
// answering: that's the behaviour to learn.
//
// Each line of train.jsonl and heldout.jsonl is one chat in the usual format
// for fine-tuning with tools ({"messages": [...], "tools": [...]}), with what
// the selection may want next to it (drill, era, the reasoning grade). Held
// out: about 15% of tasks, picked by task, never to be trained on.
//
// Transcripts from Loom's runner since 10-26 hold the exact messages Ollama
// was sent. Older ones hold only what the panel shows, so their messages are
// rebuilt from that: the tool results are exact (the panel keeps them as
// sent), the system prompt and the "check your work" message are copied from
// the newest exact transcript of the same project, and one reply's tool
// calls can't always be told from the next reply's. Those lines say
// "exact": false. Anything that can't be rebuilt faithfully is left out and
// counted (a pause by a loop guard, an error, a check that sent work back).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// nudgeLive: when the "say why" rule (QWEN.template.md) and the reasoning log
// went live; attempts before it are era "before".
var nudgeLive = time.Date(2026, 10, 23, 13, 24, 0, 0, time.FixedZone("CDT", -5*3600))

// ExportOptions: where the examples and full transcripts are, and where the
// files go. HeldOutPct 0 means 15.
type ExportOptions struct {
	ExamplesDir, LogDir, Out string
	HeldOutPct               int
}

// ExportReport: what was written, and why the rest wasn't.
type ExportReport struct {
	Train, HeldOut int
	Exact, Rebuilt int
	Skipped        map[string]int
}

func (r ExportReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d chats: %d to train on, %d held out (%d exact, %d rebuilt from older transcripts)\n", r.Train+r.HeldOut, r.Train, r.HeldOut, r.Exact, r.Rebuilt)
	reasons := make([]string, 0, len(r.Skipped))
	for k := range r.Skipped {
		reasons = append(reasons, k)
	}
	sort.Slice(reasons, func(i, j int) bool { return r.Skipped[reasons[i]] > r.Skipped[reasons[j]] })
	for _, k := range reasons {
		fmt.Fprintf(&b, "  left out %3d: %s\n", r.Skipped[k], k)
	}
	return b.String()
}

// chatMessage: a message as fine-tuning tools take it (OpenAI-style tool calls).
type chatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []chatCall `json:"tool_calls,omitempty"`
	Name      string     `json:"name,omitempty"` // tool messages: which tool answered
}

type chatCall struct {
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// trainingChat: one line of the output.
type trainingChat struct {
	ID       string          `json:"id"`
	Project  string          `json:"project"`
	Task     string          `json:"task"`
	Drill    bool            `json:"drill"`
	Era      string          `json:"era"` // "before" or "after" the "say why" rule (nudgeLive)
	Exact    bool            `json:"exact"`
	Rounds   int             `json:"qwenRounds"`
	Quality  int             `json:"quality,omitempty"` // the last review's grade of its reasons, 1-5
	Thinking *ThinkingStats  `json:"thinking,omitempty"`
	Messages []chatMessage   `json:"messages"`
	Tools    json.RawMessage `json:"tools"`
}

// fullTranscript: a runner transcript, as far as the export needs it.
type fullTranscript struct {
	Items []struct {
		Kind     string          `json:"kind"`
		Text     string          `json:"text"`
		Name     string          `json:"name"`
		Label    string          `json:"label"`
		Args     json.RawMessage `json:"args"`
		Output   *string         `json:"output"`
		SentBack bool            `json:"sentBack"`
	} `json:"items"`
	Messages []struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		ToolName  string `json:"tool_name"`
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools json.RawMessage `json:"tools"`
}

// template: what an older transcript's rebuilt messages borrow from an exact one.
type template struct {
	system, selfCheck string
	tools             json.RawMessage
}

const selfCheckNote = "Asked it to check its work against the request"

var roundFile = regexp.MustCompile(`\.round(\d+)\.qwen\.json$`)

// ExportTraining writes train.jsonl and heldout.jsonl in o.Out.
func ExportTraining(o ExportOptions) (ExportReport, error) {
	rep := ExportReport{Skipped: map[string]int{}}
	if o.HeldOutPct == 0 {
		o.HeldOutPct = 15
	}
	latest, err := latestExamples(o.ExamplesDir)
	if err != nil {
		return rep, err
	}
	type picked struct {
		ex Example
		tr *fullTranscript
	}
	var keep []picked
	templates := map[string]template{} // by project: the newest exact transcript's
	newest := map[string]time.Time{}
	for _, ex := range latest {
		if why := exportable(ex); why != "" {
			rep.Skipped[why]++
			continue
		}
		path := lastQwenTranscript(o.LogDir, ex)
		if path == "" {
			rep.Skipped["no full transcript of its last round"]++
			continue
		}
		var tr fullTranscript
		if b, err := os.ReadFile(path); err != nil || json.Unmarshal(b, &tr) != nil {
			rep.Skipped["the transcript can't be read"]++
			continue
		}
		if want := oneLine(ex.Task); !strings.HasPrefix(oneLine(firstUser(&tr)), want[:min(len(want), 80)]) {
			rep.Skipped["the transcript isn't of this task"]++
			continue
		}
		if t, ok := templateOf(&tr); ok && ex.Started.After(newest[ex.Project]) {
			templates[ex.Project], newest[ex.Project] = t, ex.Started
		}
		keep = append(keep, picked{ex, &tr})
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return rep, err
	}
	trainF, err := os.Create(filepath.Join(o.Out, "train.jsonl"))
	if err != nil {
		return rep, err
	}
	defer trainF.Close()
	heldF, err := os.Create(filepath.Join(o.Out, "heldout.jsonl"))
	if err != nil {
		return rep, err
	}
	defer heldF.Close()
	train, held := bufio.NewWriter(trainF), bufio.NewWriter(heldF)
	for _, p := range keep {
		chat := trainingChat{ID: p.ex.ID, Project: p.ex.Project, Task: p.ex.Task, Drill: strings.HasPrefix(p.ex.Task, "A bug has crept into"), Era: "after"}
		if p.ex.Started.Before(nudgeLive) {
			chat.Era = "before"
		}
		for _, r := range p.ex.Rounds {
			switch r.Who {
			case "qwen":
				chat.Rounds++
				chat.Thinking = r.Thinking
			case "review":
				chat.Quality = r.Quality
			}
		}
		if len(p.tr.Messages) > 0 {
			chat.Messages, chat.Tools, chat.Exact = exactMessages(p.tr), p.tr.Tools, true
		} else {
			t, ok := templates[p.ex.Project]
			if !ok {
				rep.Skipped["older transcript, and no exact one of its project to rebuild it from yet"]++
				continue
			}
			msgs, why := rebuildMessages(p.tr, t)
			if why != "" {
				rep.Skipped[why]++
				continue
			}
			chat.Messages, chat.Tools = msgs, t.tools
		}
		chat.Messages = cutRepeats(chat.Messages)
		b, err := json.Marshal(chat)
		if err != nil {
			return rep, err
		}
		w := train
		if heldOutTask(p.ex.ID, o.HeldOutPct) {
			w, rep.HeldOut = held, rep.HeldOut+1
		} else {
			rep.Train++
		}
		if chat.Exact {
			rep.Exact++
		} else {
			rep.Rebuilt++
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := train.Flush(); err != nil {
		return rep, err
	}
	return rep, held.Flush()
}

// latestExamples: every example, the latest attempt of each task.
func latestExamples(dir string) ([]Example, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*", "*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no examples in %s", dir)
	}
	byID := map[string]Example{}
	for _, f := range files {
		// Set aside by hand: the 10-18 rounds that ran with a 4k context.
		if strings.Contains(filepath.Base(f), "4k-context") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var ex Example
		if json.Unmarshal(b, &ex) != nil || ex.ID == "" {
			continue
		}
		if old, ok := byID[ex.ID]; !ok || ex.Started.After(old.Started) {
			byID[ex.ID] = ex
		}
	}
	out := make([]Example, 0, len(byID))
	for _, ex := range byID {
		out = append(out, ex)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// exportable: "" if the attempt belongs in the training data, else why not.
func exportable(ex Example) string {
	switch {
	case ex.Outcome != "accepted":
		return "not accepted"
	case ex.Author != "qwen" || ex.Verdict != "pass":
		return "Claude finished it"
	}
	for _, r := range ex.Rounds {
		if r.Misleading {
			return "a review found what it said misleading"
		}
	}
	return ""
}

// lastQwenTranscript: the full transcript of the attempt's last local-model
// round. Examples since 10-26 name it; for older ones it's the
// highest-numbered round file written while the attempt ran (Try again reuses
// the task's ID, so an earlier attempt's later rounds can still be there).
func lastQwenTranscript(logDir string, ex Example) string {
	for i := len(ex.Rounds) - 1; i >= 0; i-- {
		if r := ex.Rounds[i]; r.Who == "qwen" && r.File != "" {
			if p := filepath.Join(logDir, r.File); fileExists(p) {
				return p
			}
			break
		}
	}
	files, _ := filepath.Glob(filepath.Join(logDir, ex.ID+".round*.qwen.json"))
	best, bestN := "", -1
	for _, f := range files {
		m := roundFile.FindStringSubmatch(f)
		info, err := os.Stat(f)
		if m == nil || err != nil {
			continue
		}
		if t := info.ModTime(); t.Before(ex.Started.Add(-time.Minute)) || (!ex.Finished.IsZero() && t.After(ex.Finished.Add(5*time.Minute))) {
			continue
		}
		if n, _ := strconv.Atoi(m[1]); n > bestN {
			best, bestN = f, n
		}
	}
	return best
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func firstUser(tr *fullTranscript) string {
	for _, m := range tr.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	for _, it := range tr.Items {
		if it.Kind == "user" {
			return it.Text
		}
	}
	return ""
}

// templateOf: what an older transcript of the same project can borrow from this exact one.
func templateOf(tr *fullTranscript) (template, bool) {
	if len(tr.Messages) == 0 || tr.Messages[0].Role != "system" || len(tr.Tools) == 0 {
		return template{}, false
	}
	t := template{system: tr.Messages[0].Content, tools: tr.Tools}
	for _, m := range tr.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "Before you finish, check your work") {
			t.selfCheck = m.Content
		}
	}
	return t, true
}

func exactMessages(tr *fullTranscript) []chatMessage {
	out := make([]chatMessage, 0, len(tr.Messages))
	for _, m := range tr.Messages {
		cm := chatMessage{Role: m.Role, Content: m.Content}
		if m.Role == "tool" {
			cm.Name = m.ToolName
		}
		for _, c := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, newCall(c.Function.Name, c.Function.Arguments))
		}
		out = append(out, cm)
	}
	return out
}

func newCall(name string, args json.RawMessage) chatCall {
	c := chatCall{Type: "function"}
	c.Function.Name = name
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	c.Function.Arguments = args
	return c
}

// rebuildMessages: an older transcript's messages, from what the panel kept
// (see the file comment); a reason when that can't be done faithfully.
func rebuildMessages(tr *fullTranscript, t template) ([]chatMessage, string) {
	msgs := []chatMessage{{Role: "system", Content: t.system}}
	cur := -1 // the assistant message the next tool rows belong to
	for _, it := range tr.Items {
		switch it.Kind {
		case "user":
			msgs, cur = append(msgs, chatMessage{Role: "user", Content: it.Text}), -1
		case "note":
			switch {
			case strings.HasPrefix(it.Text, selfCheckNote):
				if t.selfCheck == "" {
					return nil, "older transcript with a self-check, and no exact one to copy its text from"
				}
				msgs, cur = append(msgs, chatMessage{Role: "user", Content: t.selfCheck}), -1
			case strings.HasPrefix(it.Text, "Paused"):
				return nil, "a loop guard paused it (older transcript: can't rebuild that)"
			}
		case "assistant":
			msgs = append(msgs, chatMessage{Role: "assistant", Content: it.Text})
			cur = len(msgs) - 1
		case "tool":
			if it.Output == nil {
				return nil, "a tool call with no result kept (older transcript)"
			}
			if cur < 0 {
				// Replies with only tool calls aren't kept as rows.
				msgs = append(msgs, chatMessage{Role: "assistant"})
				cur = len(msgs) - 1
			}
			msgs[cur].ToolCalls = append(msgs[cur].ToolCalls, newCall(it.Name, callArgs(it.Name, it.Label, it.Args)))
			msgs = append(msgs, chatMessage{Role: "tool", Name: it.Name, Content: *it.Output})
		case "check":
			if it.SentBack {
				return nil, "the check after changes sent work back (older transcript: can't rebuild that)"
			}
		case "error":
			return nil, "it ended in an error"
		}
	}
	return msgs, ""
}

// cutRepeats ends a chat where the model asked to run tests that had already
// passed, with nothing changed since: the turn that asked becomes its last,
// without the call. What followed was the habit Loom now stops (it asked
// again and again, 10-23 to 10-26: 40 of the first 67 chats), and isn't what
// to learn. Left as it is if it changed any file after, or that turn says
// nothing.
func cutRepeats(msgs []chatMessage) []chatMessage {
	for k, m := range msgs {
		if m.Role != "tool" || !(strings.HasPrefix(m.Content, "Not run: these tests already passed") || strings.HasPrefix(m.Content, "Not run: you were told these tests")) {
			continue
		}
		a := k - 1
		for a >= 0 && msgs[a].Role != "assistant" {
			a--
		}
		if a < 0 || strings.TrimSpace(msgs[a].Content) == "" {
			return msgs
		}
		for _, later := range msgs[a+1:] {
			for _, c := range later.ToolCalls {
				if c.Function.Name == "edit_file" || c.Function.Name == "write_file" {
					return msgs
				}
			}
		}
		last := msgs[a]
		last.ToolCalls = nil
		return append(msgs[:a:a], last)
	}
	return msgs
}

// callArgs: a tool call's arguments as the model wrote them. The panel
// replaces run_tests' with the command it ran, so its target comes from the
// row's label ("Run the tests: src.tests.x").
func callArgs(name, label string, args json.RawMessage) json.RawMessage {
	if name != "run_tests" {
		return args
	}
	target := strings.TrimSpace(strings.TrimPrefix(label, "Run the tests:"))
	if target == "" || target == label {
		return json.RawMessage("{}")
	}
	b, _ := json.Marshal(map[string]string{"target": target})
	return b
}

// heldOutTask: whether a task is in the held-out share, the same way every export.
func heldOutTask(id string, pct int) bool {
	h := fnv.New32a()
	h.Write([]byte(id))
	return int(h.Sum32()%100) < pct
}

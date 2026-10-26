package away

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exportFixture: an example and, optionally, its last round's full transcript.
type exportFixture struct {
	ex         Example
	transcript string // the file's JSON; "" for none
	file       string // its name in the log folder
}

func writeExportFixtures(t *testing.T, fx []exportFixture) (examples, logs string) {
	t.Helper()
	root := t.TempDir()
	examples, logs = filepath.Join(root, "away-examples"), filepath.Join(root, "logs")
	os.MkdirAll(logs, 0o755)
	for _, f := range fx {
		if err := writeExample(filepath.Join(examples, f.ex.Project, f.ex.ID+"@"+f.ex.Started.Format("20060102-150405")+".json"), f.ex); err != nil {
			t.Fatal(err)
		}
		if f.transcript != "" {
			p := filepath.Join(logs, f.file)
			os.WriteFile(p, []byte(f.transcript), 0o644)
			mid := f.ex.Started.Add(f.ex.Finished.Sub(f.ex.Started) / 2)
			os.Chtimes(p, mid, mid)
		}
	}
	return
}

func readChats(t *testing.T, path string) []trainingChat {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []trainingChat
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 10<<20)
	for sc.Scan() {
		var c trainingChat
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func TestExportTraining(t *testing.T) {
	at := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.Local) }
	ex := func(id, task string, start time.Time, author, verdict, outcome string, rounds ...ExampleRound) Example {
		return Example{ID: id, Project: "P", Task: task, Started: start, Finished: start.Add(30 * time.Minute), Author: author, Verdict: verdict, Outcome: outcome, Rounds: rounds}
	}
	// Longer than the 80 characters the transcript's first message is matched on.
	const longTask = "Add tests for a in a.py, in a new file test_a.py, using Django's TestCase. Read the function first, then cover every case."
	qwen := ExampleRound{Who: "qwen", Thinking: &ThinkingStats{Calls: 3, Explained: 67}}
	review := ExampleRound{Who: "review", Verdict: "pass", Quality: 4}

	// Since 10-26: the exact messages, which older transcripts borrow from.
	exact := `{"items":[{"kind":"user","text":"Add tests for b"}],
	 "messages":[
	  {"role":"system","content":"SYSTEM PROMPT"},
	  {"role":"user","content":"Add tests for b"},
	  {"role":"assistant","content":"I'll read b.py because the task names it.","tool_calls":[{"function":{"name":"read_file","arguments":{"path":"b.py"}}}]},
	  {"role":"tool","tool_name":"read_file","content":"def b(): pass"},
	  {"role":"user","content":"Before you finish, check your work against the request. ..."},
	  {"role":"assistant","content":"Done."}],
	 "tools":[{"type":"function","function":{"name":"read_file"}}]}`
	newRound := qwen
	newRound.File = "26-new.round1.qwen.json"

	// Before: only what the panel showed. Its tool-only reply has no row,
	// run_tests' args were replaced by the command, and a self-check note.
	old := `{"items":[
	  {"kind":"user","text":"` + longTask + `\n\nHow similar tasks went before..."},
	  {"kind":"note","text":"Following your instructions."},
	  {"kind":"assistant","text":"Let me read a.py, which the task names:"},
	  {"kind":"tool","name":"read_file","label":"Read a.py","args":{"path":"a.py"},"output":"def a(): return 1"},
	  {"kind":"tool","name":"write_file","label":"Write test_a.py","args":{"path":"test_a.py","content":"..."},"output":"Wrote test_a.py"},
	  {"kind":"tool","name":"run_tests","label":"Run the tests: tests.test_a","args":{"command":"python manage.py test tests.test_a"},"output":"OK"},
	  {"kind":"assistant","text":"The tests pass."},
	  {"kind":"check","status":"passed"},
	  {"kind":"note","text":"Asked it to check its work against the request before finishing."},
	  {"kind":"assistant","text":"1. tests for a: test_a.py"}]}`

	paused := `{"items":[{"kind":"user","text":"Add tests for c"},{"kind":"note","text":"Paused: it repeated its previous reply."}]}`

	examples, logs := writeExportFixtures(t, []exportFixture{
		{ex("23-old", longTask, at(22, 9), "qwen", "pass", "accepted", qwen, review), old, "23-old.round1.qwen.json"},
		{ex("26-new", "Add tests for b", at(26, 9), "qwen", "pass", "accepted", newRound, review), exact, "26-new.round1.qwen.json"},
		{ex("24-claude", "Add tests for d", at(24, 9), "qwen+claude", "fixed", "accepted", qwen), "", ""},
		{ex("24-lied", "Add tests for e", at(24, 10), "qwen", "pass", "accepted", qwen, ExampleRound{Who: "review", Verdict: "pass", Misleading: true}), "", ""},
		{ex("24-no", "Add tests for f", at(24, 11), "qwen", "pass", "discarded", qwen), "", ""},
		{ex("24-paused", "Add tests for c", at(24, 12), "qwen", "pass", "accepted", qwen), paused, "24-paused.round1.qwen.json"},
		{ex("24-drill", "A bug has crept into g in g.py: fix it", at(24, 13), "qwen", "pass", "accepted", qwen), "", ""},
	})
	// A stale round 2 from an earlier attempt, written before this one started: not it.
	stale := filepath.Join(logs, "23-old.round2.qwen.json")
	os.WriteFile(stale, []byte(`{"items":[{"kind":"user","text":"something else"}]}`), 0o644)
	os.Chtimes(stale, at(20, 9), at(20, 9))

	out := filepath.Join(t.TempDir(), "out")
	rep, err := ExportTraining(ExportOptions{ExamplesDir: examples, LogDir: logs, Out: out, HeldOutPct: -1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Train != 2 || rep.HeldOut != 0 || rep.Exact != 1 || rep.Rebuilt != 1 {
		t.Fatalf("%+v\n%s", rep, rep)
	}
	for why, n := range map[string]int{"Claude finished it": 1, "a review found what it said misleading": 1, "not accepted": 1, "no full transcript of its last round": 1} {
		if rep.Skipped[why] != n {
			t.Fatalf("left out for %q: %d, want %d\n%s", why, rep.Skipped[why], n, rep)
		}
	}
	if rep.Skipped["a loop guard paused it (older transcript: can't rebuild that)"] != 1 {
		t.Fatalf("the paused one:\n%s", rep)
	}

	chats := readChats(t, filepath.Join(out, "train.jsonl"))
	byID := map[string]trainingChat{}
	for _, c := range chats {
		byID[c.ID] = c
	}
	n := byID["26-new"]
	if !n.Exact || n.Era != "after" || len(n.Messages) != 6 || n.Messages[3].Role != "tool" || n.Messages[3].Name != "read_file" || n.Quality != 4 {
		t.Fatalf("exact: %+v", n)
	}
	o := byID["23-old"]
	if o.Exact || o.Era != "before" || o.Rounds != 1 || o.Thinking == nil || o.Thinking.Explained != 67 {
		t.Fatalf("rebuilt: %+v", o)
	}
	var shape []string
	for _, m := range o.Messages {
		s := m.Role
		for _, c := range m.ToolCalls {
			s += "+" + c.Function.Name
		}
		shape = append(shape, s)
	}
	want := "system user assistant+read_file+write_file+run_tests tool tool tool assistant user assistant"
	if strings.Join(shape, " ") != want {
		t.Fatalf("rebuilt shape:\n %s\nwant\n %s", strings.Join(shape, " "), want)
	}
	if o.Messages[0].Content != "SYSTEM PROMPT" || !strings.HasPrefix(o.Messages[7].Content, "Before you finish") {
		t.Fatalf("borrowed text: %q / %q", o.Messages[0].Content, o.Messages[7].Content)
	}
	if args := string(o.Messages[2].ToolCalls[2].Function.Arguments); args != `{"target":"tests.test_a"}` {
		t.Fatalf("run_tests args: %s", args)
	}
	if o.Messages[5].Content != "OK" || string(o.Tools) != string(n.Tools) {
		t.Fatalf("tool result / tools: %q %s", o.Messages[5].Content, o.Tools)
	}
}

func TestHeldOutIsByTaskAndStable(t *testing.T) {
	in := 0
	for i := 0; i < 1000; i++ {
		id := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour).Format("0102-150405")
		if heldOutTask(id, 15) != heldOutTask(id, 15) {
			t.Fatal("not stable")
		}
		if heldOutTask(id, 15) {
			in++
		}
	}
	if in < 100 || in > 200 {
		t.Fatalf("%d of 1000 held out at 15%%", in)
	}
}

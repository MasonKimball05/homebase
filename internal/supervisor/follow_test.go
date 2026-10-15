package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func texts(l *Logs) []string {
	var out []string
	for _, ln := range l.Since(0) {
		if ln.Stream == "out" {
			out = append(out, ln.Text)
		}
	}
	return out
}

func waitLines(t *testing.T, l *Logs, want ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Join(texts(l), "|") == strings.Join(want, "|") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("lines = %q, want %q", texts(l), want)
}

func TestFollowFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	logs := NewLogs(filepath.Join(dir, "own.log"), 100, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Not there yet: it waits.
	go followFile(ctx, path, logs, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	if err := os.WriteFile(path, []byte("one\r\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitLines(t, logs, "one", "two")

	// Appended, including a line written in two parts.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("thr")
	time.Sleep(60 * time.Millisecond)
	f.WriteString("ee\n")
	f.Close()
	waitLines(t, logs, "one", "two", "three")

	// Rotated: a new, shorter file under the same name.
	os.Remove(path)
	os.WriteFile(path, []byte("fresh\n"), 0o644)
	waitLines(t, logs, "one", "two", "three", "fresh")
}

func TestFollowFileShowsOnlyTheTailOfABigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	var b strings.Builder
	for b.Len() < 2*followTail {
		b.WriteString("an old line that nobody needs to see again\n")
	}
	b.WriteString("the last line\n")
	os.WriteFile(path, []byte(b.String()), 0o644)
	logs := NewLogs(filepath.Join(dir, "own.log"), 5000, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go followFile(ctx, path, logs, 20*time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(strings.Join(texts(logs), "\n"), "the last line") {
		time.Sleep(20 * time.Millisecond)
	}
	got := texts(logs)
	if len(got) == 0 || got[len(got)-1] != "the last line" {
		t.Fatalf("last line %q", got)
	}
	if lineLen := len("an old line that nobody needs to see again\n"); (len(got)-1)*lineLen > followTail {
		t.Fatalf("read %d lines: more than the tail", len(got))
	}
	for _, line := range got {
		if line != "an old line that nobody needs to see again" && line != "the last line" {
			t.Fatalf("a cut-off line came through: %q", line)
		}
	}
}

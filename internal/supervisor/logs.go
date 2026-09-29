package supervisor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Line is one line of an app's output.
type Line struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Stream string    `json:"stream"` // "out", "err", or "sys" for homebase's own notes
	Text   string    `json:"text"`
}

// Logs keeps the last N lines in memory for the dashboard and appends every
// line to a file on disk, rotating it when it gets big.
type Logs struct {
	mu      sync.Mutex
	lines   []Line // ring buffer
	next    int64  // Seq of the next line
	max     int
	path    string
	maxFile int64
	file    *os.File
	size    int64
}

func NewLogs(path string, keep int, maxFileBytes int64) *Logs {
	return &Logs{max: keep, path: path, maxFile: maxFileBytes, next: 1}
}

// Add records one line. Safe to call from many goroutines.
func (l *Logs) Add(stream, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	ln := Line{Seq: l.next, Time: time.Now(), Stream: stream, Text: text}
	l.next++
	if len(l.lines) < l.max {
		l.lines = append(l.lines, ln)
	} else {
		// Full: drop the oldest. Copying keeps the slice in order, and at
		// a few hundred lines it's cheaper than it sounds.
		copy(l.lines, l.lines[1:])
		l.lines[len(l.lines)-1] = ln
	}
	l.writeFile(ln)
}

// Sysf logs a note from homebase itself (starts, exits, restarts).
func (l *Logs) Sysf(format string, args ...any) {
	l.Add("sys", fmt.Sprintf(format, args...))
}

// Since returns lines with Seq > after, so the dashboard can poll for new output.
func (l *Logs) Since(after int64) []Line {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Line{}
	for _, ln := range l.lines {
		if ln.Seq > after {
			out = append(out, ln)
		}
	}
	return out
}

func (l *Logs) writeFile(ln Line) {
	if l.path == "" {
		return
	}
	if l.file == nil {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
			return
		}
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return // logging to disk is best effort; the in-memory buffer still works
		}
		st, _ := f.Stat()
		l.file, l.size = f, st.Size()
	}
	n, _ := fmt.Fprintf(l.file, "%s [%s] %s\n", ln.Time.Format("2006-01-02 15:04:05"), ln.Stream, ln.Text)
	l.size += int64(n)
	if l.maxFile > 0 && l.size > l.maxFile {
		// Keep one previous file: app.log -> app.log.1
		l.file.Close()
		l.file = nil
		_ = os.Rename(l.path, l.path+".1")
	}
}

func (l *Logs) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}

// lineWriter turns a process's raw output into Lines. Output arrives in
// arbitrary chunks, so a partial line is held until its newline arrives.
type lineWriter struct {
	logs    *Logs
	stream  string
	mu      sync.Mutex
	pending []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		w.logs.Add(w.stream, string(bytes.TrimRight(w.pending[:i], "\r")))
		w.pending = w.pending[i+1:]
	}
	// Guard against a program that never prints a newline.
	if len(w.pending) > 16<<10 {
		w.logs.Add(w.stream, string(w.pending))
		w.pending = nil
	}
	return len(p), nil
}

// flush emits any trailing text without a newline (called when the process exits).
func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		w.logs.Add(w.stream, string(bytes.TrimRight(w.pending, "\r")))
		w.pending = nil
	}
}

package supervisor

// Following a log file another program writes, for apps homebase doesn't
// run itself. Ollama is the case in point: its tray app starts the server
// and logs to %LOCALAPPDATA%\Ollama\server.log, so homebase shows it as
// "external" and has no output of its own to show. With "log_file" set, the
// app's Logs panel follows that file instead.

import (
	"bufio"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// followTail is how much of an existing file is shown at first.
const followTail = 64 << 10

// followFile copies new lines of path into logs until ctx ends, leaving out
// lines that match skip (nil keeps everything). The file may not exist yet,
// may be truncated or replaced (rotation); it copes with all three by
// checking its size and identity every interval.
func followFile(ctx context.Context, path string, logs *Logs, every time.Duration, skip *regexp.Regexp) {
	var (
		f       *os.File
		info    os.FileInfo
		offset  int64
		partial string // a line still being written
		missing bool   // already said the file isn't there
	)
	closeFile := func() {
		if f != nil {
			f.Close()
			f = nil
		}
	}
	defer closeFile()
	open := func(fromEnd bool) bool {
		closeFile()
		file, err := os.Open(path)
		if err != nil {
			if !missing {
				logs.Sysf("waiting for %s to appear", path)
				missing = true
			}
			return false
		}
		st, err := file.Stat()
		if err != nil {
			file.Close()
			return false
		}
		missing = false
		f, info, partial = file, st, ""
		offset = 0
		if fromEnd && st.Size() > followTail {
			offset = st.Size() - followTail
			partial = "\x00" // drop the first, probably cut, line
		}
		logs.Sysf("following %s", path)
		return true
	}
	read := func() {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return
		}
		r := bufio.NewReader(f)
		for {
			chunk, err := r.ReadString('\n')
			offset += int64(len(chunk))
			if strings.HasSuffix(chunk, "\n") {
				line := strings.TrimRight(partial+chunk, "\r\n")
				if strings.HasPrefix(line, "\x00") {
					partial = ""
					continue
				}
				partial = ""
				if skip == nil || !skip.MatchString(line) {
					logs.Add("out", line)
				}
				continue
			}
			partial += chunk
			if err != nil {
				return
			}
		}
	}

	first := true
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if f == nil {
			if open(first) {
				read()
			}
		} else if st, err := os.Stat(path); err != nil || !os.SameFile(st, info) || st.Size() < offset {
			// Gone, replaced, or truncated: start again from the top of the new file.
			if open(false) {
				read()
			}
		} else if st.Size() > offset {
			read()
		}
		first = false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

package projects

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// gitRunner runs git in a repo and returns its output (a field, so tests can
// count calls; the real one is runGit).
type gitRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// quotepath off: names with accents come through as themselves, not \303\251.
	full := append([]string{"-c", "core.quotepath=off", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0") // reading never takes git's index lock
	hideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type repoStatus struct {
	branch string
	files  map[string]string // path ("/"-separated) -> two-letter status, e.g. " M", "??", "A "
}

// status runs `git status` once and parses it.
func (b *Browser) status(ctx context.Context, dir string) (repoStatus, error) {
	out, err := b.git(ctx, dir, "status", "--porcelain=v1", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		return repoStatus{}, err
	}
	return parseStatus(out), nil
}

// parseStatus reads `git status --porcelain=v1 -z --branch`.
func parseStatus(out []byte) repoStatus {
	st := repoStatus{files: map[string]string{}}
	recs := strings.Split(string(out), "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if strings.HasPrefix(r, "## ") {
			br := strings.TrimPrefix(r, "## ")
			br = strings.TrimPrefix(br, "No commits yet on ")
			if j := strings.Index(br, "..."); j >= 0 {
				br = br[:j]
			}
			if j := strings.Index(br, " ["); j >= 0 {
				br = br[:j]
			}
			st.branch = br
			continue
		}
		if len(r) < 4 {
			continue
		}
		code, path := r[:2], r[3:]
		st.files[path] = code
		if code[0] == 'R' || code[0] == 'C' {
			i++ // a rename's old name follows; the new name is the one that exists
		}
	}
	return st
}

func (b *Browser) lastCommit(ctx context.Context, dir string) *Commit {
	out, err := b.git(ctx, dir, "log", "-1", "--format=%h%x00%ct%x00%s")
	if err != nil {
		return nil
	}
	f := strings.SplitN(strings.TrimRight(string(out), "\n"), "\x00", 3)
	if len(f) != 3 {
		return nil
	}
	secs, _ := strconv.ParseInt(f[1], 10, 64)
	return &Commit{Hash: f[0], Subject: f[2], When: time.Unix(secs, 0)}
}

// FileChange is one changed file and its diff against the last commit.
type FileChange struct {
	Path      string `json:"path"`
	Status    string `json:"status"`         // git's two letters
	Kind      string `json:"kind"`           // "added", "modified", "deleted", "renamed", "new" (untracked)
	Diff      string `json:"diff,omitempty"` // unified diff, without the "diff --git" header lines
	Note      string `json:"note,omitempty"` // why there's no diff (binary, secret, too big)
	Truncated bool   `json:"truncated,omitempty"`
}

const (
	maxDiffPerFile = 200 << 10
	maxDiffTotal   = 3 << 20
	maxNewFile     = 256 << 10
)

// changes is every uncommitted change, staged or not, against HEAD. New
// (untracked) files are shown as all-added.
func (b *Browser) changesFor(ctx context.Context, dir string) (repoStatus, []FileChange, error) {
	st, err := b.status(ctx, dir)
	if err != nil {
		return st, nil, err
	}
	diffs := map[string]string{}
	if len(st.files) > 0 {
		// One git call for every tracked change. A repo without commits has no HEAD: diff the index instead.
		args := []string{"diff", "HEAD", "--no-color", "--no-ext-diff", "--find-renames"}
		if _, err := b.git(ctx, dir, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
			args = []string{"diff", "--cached", "--no-color", "--no-ext-diff"}
		}
		if out, err := b.git(ctx, dir, args...); err == nil {
			diffs = splitDiff(string(out))
		}
	}

	var list []FileChange
	total := 0
	for _, path := range sortedKeys(st.files) {
		code := st.files[path]
		fc := FileChange{Path: path, Status: code, Kind: kind(code)}
		switch {
		case secret(path):
			fc.Note = "Changed. Its contents aren't shown: files like this usually hold keys or passwords."
		case code == "??":
			fc.Diff, fc.Note, fc.Truncated = newFileDiff(filepath.Join(dir, filepath.FromSlash(path)))
		default:
			d, ok := diffs[path]
			switch {
			case !ok:
				fc.Note = "No text changes to show (only the file's mode, or it's ignored by the diff)."
			case strings.Contains(d, "\nBinary files ") || strings.HasPrefix(d, "Binary files "):
				fc.Note = "A binary file changed."
			default:
				fc.Diff = d
			}
		}
		if len(fc.Diff) > maxDiffPerFile {
			fc.Diff, fc.Truncated = cutAtLine(fc.Diff, maxDiffPerFile), true
		}
		if total+len(fc.Diff) > maxDiffTotal {
			fc.Diff, fc.Note, fc.Truncated = "", "Not shown: the changes are too large to send in one go. Open the file in the Files tab.", true
		}
		total += len(fc.Diff)
		list = append(list, fc)
	}
	return st, list, nil
}

// splitDiff cuts one `git diff` output into per-file pieces, keyed by the
// file's (new) path, each starting at its first "---"/"@@"/"Binary" line.
func splitDiff(out string) map[string]string {
	res := map[string]string{}
	chunks := strings.Split("\n"+out, "\ndiff --git ")
	for _, c := range chunks[1:] {
		header, body, _ := strings.Cut(c, "\n")
		path := ""
		// Prefer the explicit names git prints below the header.
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "+++ b/") {
				path = strings.TrimPrefix(line, "+++ b/")
				break
			}
			if strings.HasPrefix(line, "rename to ") {
				path = strings.TrimPrefix(line, "rename to ")
			}
			if strings.HasPrefix(line, "--- a/") && path == "" {
				path = strings.TrimPrefix(line, "--- a/") // deleted: "+++ /dev/null"
			}
			if strings.HasPrefix(line, "@@") {
				break
			}
		}
		if path == "" {
			// "a/x b/x": take the b/ side (binary files have no ---/+++ lines).
			if i := strings.LastIndex(header, " b/"); i >= 0 {
				path = header[i+3:]
			}
		}
		// Keep from the first ---, @@ or Binary line: the index/mode lines are noise here.
		start := -1
		for _, marker := range []string{"\n--- ", "\nBinary files ", "\n@@ "} {
			// Searching "\n"+body: index i there is where the line starts in body.
			if i := strings.Index("\n"+body, marker); i >= 0 && (start < 0 || i < start) {
				start = i
			}
		}
		if start < 0 {
			start = 0
		}
		res[path] = strings.TrimRight(body[start:], "\n")
	}
	return res
}

// newFileDiff shows an untracked file as a diff that adds every line.
func newFileDiff(path string) (diff, note string, truncated bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", "New file (couldn't read it).", false
	}
	if info.IsDir() {
		return "", "New folder.", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "New file (couldn't read it).", false
	}
	defer f.Close()
	buf := make([]byte, min(info.Size(), maxNewFile))
	n, _ := f.Read(buf)
	buf = buf[:n]
	if isBinary(buf) {
		return "", "A new binary file.", false
	}
	text := strings.ReplaceAll(string(buf), "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var sb strings.Builder
	fmt.Fprintf(&sb, "@@ -0,0 +1,%d @@\n", len(lines))
	for _, l := range lines {
		sb.WriteString("+" + l + "\n")
	}
	return strings.TrimRight(sb.String(), "\n"), "", info.Size() > maxNewFile
}

func kind(code string) string {
	switch {
	case code == "??":
		return "new"
	case strings.ContainsRune(code, 'R'):
		return "renamed"
	case strings.ContainsRune(code, 'D'):
		return "deleted"
	case strings.ContainsRune(code, 'A'):
		return "added"
	default:
		return "modified"
	}
}

func isBinary(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0
}

func cutAtLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if i := strings.LastIndexByte(s[:max], '\n'); i > 0 {
		return s[:i]
	}
	return s[:max]
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

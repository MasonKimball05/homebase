package projects

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const maxFile = 1 << 20 // larger files are described, not sent

func (b *Browser) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "projects": b.Projects(r.Context())})
}

func (b *Browser) changes(w http.ResponseWriter, r *http.Request) {
	p, ok := b.project(r.Context(), r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such project")
		return
	}
	st, files, err := b.changesFor(r.Context(), p.path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if files == nil {
		files = []FileChange{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": p, "branch": st.branch, "files": files})
}

// Entry is one item in a folder listing.
type Entry struct {
	Name     string    `json:"name"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size,omitempty"`
	Modified time.Time `json:"modified"`
	// Status: git's two letters for a changed file; "changed" for a folder
	// with changes somewhere inside.
	Status string `json:"status,omitempty"`
}

func (b *Browser) tree(w http.ResponseWriter, r *http.Request) {
	p, ok := b.project(r.Context(), r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such project")
		return
	}
	full, rel, err := resolve(p.path, r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := os.ReadDir(full)
	if err != nil {
		writeError(w, http.StatusBadRequest, "not a folder")
		return
	}
	st, _ := b.status(r.Context(), p.path)
	prefix := ""
	if rel != "" {
		prefix = rel + "/"
	}
	entries := []Entry{}
	for _, it := range items {
		if it.Name() == ".git" {
			continue
		}
		info, err := it.Info()
		if err != nil {
			continue
		}
		e := Entry{Name: it.Name(), Dir: info.IsDir() || (info.Mode()&fs.ModeSymlink != 0 && isDir(full, it.Name())), Modified: info.ModTime()}
		path := prefix + it.Name()
		if e.Dir {
			for changed := range st.files {
				if strings.HasPrefix(changed, path+"/") {
					e.Status = "changed"
					break
				}
			}
		} else {
			e.Size = info.Size()
			e.Status = st.files[path]
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "entries": entries})
}

func isDir(dir, name string) bool {
	info, err := os.Stat(dir + string(os.PathSeparator) + name)
	return err == nil && info.IsDir()
}

func (b *Browser) file(w http.ResponseWriter, r *http.Request) {
	p, ok := b.project(r.Context(), r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such project")
		return
	}
	full, rel, err := resolve(p.path, r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusBadRequest, "not a file")
		return
	}
	st, _ := b.status(r.Context(), p.path)
	out := map[string]any{"path": rel, "size": info.Size(), "modified": info.ModTime(), "status": st.files[rel]}
	switch {
	case secret(rel):
		out["note"] = "Not shown: files like this usually hold keys or passwords."
	case info.Size() > maxFile:
		out["note"] = "Too large to show here (over 1 MB)."
	default:
		data, err := os.ReadFile(full)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "couldn't read it")
			return
		}
		if isBinary(data) {
			out["note"] = "A binary file."
		} else {
			out["text"] = string(data)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

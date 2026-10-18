package away

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Register adds the queue's API. Reading is open (like the rest of the
// dashboard, Tailscale only); anything that changes something needs the
// dashboard's CSRF check (X-Homebase), which Loom sends too.
//
//	GET  /api/away                       the mode, idle time and tasks
//	POST /api/away/mode                  {"mode": "off" | "on" | "idle"}
//	POST /api/away/tasks                 {"project": "…", "task": "…"}
//	POST /api/away/tasks/{id}/{action}   cancel, retry, accept, discard, delete
//	GET  /api/away/tasks/{id}/diff       the branch against where it started (text)
//	GET  /api/away/tasks/{id}/log        the end of its log (text)
//	GET  /api/away/suggest?project=      untested functions to give it (suggest.go)
//	POST /api/away/draft                 {"project", "path", "function"}: a task for one, written with the local model
//	GET  /api/away/examples[?project=]   every finished attempt, newest first (examples.go)
//	GET  /api/away/examples/{project}/{file}  one, in full
func (s *Service) Register(mux *http.ServeMux, allowed func(*http.Request) bool) {
	mux.HandleFunc("GET /api/away", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.View())
	})
	change := func(h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if allowed == nil || !allowed(r) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "request not from Loom or the dashboard"})
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /api/away/mode", change(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mode string `json:"mode"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.SetMode(body.Mode); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.View())
	}))
	mux.HandleFunc("POST /api/away/tasks", change(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Project string `json:"project"`
			Task    string `json:"task"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		t, err := s.Add(body.Project, body.Task)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, t)
	}))
	mux.HandleFunc("POST /api/away/tasks/{id}/{action}", change(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Act(r.Context(), r.PathValue("id"), r.PathValue("action")); err != nil {
			code := http.StatusConflict
			if errors.Is(err, errNoTask) {
				code = http.StatusNotFound
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.View())
	}))
	mux.HandleFunc("GET /api/away/tasks/{id}/diff", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Diff(r.Context(), r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeText(w, d)
	})
	mux.HandleFunc("GET /api/away/suggest", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Suggest(r.URL.Query().Get("project"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST /api/away/draft", change(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Project  string `json:"project"`
			Path     string `json:"path"`
			Function string `json:"function"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		d, err := s.DraftTask(r.Context(), body.Project, body.Path, body.Function)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, d)
	}))
	mux.HandleFunc("GET /api/away/examples", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Examples(r.URL.Query().Get("project"))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("GET /api/away/examples/{project}/{file}", func(w http.ResponseWriter, r *http.Request) {
		b, err := s.ExampleJSON(r.PathValue("project") + "/" + r.PathValue("file"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such example"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /api/away/tasks/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		l, err := s.Log(r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeText(w, l)
	})
}

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errors.New("expected JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, s)
}

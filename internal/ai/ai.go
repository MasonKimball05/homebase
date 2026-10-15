// Package ai lets the dashboard, and Loom on the Mac, talk to the models
// Ollama serves on this PC.
//
// Ollama has no login of its own, so it stays bound to 127.0.0.1 and homebase
// passes on exactly three of its endpoints under /ollama/:
//
//	GET  /ollama/api/version   is it there
//	GET  /ollama/api/tags      which models are pulled
//	POST /ollama/api/chat      a (streamed) chat request
//
// Nothing that pulls, deletes, copies or creates models is reachable. A chat
// request must come from the dashboard or a non-browser client (the same
// X-Homebase header and Sec-Fetch-Site checks as the app buttons), carries
// at most MaxBody bytes, and is checked for a model name and messages before
// it's sent on. The answer is streamed back as it arrives, and closing the
// page (or pressing Stop) cancels the request, which stops Ollama generating.
//
// Because the paths match Ollama's own under the /ollama prefix, any Ollama
// client that lets you set a base URL and a header can use it: Loom's
// "localAi.host" is http://arkans-pc1:8090/ollama.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// MaxBody caps a chat request (a long conversation with file contents fits easily).
const MaxBody = 8 << 20

var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,199}$`)

type Proxy struct {
	base         *url.URL
	defaultModel string
	client       *http.Client // for chat: no overall timeout, answers stream for minutes
	quick        *http.Client // for version/tags
}

func New(ollama, defaultModel string) (*Proxy, error) {
	u, err := url.Parse(strings.TrimRight(ollama, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad Ollama address %q", ollama)
	}
	dial := (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	transport := &http.Transport{
		DialContext: dial,
		// Loading a 20 GB model into the GPU can take a minute before the first byte.
		ResponseHeaderTimeout: 5 * time.Minute,
		Proxy:                 nil, // Ollama is on this machine: never via an HTTP proxy
	}
	return &Proxy{
		base:         u,
		defaultModel: defaultModel,
		client:       &http.Client{Transport: transport},
		quick:        &http.Client{Transport: transport, Timeout: 10 * time.Second},
	}, nil
}

// Register adds the routes. allowed decides whether a POST came from the
// dashboard (or a non-browser client) rather than another website.
func (p *Proxy) Register(mux *http.ServeMux, allowed func(*http.Request) bool) {
	mux.HandleFunc("GET /api/ai", p.info)
	// The base address itself isn't an endpoint; someone opening it in a
	// browser gets the page that explains what's here instead of a 404.
	for _, path := range []string{"GET /ollama", "GET /ollama/{$}"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ai.html", http.StatusFound)
		})
	}
	mux.HandleFunc("GET /ollama/api/version", p.passGet("/api/version"))
	mux.HandleFunc("GET /ollama/api/tags", p.passGet("/api/tags"))
	mux.HandleFunc("POST /ollama/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if !allowed(r) {
			writeError(w, http.StatusForbidden, "request not from the dashboard")
			return
		}
		p.chat(w, r)
	})
}

// info tells the chat page which model to start with.
func (p *Proxy) info(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "default_model": p.defaultModel})
}

func (p *Proxy) passGet(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), "GET", p.base.String()+path, nil)
		resp, err := p.quick.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, unreachable(err))
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 4<<20))
	}
}

// CheckChat validates a chat request before it goes to Ollama: a JSON
// object naming a model, with a messages array.
func CheckChat(body []byte) error {
	var req struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("the request must be JSON: %v", err)
	}
	if !modelRe.MatchString(req.Model) {
		return fmt.Errorf("not a model name: %q", req.Model)
	}
	if req.Messages == nil {
		return fmt.Errorf("the request has no messages")
	}
	return nil
}

func (p *Proxy) chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the conversation is too long to send (8 MB at most); start a new one")
		return
	}
	if err := CheckChat(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A fresh request: the browser's Origin, cookies and other headers stay here.
	req, _ := http.NewRequestWithContext(r.Context(), "POST", p.base.String()+"/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return // the page went away
		}
		writeError(w, http.StatusBadGateway, unreachable(err))
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	stream(r.Context(), w, resp.Body)
}

// stream copies the answer through, flushing each chunk so tokens show up as they're generated.
func stream(ctx context.Context, w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil || ctx.Err() != nil {
			return
		}
	}
}

func unreachable(err error) string {
	return "can't reach Ollama on this PC (is it running?): " + err.Error()
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

package ai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeOllama answers like Ollama and records what it was sent.
func fakeOllama(t *testing.T) (*httptest.Server, *http.Request) {
	t.Helper()
	got := &http.Request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r.Clone(r.Context())
		switch r.URL.Path {
		case "/api/version":
			io.WriteString(w, `{"version":"0.32.1"}`)
		case "/api/tags":
			io.WriteString(w, `{"models":[{"name":"qwen3-coder:30b"}]}`)
		case "/api/chat":
			w.Header().Set("Content-Type", "application/x-ndjson")
			io.WriteString(w, `{"message":{"content":"Hi"}}`+"\n")
			w.(http.Flusher).Flush()
			io.WriteString(w, `{"done":true}`+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func handler(t *testing.T, ollama string) http.Handler {
	t.Helper()
	p, err := New(ollama, "qwen3-coder:30b")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	p.Register(mux, func(r *http.Request) bool { return r.Header.Get("X-Homebase") == "1" })
	return mux
}

func call(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPassesVersionAndTags(t *testing.T) {
	o, _ := fakeOllama(t)
	h := handler(t, o.URL)
	if rec := call(h, "GET", "/ollama/api/tags", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "qwen3-coder") {
		t.Errorf("tags: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/ollama/api/version", "", nil); !strings.Contains(rec.Body.String(), "0.32.1") {
		t.Errorf("version: %s", rec.Body)
	}
	if rec := call(h, "GET", "/api/ai", "", nil); !strings.Contains(rec.Body.String(), `"default_model":"qwen3-coder:30b"`) {
		t.Errorf("info: %s", rec.Body)
	}
}

func TestOnlyChatAndListingAreReachable(t *testing.T) {
	o, _ := fakeOllama(t)
	h := handler(t, o.URL)
	for _, path := range []string{"/ollama/api/pull", "/ollama/api/delete", "/ollama/api/create", "/ollama/api/generate"} {
		if rec := call(h, "POST", path, `{"model":"x"}`, map[string]string{"X-Homebase": "1"}); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d", path, rec.Code)
		}
	}
}

func TestChatStreamsThrough(t *testing.T) {
	o, got := fakeOllama(t)
	h := handler(t, o.URL)
	rec := call(h, "POST", "/ollama/api/chat", `{"model":"qwen3-coder:30b","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Homebase": "1", "Origin": "http://evil.example", "Cookie": "a=b"})
	if rec.Code != 200 {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body)
	}
	if lines := strings.Count(rec.Body.String(), "\n"); lines != 2 {
		t.Errorf("want 2 lines, got %q", rec.Body)
	}
	if got.Header.Get("Origin") != "" || got.Header.Get("Cookie") != "" {
		t.Errorf("browser headers leaked to Ollama: %v", got.Header)
	}
}

func TestChatChecks(t *testing.T) {
	o, _ := fakeOllama(t)
	h := handler(t, o.URL)
	ok := map[string]string{"X-Homebase": "1"}
	cases := []struct {
		name    string
		body    string
		headers map[string]string
		want    int
	}{
		{"no dashboard header", `{"model":"m","messages":[]}`, nil, http.StatusForbidden},
		{"not json", `hello`, ok, http.StatusBadRequest},
		{"no model", `{"messages":[]}`, ok, http.StatusBadRequest},
		{"model looks like a flag or path trick", `{"model":"../x","messages":[]}`, ok, http.StatusBadRequest},
		{"no messages", `{"model":"m"}`, ok, http.StatusBadRequest},
		{"too big", `{"model":"m","messages":["` + strings.Repeat("x", MaxBody) + `"]}`, ok, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := call(h, "POST", "/ollama/api/chat", tc.body, tc.headers); rec.Code != tc.want {
				t.Errorf("got %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestOllamaDown(t *testing.T) {
	h := handler(t, "http://127.0.0.1:1")
	start := time.Now()
	rec := call(h, "GET", "/ollama/api/tags", "", nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "can't reach Ollama") {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	if time.Since(start) > 6*time.Second {
		t.Error("took too long to give up")
	}
}

func TestBaseAddressOpensThePage(t *testing.T) {
	h := handler(t, "http://127.0.0.1:1") // never contacted
	for _, path := range []string{"/ollama", "/ollama/"} {
		rec := call(h, "GET", path, "", nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ai.html" {
			t.Errorf("GET %s: %d %q, want a redirect to /ai.html", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	// Only the base itself: other paths under it still aren't passed on.
	if rec := call(h, "GET", "/ollama/api/ps", "", nil); rec.Code == http.StatusFound || rec.Code == http.StatusOK {
		t.Errorf("GET /ollama/api/ps answered %d", rec.Code)
	}
}

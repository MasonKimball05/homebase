package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MasonKimball05/homebase/internal/config"
	"github.com/MasonKimball05/homebase/internal/supervisor"
)

// A manager whose app is never run: enough to exercise the HTTP layer.
func newServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := config.Config{LogDir: t.TempDir(), Apps: []config.App{{Name: "web", Title: "Web", Dir: t.TempDir(), Command: "x"}}}
	return New(supervisor.NewManager(cfg)).Handler()
}

func do(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStatusListsApps(t *testing.T) {
	rec := do(newServer(t), "GET", "/api/status", nil)
	var v statusView
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if len(v.Apps) != 1 || v.Apps[0].Name != "web" || v.Apps[0].State != supervisor.Stopped {
		t.Errorf("status: %+v", v)
	}
}

func TestActionsNeedTheDashboardHeader(t *testing.T) {
	h := newServer(t)
	tests := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{"no header (plain form post from another site)", "/api/apps/web/start", nil, http.StatusForbidden},
		{"header but cross-site", "/api/apps/web/start", map[string]string{"X-Homebase": "1", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"from the dashboard", "/api/apps/web/start", map[string]string{"X-Homebase": "1", "Sec-Fetch-Site": "same-origin"}, http.StatusAccepted},
		{"unknown app", "/api/apps/nope/start", map[string]string{"X-Homebase": "1"}, http.StatusNotFound},
		{"unknown action", "/api/apps/web/delete", map[string]string{"X-Homebase": "1"}, http.StatusConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if rec := do(h, "POST", tc.path, tc.headers); rec.Code != tc.want {
				t.Errorf("got %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestGetCantTriggerActions(t *testing.T) {
	// e.g. <img src="http://arkans-pc1:8090/api/apps/web/stop"> on some page
	if rec := do(newServer(t), "GET", "/api/apps/web/stop", nil); rec.Code == http.StatusAccepted {
		t.Error("GET triggered an action")
	}
}

func TestDashboardHasCSP(t *testing.T) {
	rec := do(newServer(t), "GET", "/", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>homebase</title>") {
		t.Fatalf("index: %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Error("missing CSP")
	}
}

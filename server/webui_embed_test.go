//go:build embedui

package server

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedWorkbenchRoutes(t *testing.T) {
	h := (&Server{}).webuiHandler()
	if len(WebUIBuildID) != 64 {
		t.Fatalf("missing embedded build ID: %q", WebUIBuildID)
	}
	meta := httptest.NewRecorder()
	h.ServeHTTP(meta, httptest.NewRequest("GET", "/build-info.json", nil))
	var info struct {
		SourceID string `json:"source_id"`
	}
	if json.Unmarshal(meta.Body.Bytes(), &info) != nil || info.SourceID != WebUIBuildID || meta.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("stale/missing build metadata")
	}
	assets := 0
	_ = fs.WalkDir(webuiDist, "webui/dist/_next/static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".js") {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", strings.TrimPrefix(p, "webui/dist"), nil))
			if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
				t.Errorf("asset cache: %s", p)
			}
			assets++
		}
		return err
	})
	if assets == 0 {
		t.Fatal("no embedded JS")
	}
	for _, route := range []string{"/chat/", "/system/agents/", "/system/llm/", "/system/tools/", "/settings/", "/function/workspace/", "/system/skills/"} {
		expected, err := webuiDist.ReadFile("webui/dist" + route + "index.html")
		if err != nil {
			t.Fatalf("missing exported page for %s: %v", route, err)
		}
		for _, path := range []string{strings.TrimSuffix(route, "/"), route} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				r := httptest.NewRecorder()
				h.ServeHTTP(r, httptest.NewRequest(method, path, nil))
				if r.Code != http.StatusOK {
					t.Errorf("%s %s: got %d, want 200", method, path, r.Code)
				}
				if r.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("stale page cache: %s", path)
				}
				if !strings.HasPrefix(r.Header().Get("Content-Type"), "text/html") {
					t.Errorf("%s %s: expected HTML content type", method, path)
				}
				if method == http.MethodGet && !bytes.Equal(r.Body.Bytes(), expected) {
					t.Errorf("GET %s did not serve its exported page", path)
				}
				if method == http.MethodHead && r.Body.Len() != 0 {
					t.Errorf("HEAD %s returned a body", path)
				}
			}
		}
	}
	for _, path := range []string{"/dashboard", "/function/tasks", "/system/not-a-route", "/_next/missing.js"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest(method, path, nil))
			if r.Code != http.StatusNotFound {
				t.Errorf("%s %s: got %d", method, path, r.Code)
			}
			if method == http.MethodHead && r.Body.Len() != 0 {
				t.Errorf("HEAD %s returned a body", path)
			}
		}
	}
}

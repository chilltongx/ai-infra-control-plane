package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIHandlerServesEmbeddedAssets(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/", contentType: "text/html", contains: "ForgeGrid"},
		{path: "/app.css", contentType: "text/css", contains: ":root"},
		{path: "/app.js", contentType: "text/javascript", contains: "async function api"},
	} {
		test := test
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			UIHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			if got := response.Header().Get("Content-Type"); !strings.Contains(got, test.contentType) {
				t.Fatalf("Content-Type = %q", got)
			}
			if !strings.Contains(response.Body.String(), test.contains) {
				t.Fatalf("body does not contain %q", test.contains)
			}
		})
	}
}

func TestEmbeddedUIExposesForgeGridResourcesAndAccessibleNavigation(t *testing.T) {
	t.Parallel()

	indexResponse := httptest.NewRecorder()
	UIHandler().ServeHTTP(indexResponse, httptest.NewRequest(http.MethodGet, "/", nil))
	index := indexResponse.Body.String()
	for _, required := range []string{
		`role="tablist"`, `role="tab"`, `aria-selected="true"`, `role="tabpanel"`,
		`Search runs`, `name="gpu_count"`, `name="min_free_gpu_memory_bytes"`,
	} {
		if !strings.Contains(index, required) {
			t.Fatalf("index.html missing %q", required)
		}
	}

	scriptResponse := httptest.NewRecorder()
	UIHandler().ServeHTTP(scriptResponse, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	script := scriptResponse.Body.String()
	for _, required := range []string{
		"resource_requirements", "min_free_gpu_memory_bytes", "last_allocation",
		"last_heartbeat_at", "active_run_id",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("app.js missing %q", required)
		}
	}
	if strings.Contains(script, "/attempts?") {
		t.Fatal("app.js depends on an attempts listing endpoint")
	}
}

func TestUIHandlerDoesNotMaskUnknownRoutes(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	UIHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/not-real", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestUIHandlerHandlesFaviconWithoutConsoleNoise(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	UIHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestEmbeddedUIUsesSessionScopedBearerAuthAndPreservesRequestHeaders(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	UIHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	body := response.Body.String()
	for _, required := range []string{
		"sessionStorage.getItem", "sessionStorage.setItem", `headers.set("Authorization"`,
		"new Headers(options.headers || {})", "fetch(path, { ...options, headers })",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("app.js missing %q", required)
		}
	}
	if strings.Contains(body, "localStorage") {
		t.Fatal("app.js persists API token beyond the browser session")
	}
}

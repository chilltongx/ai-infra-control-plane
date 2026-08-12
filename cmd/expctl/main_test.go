package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExperimentCreateRequest(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/experiments" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "request-1" {
			t.Fatalf("Idempotency-Key = %q", got)
		}
		var body struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Name != "demo" || body.Labels["owner"] != "infra" {
			t.Fatalf("body = %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"exp-1","name":"demo"}`)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := run([]string{"-server", server.URL, "experiment", "create", "-name", "demo", "-label", "owner=infra", "-idempotency-key", "request-1"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"id": "exp-1"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestRunCreatePreservesCommandArguments(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/experiments/exp-1/runs" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body struct {
			Recipe struct {
				Adapter string   `json:"adapter"`
				Command []string `json:"command"`
			} `json:"recipe"`
			ResourceRequirements struct {
				GPUCount              int   `json:"gpu_count"`
				MinFreeGPUMemoryBytes int64 `json:"min_free_gpu_memory_bytes"`
			} `json:"resource_requirements"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Recipe.Adapter != "demo.sleep" || len(body.Recipe.Command) != 1 || body.Recipe.Command[0] != "1.5s" || body.ResourceRequirements.GPUCount != 1 || body.ResourceRequirements.MinFreeGPUMemoryBytes != 16<<30 {
			t.Fatalf("body = %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"run-1"}`)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := run([]string{"-server", server.URL, "run", "create", "-experiment", "exp-1", "-adapter", "demo.sleep", "-gpus", "1", "-min-free-gpu-memory", "17179869184", "--", "1.5s"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRegisterIncludesSessionID(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.SessionID != "manual-session" {
			t.Fatalf("session_id = %q", body.SessionID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"worker-1"}`)
	}))
	defer server.Close()

	if err := run([]string{"-server", server.URL, "worker", "register", "-name", "node", "-session-id", "manual-session"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestServerErrorIncludesRequestID(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"lease_lost","message":"lease is gone","request_id":"req-7"}}`)
	}))
	defer server.Close()

	err := run([]string{"-server", server.URL, "run", "get", "run-1"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "lease_lost") || !strings.Contains(err.Error(), "req-7") {
		t.Fatalf("error = %v", err)
	}
}

func TestStringMapFlagRejectsMalformedValue(t *testing.T) {
	t.Parallel()
	var values stringMapFlag
	if err := values.Set("missing-separator"); err == nil {
		t.Fatal("expected error")
	}
}

func TestEnvironmentBearerTokenIsInjected(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_TOKEN", "cli-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer cli-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	if err := run([]string{"-server", server.URL, "health"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestTokenFileOverridesEnvironmentWithoutRawTokenFlag(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_TOKEN", "environment-secret")
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer file-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	if err := run([]string{"-server", server.URL, "-token-file", tokenPath, "health"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

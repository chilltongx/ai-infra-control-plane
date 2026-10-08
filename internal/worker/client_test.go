package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClaimDecodesFlatAPIShape(t *testing.T) {
	var claim Claim
	if err := json.Unmarshal([]byte(`{"attempt_id":"a1","run_id":"r1","fence":2,"lease_token":"token","expires_at":"2026-08-12T00:00:00Z","recipe":{"adapter":"demo.sleep","command":["1s"]},"allocation":{"worker_id":"w1","gpu_ids":["GPU-a"],"gpus":[{"id":"GPU-a","index":3,"name":"RTX","vendor":"NVIDIA","total_memory_bytes":100,"free_memory_bytes":80}]}}`), &claim); err != nil {
		t.Fatal(err)
	}
	if claim.AttemptID != "a1" || claim.RunID != "r1" || claim.Fence != 2 || claim.Recipe.Adapter != AdapterDemoSleep || len(claim.Allocation.GPUs) != 1 || claim.Allocation.GPUs[0].Index != 3 {
		t.Fatalf("unexpected claim: %#v", claim)
	}
}

func TestClaimSendsSessionID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if got := request.URL.Query().Get("session_id"); got != "" {
			t.Fatalf("session_id leaked into URL: %q", got)
		}
		var body struct {
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.SessionID != "session-one" {
			t.Fatalf("session_id = %q", body.SessionID)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.Claim(context.Background(), "worker-one", "session-one", 30*time.Second, time.Second)
	if err != nil || claim != nil {
		t.Fatalf("Claim() = %#v, %v", claim, err)
	}
}

func TestRegisterSerializesSessionAndResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body RegisterRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.SessionID != "session-one" || body.Resources.ProbeStatus != ResourceProbeOK || len(body.Resources.GPUs) != 1 || body.Resources.GPUs[0].ID != "GPU-a" {
			t.Fatalf("register body = %#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"id":"worker-one"}`))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Register(context.Background(), RegisterRequest{
		Name: "worker", Adapter: AdapterDemoSleep, SessionID: "session-one",
		Resources: WorkerResources{GPUs: []GPUResource{{ID: "GPU-a", Name: "RTX", Vendor: "NVIDIA"}}, ObservedAt: time.Unix(1, 0), ProbeStatus: ResourceProbeOK},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkerHeartbeatSerializesSessionAndResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body WorkerHeartbeatRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.SessionID != "session-one" || body.Resources.ProbeStatus != ResourceProbeUnavailable || body.Resources.GPUs == nil || len(body.Resources.GPUs) != 0 {
			t.Fatalf("heartbeat body = %#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.WorkerHeartbeat(context.Background(), "worker-one", WorkerHeartbeatRequest{
		State: "online", SessionID: "session-one",
		Resources: WorkerResources{GPUs: []GPUResource{}, ObservedAt: time.Unix(1, 0), ProbeStatus: ResourceProbeUnavailable},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAttemptHeartbeatSendsLeaseExtension(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/runs/r1/attempts/a1/heartbeat" {
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		var body struct {
			LeaseToken    string `json:"lease_token"`
			Fence         int64  `json:"fence"`
			ExtendSeconds int64  `json:"extend_seconds"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.LeaseToken != "token" || body.Fence != 2 || body.ExtendSeconds != 600 {
			t.Fatalf("heartbeat body = %+v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"attempt_id":"a1","run_id":"r1","fence":2,"expires_at":"2026-08-12T00:10:00Z"}`))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	expiresAt, err := client.AttemptHeartbeat(context.Background(), Claim{AttemptID: "a1", RunID: "r1", Fence: 2, LeaseToken: "token"}, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, time.August, 12, 0, 10, 0, 0, time.UTC); !expiresAt.Equal(want) {
		t.Fatalf("lease expiry = %s, want %s", expiresAt, want)
	}
}

func TestClaimDecodesNestedInternalShape(t *testing.T) {
	var claim Claim
	if err := json.Unmarshal([]byte(`{"attempt":{"id":"a1","run_id":"r1","fence":2,"lease_token":"token","lease_expires_at":"2026-08-12T00:00:00Z"},"run":{"id":"r1","recipe":{"adapter":"demo.sleep","command":["1s"]}}}`), &claim); err != nil {
		t.Fatal(err)
	}
	if claim.AttemptID != "a1" || claim.RunID != "r1" || claim.Fence != 2 || claim.LeaseToken != "token" || claim.ExpiresAt.IsZero() || claim.Recipe.Adapter != AdapterDemoSleep {
		t.Fatalf("unexpected claim: %#v", claim)
	}
}

func TestRunStateReportsCancellationRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/runs/r1" {
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"r1","state":"active","cancel_requested_at":"2026-08-12T00:00:00Z"}`))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.RunState(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if state != "cancel_requested" {
		t.Fatalf("got state %q", state)
	}
}

func TestHTTPClientInjectsEnvironmentBearerToken(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_TOKEN", "worker-secret")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer worker-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"worker-1"}`))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Register(context.Background(), RegisterRequest{Name: "worker", Adapter: AdapterDemoSleep}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPClientExplicitTokenOverridesEnvironment(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_TOKEN", "environment-secret")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer explicit-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"worker-1"}`))
	}))
	defer server.Close()
	client, err := NewHTTPClientWithToken(server.URL, server.Client(), "explicit-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Register(context.Background(), RegisterRequest{Name: "worker", Adapter: AdapterDemoSleep}); err != nil {
		t.Fatal(err)
	}
}

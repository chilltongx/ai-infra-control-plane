package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubService struct {
	createExperiment func(context.Context, CreateExperimentInput) (Experiment, error)
	createRun        func(context.Context, CreateRunInput) (Run, error)
	claim            func(context.Context, ClaimInput) (Claim, bool, error)
	registerWorker   func(context.Context, RegisterWorkerInput) (Worker, error)
	heartbeatWorker  func(context.Context, string, WorkerHeartbeatInput) (Worker, error)
	listAttempts     func(context.Context, string, int) ([]Attempt, error)
	start            func(context.Context, AttemptMutationInput) (Run, error)
	heartbeat        func(context.Context, AttemptMutationInput) (Lease, error)
	complete         func(context.Context, CompleteAttemptInput) (Run, error)
	readyErr         error
}

func (s stubService) CreateExperiment(ctx context.Context, input CreateExperimentInput) (Experiment, error) {
	if s.createExperiment != nil {
		return s.createExperiment(ctx, input)
	}
	return Experiment{}, nil
}
func (stubService) ListExperiments(context.Context, int) ([]Experiment, error) { return nil, nil }
func (stubService) GetExperiment(context.Context, string) (Experiment, error) {
	return Experiment{}, ErrNotFound
}
func (s stubService) CreateRun(ctx context.Context, input CreateRunInput) (Run, error) {
	if s.createRun != nil {
		return s.createRun(ctx, input)
	}
	return Run{}, nil
}
func (stubService) ListRuns(context.Context, string, int) ([]Run, error) { return nil, nil }
func (stubService) GetRun(context.Context, string) (Run, error)          { return Run{}, ErrNotFound }
func (s stubService) ListAttempts(ctx context.Context, runID string, limit int) ([]Attempt, error) {
	if s.listAttempts != nil {
		return s.listAttempts(ctx, runID, limit)
	}
	return nil, nil
}
func (stubService) CancelRun(context.Context, string, string) (Run, error) {
	return Run{}, nil
}
func (s stubService) RegisterWorker(ctx context.Context, input RegisterWorkerInput) (Worker, error) {
	if s.registerWorker != nil {
		return s.registerWorker(ctx, input)
	}
	return Worker{}, nil
}
func (stubService) ListWorkers(context.Context, int) ([]Worker, error) { return nil, nil }
func (s stubService) HeartbeatWorker(ctx context.Context, id string, input WorkerHeartbeatInput) (Worker, error) {
	if s.heartbeatWorker != nil {
		return s.heartbeatWorker(ctx, id, input)
	}
	return Worker{}, nil
}
func (s stubService) Claim(ctx context.Context, input ClaimInput) (Claim, bool, error) {
	if s.claim != nil {
		return s.claim(ctx, input)
	}
	return Claim{}, false, nil
}
func (s stubService) StartAttempt(ctx context.Context, input AttemptMutationInput) (Run, error) {
	if s.start != nil {
		return s.start(ctx, input)
	}
	return Run{}, nil
}

func (s stubService) HeartbeatAttempt(ctx context.Context, input AttemptMutationInput) (Lease, error) {
	if s.heartbeat != nil {
		return s.heartbeat(ctx, input)
	}
	return Lease{}, nil
}
func (s stubService) CompleteAttempt(ctx context.Context, input CompleteAttemptInput) (Run, error) {
	if s.complete != nil {
		return s.complete(ctx, input)
	}
	return Run{}, nil
}
func (stubService) ListEvents(context.Context, string, int64, int) ([]Event, error) {
	return nil, nil
}
func (s stubService) Ready(context.Context) error { return s.readyErr }

func newTestHandler(service Service) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(service, Options{Logger: logger, Version: "test"}).Handler()
}

func TestCreateExperimentCarriesIdempotencyKey(t *testing.T) {
	t.Parallel()
	service := stubService{createExperiment: func(ctx context.Context, input CreateExperimentInput) (Experiment, error) {
		if got := IdempotencyKey(ctx); got != "create-demo-1" {
			t.Fatalf("IdempotencyKey() = %q", got)
		}
		return Experiment{ID: "exp-1", Name: input.Name}, nil
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/experiments", strings.NewReader(`{"name":"demo"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-demo-1")
	response := httptest.NewRecorder()
	newTestHandler(service).ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Request-ID"); got == "" {
		t.Fatal("missing X-Request-ID")
	}
}

func TestCreateRejectsUnknownJSONField(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/v1/experiments", strings.NewReader(`{"name":"demo","typo":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(stubService{}).ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusBadRequest, "invalid_json")
}

func TestClaimReturnsFullLeaseContract(t *testing.T) {
	t.Parallel()
	expires := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	service := stubService{claim: func(_ context.Context, input ClaimInput) (Claim, bool, error) {
		if input.WorkerID != "worker-1" || input.SessionID != "session-1" || input.LeaseTTL != 45*time.Second || input.WaitTimeout != 2*time.Second {
			t.Fatalf("unexpected claim input: %+v", input)
		}
		return Claim{AttemptID: "attempt-1", RunID: "run-1", Fence: 7, LeaseToken: "plain-secret", ExpiresAt: expires, Recipe: Recipe{Adapter: "local", Command: []string{"echo", "ok"}}, Allocation: ResourceAllocation{WorkerID: "worker-1", GPUIDs: []string{"GPU-1"}}}, true, nil
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/workers/worker-1/claim?lease_ttl=45s&wait=2s", strings.NewReader(`{"session_id":"session-1"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(service).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var got Claim
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.LeaseToken != "plain-secret" || got.Fence != 7 || got.Recipe.Adapter != "local" || got.Allocation.GPUIDs[0] != "GPU-1" {
		t.Fatalf("claim = %+v", got)
	}
}

func TestCreateRunCarriesResourceRequirements(t *testing.T) {
	t.Parallel()
	service := stubService{createRun: func(_ context.Context, input CreateRunInput) (Run, error) {
		if input.ResourceRequirements.GPUCount != 2 || input.ResourceRequirements.MinFreeGPUMemoryBytes != 16<<30 {
			t.Fatalf("requirements = %+v", input.ResourceRequirements)
		}
		return Run{ID: "run-1"}, nil
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/experiments/exp-1/runs", strings.NewReader(`{"recipe":{"adapter":"demo.sleep","command":["1s"]},"resource_requirements":{"gpu_count":2,"min_free_gpu_memory_bytes":17179869184}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(service).ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkerHeartbeatCarriesStructuredResources(t *testing.T) {
	t.Parallel()
	service := stubService{heartbeatWorker: func(_ context.Context, id string, input WorkerHeartbeatInput) (Worker, error) {
		if id != "worker-1" || input.SessionID != "session-1" || input.Resources == nil || input.Resources.ProbeStatus != "ok" || input.Resources.GPUs[0].FreeMemoryBytes != 20<<30 {
			t.Fatalf("heartbeat = %q %+v", id, input)
		}
		return Worker{ID: id}, nil
	}}
	body := `{"session_id":"session-1","state":"online","resources":{"probe_status":"ok","gpus":[{"id":"GPU-1","index":0,"name":"RTX","vendor":"nvidia","total_memory_bytes":25769803776,"free_memory_bytes":21474836480}]}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/workers/worker-1/heartbeat", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(service).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRegisterWorkerRequiresSessionID(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/v1/workers", strings.NewReader(`{"name":"node","adapter":"demo.sleep"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(stubService{}).ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusBadRequest, "invalid_argument")
}

func TestAttemptListDoesNotExposeLeaseToken(t *testing.T) {
	t.Parallel()
	service := stubService{listAttempts: func(_ context.Context, id string, _ int) ([]Attempt, error) {
		return []Attempt{{ID: "attempt-1", RunID: id, WorkerID: "worker-1", Fence: 2, Allocation: &ResourceAllocation{GPUIDs: []string{"GPU-1"}}}}, nil
	}}
	response := httptest.NewRecorder()
	newTestHandler(service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/runs/run-1/attempts", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "lease_token") || !strings.Contains(response.Body.String(), "GPU-1") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClaimNoWorkReturnsNoContent(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/v1/workers/worker-1/claim", strings.NewReader(`{"session_id":"session-1"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(stubService{}).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestStartRequiresLeaseTokenAndFence(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/attempts/attempt-1/start", strings.NewReader(`{"lease_token":"token","fence":0}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(stubService{}).ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusBadRequest, "invalid_argument")
}

func TestAttemptHeartbeatCarriesExtension(t *testing.T) {
	t.Parallel()
	service := stubService{heartbeat: func(_ context.Context, input AttemptMutationInput) (Lease, error) {
		if input.RunID != "run-1" || input.AttemptID != "attempt-1" || input.ExtendSeconds != 600 {
			t.Fatalf("unexpected heartbeat input: %+v", input)
		}
		return Lease{AttemptID: input.AttemptID, RunID: input.RunID, Fence: input.Fence}, nil
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/attempts/attempt-1/heartbeat", strings.NewReader(`{"lease_token":"token","fence":1,"extend_seconds":600}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(service).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestAttemptHeartbeatRejectsExtensionAboveMaximum(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/attempts/attempt-1/heartbeat", strings.NewReader(`{"lease_token":"token","fence":1,"extend_seconds":601}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(stubService{}).ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusBadRequest, "invalid_argument")
}

func TestAttemptHeartbeatRejectsNegativeExtension(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/attempts/attempt-1/heartbeat", strings.NewReader(`{"lease_token":"token","fence":1,"extend_seconds":-1}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(stubService{}).ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusBadRequest, "invalid_argument")
}

func TestAttemptHeartbeatRequiresExtension(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/attempts/attempt-1/heartbeat", strings.NewReader(`{"lease_token":"token","fence":1}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(stubService{}).ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusBadRequest, "invalid_argument")
}

func TestCompleteAcceptsCancelledOutcome(t *testing.T) {
	t.Parallel()
	service := stubService{complete: func(_ context.Context, input CompleteAttemptInput) (Run, error) {
		if input.Outcome != "cancelled" {
			t.Fatalf("outcome = %q", input.Outcome)
		}
		return Run{ID: input.RunID, State: "cancelled"}, nil
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/attempts/attempt-1/complete", strings.NewReader(`{"lease_token":"token","fence":1,"outcome":"cancelled"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(service).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestLeaseLostMapsToConflict(t *testing.T) {
	t.Parallel()
	service := stubService{start: func(context.Context, AttemptMutationInput) (Run, error) {
		return Run{}, ErrLeaseLost
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/attempts/attempt-1/start", strings.NewReader(`{"lease_token":"token","fence":1}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestHandler(service).ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusConflict, "lease_lost")
}

func TestReadyFailureDoesNotLeakError(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	newTestHandler(stubService{readyErr: errors.New("secret database path")}).ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusServiceUnavailable, "unavailable")
	if strings.Contains(response.Body.String(), "secret database path") {
		t.Fatalf("response leaked internal error: %s", response.Body.String())
	}
}

func TestListUsesEmptyArray(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/v1/experiments", nil)
	response := httptest.NewRecorder()
	newTestHandler(stubService{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"items":[]`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestMetricsUsesRouteTemplate(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(stubService{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/runs/run-secret", nil))
	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(metrics.Body.String(), "run-secret") {
		t.Fatalf("metrics contain high-cardinality resource ID: %s", metrics.Body.String())
	}
	if !strings.Contains(metrics.Body.String(), `/v1/runs/{run_id}`) {
		t.Fatalf("metrics missing route template: %s", metrics.Body.String())
	}
}

func TestUIRoutesAreExactAndDoNotMaskAPIMethodHandling(t *testing.T) {
	t.Parallel()
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ui"))
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(stubService{}, Options{Logger: logger, UIHandler: ui}).Handler()

	root := httptest.NewRecorder()
	handler.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK || root.Body.String() != "ui" {
		t.Fatalf("root status/body = %d/%q", root.Code, root.Body.String())
	}

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if unknown.Code != http.StatusNotFound || unknown.Body.String() == "ui" {
		t.Fatalf("unknown status/body = %d/%q", unknown.Code, unknown.Body.String())
	}

	method := httptest.NewRecorder()
	handler.ServeHTTP(method, httptest.NewRequest(http.MethodPut, "/v1/experiments", nil))
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d, want 405", method.Code)
	}
}

func TestBearerAuthProtectsAPIAndMetricsButLeavesProbesOpen(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(stubService{}, Options{Logger: logger, APIToken: "correct-secret"}).Handler()

	for _, path := range []string{"/v1/experiments", "/metrics"} {
		unauthorized := httptest.NewRecorder()
		handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, path, nil))
		assertAPIError(t, unauthorized, http.StatusUnauthorized, "unauthorized")
		if got := unauthorized.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
			t.Fatalf("%s WWW-Authenticate = %q", path, got)
		}

		wrong := httptest.NewRecorder()
		wrongRequest := httptest.NewRequest(http.MethodGet, path, nil)
		wrongRequest.Header.Set("Authorization", "Bearer wrong-secret")
		handler.ServeHTTP(wrong, wrongRequest)
		assertAPIError(t, wrong, http.StatusUnauthorized, "unauthorized")

		authorized := httptest.NewRecorder()
		authorizedRequest := httptest.NewRequest(http.MethodGet, path, nil)
		authorizedRequest.Header.Set("Authorization", "Bearer correct-secret")
		handler.ServeHTTP(authorized, authorizedRequest)
		if authorized.Code != http.StatusOK {
			t.Fatalf("%s authorized status = %d, body = %s", path, authorized.Code, authorized.Body.String())
		}
	}

	for _, path := range []string{"/healthz", "/readyz", "/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code == http.StatusUnauthorized {
			t.Fatalf("probe/UI path %s unexpectedly requires auth", path)
		}
	}
}

func TestEmptyBearerTokenLeavesAPIOpen(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	newTestHandler(stubService{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/experiments", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, status, response.Body.String())
	}
	var envelope errorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != code || envelope.Error.RequestID == "" {
		t.Fatalf("error = %+v", envelope.Error)
	}
}

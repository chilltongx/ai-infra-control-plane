package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chilltongx/ai-infra-control-plane/internal/domain"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

func newTestStore(t *testing.T) (*JSONStore, *testClock, string) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC)}
	var sequence atomic.Int64
	path := filepath.Join(t.TempDir(), "control-plane.json")
	store, err := NewJSONStore(
		path,
		WithClock(clock.Now),
		WithIDGenerator(func(prefix string) (string, error) {
			return prefix + "_" + time.Unix(0, sequence.Add(1)).UTC().Format("150405.000000000"), nil
		}),
		WithTokenGenerator(func() (string, error) {
			return "secret-token-" + time.Unix(0, sequence.Add(1)).UTC().Format("150405.000000000"), nil
		}),
	)
	if err != nil {
		t.Fatalf("NewJSONStore() error = %v", err)
	}
	return store, clock, path
}

func createExperimentAndRun(t *testing.T, store *JSONStore, maxAttempts int) (domain.Experiment, domain.Run) {
	t.Helper()
	ctx := context.Background()
	experimentResult, err := store.CreateExperiment(ctx, CreateExperimentParams{Name: "SGLang serving benchmark"})
	if err != nil {
		t.Fatalf("CreateExperiment() error = %v", err)
	}
	runResult, err := store.CreateRun(ctx, CreateRunParams{
		ExperimentID:   experimentResult.Experiment.ID,
		Recipe:         domain.Recipe{Adapter: "local", Command: []string{"python", "-m", "sglang.benchmark.serving"}},
		RequiredLabels: map[string]string{"gpu": "nvidia"},
		MaxAttempts:    maxAttempts,
	})
	if err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	return experimentResult.Experiment, runResult.Run
}

func registerWorker(t *testing.T, store *JSONStore, id string) domain.Worker {
	t.Helper()
	worker, err := store.RegisterWorker(context.Background(), RegisterWorkerParams{
		ID: id, Name: id, Adapter: "local", Labels: map[string]string{"gpu": "nvidia"},
	})
	if err != nil {
		t.Fatalf("RegisterWorker() error = %v", err)
	}
	return worker
}

func registerGPUWorker(t *testing.T, store *JSONStore, id string, freeBytes ...int64) domain.Worker {
	t.Helper()
	gpus := make([]domain.GPUResource, len(freeBytes))
	for index, free := range freeBytes {
		gpus[index] = domain.GPUResource{ID: fmt.Sprintf("GPU-%d", index), Index: index, Name: "NVIDIA Test", Vendor: "nvidia", TotalMemoryBytes: 24 << 30, FreeMemoryBytes: free}
	}
	worker, err := store.RegisterWorker(context.Background(), RegisterWorkerParams{
		ID: id, Name: id, Adapter: "local", SessionID: "session-" + id,
		Labels:    map[string]string{"gpu": "nvidia"},
		Resources: domain.WorkerResources{GPUs: gpus, ProbeStatus: domain.ResourceProbeOK},
	})
	if err != nil {
		t.Fatalf("RegisterWorker() error = %v", err)
	}
	return worker
}

func createResourceRun(t *testing.T, repository *JSONStore, requirements domain.ResourceRequirements, priority int) domain.Run {
	t.Helper()
	experiment, err := repository.CreateExperiment(context.Background(), CreateExperimentParams{Name: fmt.Sprintf("resource-%d", priority)})
	if err != nil {
		t.Fatal(err)
	}
	run, err := repository.CreateRun(context.Background(), CreateRunParams{
		ExperimentID:         experiment.Experiment.ID,
		Recipe:               domain.Recipe{Adapter: "local", Command: []string{"1s"}},
		RequiredLabels:       map[string]string{"gpu": "nvidia"},
		ResourceRequirements: requirements, Priority: priority, MaxAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run.Run
}

func TestClaimMatchesGPUCapacityAndCapturesBestFitAllocation(t *testing.T) {
	repository, _, _ := newTestStore(t)
	run := createResourceRun(t, repository, domain.ResourceRequirements{GPUCount: 1, MinFreeGPUMemoryBytes: 16 << 30}, 1)
	registerGPUWorker(t, repository, "gpu-worker", 20<<30, 18<<30, 8<<30)
	claim, err := repository.Claim(context.Background(), ClaimParams{WorkerID: "gpu-worker", SessionID: "session-gpu-worker", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Run.ID != run.ID || len(claim.Allocation.GPUs) != 1 || claim.Allocation.GPUs[0].ID != "GPU-1" {
		t.Fatalf("claim allocation = %#v", claim.Allocation)
	}
	stored, err := repository.GetAttempt(context.Background(), claim.Attempt.ID)
	if err != nil || stored.Allocation == nil || stored.Allocation.GPUIDs[0] != "GPU-1" {
		t.Fatalf("stored attempt = %#v, %v", stored, err)
	}
}

func TestClaimResultAllocationDoesNotAliasPersistedState(t *testing.T) {
	repository, _, _ := newTestStore(t)
	run := createResourceRun(t, repository, domain.ResourceRequirements{GPUCount: 1, MinFreeGPUMemoryBytes: 16 << 30}, 1)
	registerGPUWorker(t, repository, "gpu-worker", 20<<30)
	claim, err := repository.Claim(context.Background(), ClaimParams{WorkerID: "gpu-worker", SessionID: "session-gpu-worker", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claim.Allocation.GPUIDs[0] = "tampered-id"
	claim.Allocation.GPUs[0].ID = "tampered-snapshot"
	attempt, err := repository.GetAttempt(context.Background(), claim.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedRun, err := repository.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Allocation.GPUIDs[0] != "GPU-0" || attempt.Allocation.GPUs[0].ID != "GPU-0" || storedRun.LastAllocation.GPUIDs[0] != "GPU-0" {
		t.Fatalf("caller mutation reached persisted allocation: attempt=%#v run=%#v", attempt.Allocation, storedRun.LastAllocation)
	}
}

func TestActiveAllocationSurvivesUnavailableResourceHeartbeat(t *testing.T) {
	repository, _, _ := newTestStore(t)
	createResourceRun(t, repository, domain.ResourceRequirements{GPUCount: 1, MinFreeGPUMemoryBytes: 16 << 30}, 1)
	registerGPUWorker(t, repository, "gpu-worker", 20<<30)
	claim, err := repository.Claim(context.Background(), ClaimParams{WorkerID: "gpu-worker", SessionID: "session-gpu-worker", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	unavailable := domain.WorkerResources{ProbeStatus: domain.ResourceProbeUnavailable}
	worker, err := repository.HeartbeatWorker(context.Background(), WorkerHeartbeatParams{
		WorkerID: "gpu-worker", SessionID: "session-gpu-worker", Resources: &unavailable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worker.Resources.ProbeStatus != domain.ResourceProbeUnavailable || worker.ActiveAttemptID != claim.Attempt.ID {
		t.Fatalf("worker after unavailable probe = %#v", worker)
	}
	attempt, err := repository.GetAttempt(context.Background(), claim.Attempt.ID)
	if err != nil || attempt.Allocation == nil || attempt.Allocation.GPUIDs[0] != "GPU-0" {
		t.Fatalf("immutable allocation after live probe change = %#v, %v", attempt.Allocation, err)
	}
}

func TestResourceInsufficientHighPriorityRunDoesNotBlockEligibleRun(t *testing.T) {
	repository, _, _ := newTestStore(t)
	blocked := createResourceRun(t, repository, domain.ResourceRequirements{GPUCount: 2, MinFreeGPUMemoryBytes: 16 << 30}, 100)
	eligible := createResourceRun(t, repository, domain.ResourceRequirements{GPUCount: 1, MinFreeGPUMemoryBytes: 8 << 30}, 1)
	registerGPUWorker(t, repository, "gpu-worker", 12<<30)
	claim, err := repository.Claim(context.Background(), ClaimParams{WorkerID: "gpu-worker", SessionID: "session-gpu-worker", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Run.ID != eligible.ID || claim.Run.ID == blocked.ID {
		t.Fatalf("claimed %q", claim.Run.ID)
	}
}

func TestSameWorkerCannotClaimTwoRunsConcurrently(t *testing.T) {
	repository, _, _ := newTestStore(t)
	createResourceRun(t, repository, domain.ResourceRequirements{}, 1)
	createResourceRun(t, repository, domain.ResourceRequirements{}, 1)
	registerGPUWorker(t, repository, "gpu-worker", 20<<30)
	type response struct{ err error }
	start := make(chan struct{})
	responses := make(chan response, 2)
	for range 2 {
		go func() {
			<-start
			_, err := repository.Claim(context.Background(), ClaimParams{WorkerID: "gpu-worker", SessionID: "session-gpu-worker", LeaseTTL: time.Minute})
			responses <- response{err: err}
		}()
	}
	close(start)
	winners, noWork := 0, 0
	for range 2 {
		result := <-responses
		if result.err == nil {
			winners++
		} else if errors.Is(result.err, ErrNoWork) {
			noWork++
		} else {
			t.Fatal(result.err)
		}
	}
	if winners != 1 || noWork != 1 {
		t.Fatalf("winners/no_work = %d/%d", winners, noWork)
	}
}

func TestStaleWorkerIsOfflineAndCannotClaim(t *testing.T) {
	repository, clock, _ := newTestStore(t)
	createResourceRun(t, repository, domain.ResourceRequirements{}, 1)
	registerGPUWorker(t, repository, "gpu-worker", 20<<30)
	clock.Advance(31 * time.Second)
	workers, err := repository.ListWorkers(context.Background())
	if err != nil || workers[0].State != domain.WorkerOffline {
		t.Fatalf("workers = %#v, %v", workers, err)
	}
	_, err = repository.Claim(context.Background(), ClaimParams{WorkerID: "gpu-worker", SessionID: "session-gpu-worker", LeaseTTL: time.Minute})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("claim error = %v", err)
	}
}

func TestLeaseExpiryReleasesWorkerReservation(t *testing.T) {
	repository, clock, _ := newTestStore(t)
	createResourceRun(t, repository, domain.ResourceRequirements{}, 1)
	registerGPUWorker(t, repository, "gpu-worker", 20<<30)
	if _, err := repository.Claim(context.Background(), ClaimParams{WorkerID: "gpu-worker", SessionID: "session-gpu-worker", LeaseTTL: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Second)
	if _, err := repository.ExpireLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	worker, err := repository.ListWorkers(context.Background())
	if err != nil || worker[0].ActiveAttemptID != "" || worker[0].ActiveRunID != "" {
		t.Fatalf("worker = %#v, %v", worker, err)
	}
}

func TestNewWorkerSessionCannotReplaceActiveAllocation(t *testing.T) {
	repository, clock, _ := newTestStore(t)
	createResourceRun(t, repository, domain.ResourceRequirements{}, 1)
	registerGPUWorker(t, repository, "gpu-worker", 20<<30)
	if _, err := repository.Claim(context.Background(), ClaimParams{WorkerID: "gpu-worker", SessionID: "session-gpu-worker", LeaseTTL: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	_, err := repository.RegisterWorker(context.Background(), RegisterWorkerParams{
		ID: "gpu-worker", Name: "replacement", Adapter: "local", SessionID: "new-session",
		Labels: map[string]string{"gpu": "nvidia"}, Resources: domain.WorkerResources{ProbeStatus: domain.ResourceProbeUnavailable},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("replacement RegisterWorker() error = %v, want ErrConflict", err)
	}
	clock.Advance(11 * time.Second)
	if _, err := repository.ExpireLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	worker, err := repository.RegisterWorker(context.Background(), RegisterWorkerParams{
		ID: "gpu-worker", Name: "replacement", Adapter: "local", SessionID: "new-session",
		Labels: map[string]string{"gpu": "nvidia"}, Resources: domain.WorkerResources{ProbeStatus: domain.ResourceProbeUnavailable},
	})
	if err != nil || worker.SessionID != "new-session" {
		t.Fatalf("replacement after expiry = %#v, %v", worker, err)
	}
}

func TestSnapshotLoadMigratesActiveWorkerReservation(t *testing.T) {
	repository, clock, path := newTestStore(t)
	createResourceRun(t, repository, domain.ResourceRequirements{}, 1)
	registerGPUWorker(t, repository, "gpu-worker", 20<<30)
	claim, err := repository.Claim(context.Background(), ClaimParams{WorkerID: "gpu-worker", SessionID: "session-gpu-worker", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	legacy := cloneValue(repository.state)
	worker := legacy.Workers["gpu-worker"]
	worker.ActiveRunID = ""
	worker.ActiveAttemptID = ""
	legacy.Workers[worker.ID] = worker
	legacyAttempt := legacy.Attempts[claim.Attempt.ID]
	legacyAttempt.Allocation = nil
	legacy.Attempts[legacyAttempt.ID] = legacyAttempt
	legacyRun := legacy.Runs[claim.Run.ID]
	legacyRun.LastAllocation = nil
	legacy.Runs[legacyRun.ID] = legacyRun
	if err := writeSnapshotAtomic(path, legacy); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewJSONStore(path, WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	workers, err := reopened.ListWorkers(context.Background())
	if err != nil || len(workers) != 1 {
		t.Fatalf("workers = %#v, %v", workers, err)
	}
	if workers[0].ActiveRunID != claim.Run.ID || workers[0].ActiveAttemptID != claim.Attempt.ID {
		t.Fatalf("migrated worker = %#v", workers[0])
	}
	attempt, err := reopened.GetAttempt(context.Background(), claim.Attempt.ID)
	if err != nil || attempt.Allocation == nil || attempt.Allocation.WorkerID != "gpu-worker" {
		t.Fatalf("migrated attempt = %#v, %v", attempt, err)
	}
}

func TestCreateExperimentIdempotencyPersistsAcrossRestart(t *testing.T) {
	store, clock, path := newTestStore(t)
	ctx := context.Background()
	params := CreateExperimentParams{
		Name: "benchmark", Labels: map[string]string{"owner": "infra"}, IdempotencyKey: "request-1",
	}
	first, err := store.CreateExperiment(ctx, params)
	if err != nil {
		t.Fatalf("first CreateExperiment() error = %v", err)
	}
	if first.Replayed {
		t.Fatal("first creation unexpectedly replayed")
	}

	reopened, err := NewJSONStore(path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("reopen store error = %v", err)
	}
	second, err := reopened.CreateExperiment(ctx, params)
	if err != nil {
		t.Fatalf("replayed CreateExperiment() error = %v", err)
	}
	if !second.Replayed || second.Experiment.ID != first.Experiment.ID {
		t.Fatalf("replayed result = %#v, want ID %q and Replayed", second, first.Experiment.ID)
	}

	params.Name = "different payload"
	if _, err := reopened.CreateExperiment(ctx, params); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting CreateExperiment() error = %v, want ErrIdempotencyConflict", err)
	}
	experiments, err := reopened.ListExperiments(ctx)
	if err != nil || len(experiments) != 1 {
		t.Fatalf("ListExperiments() = %d, %v; want one", len(experiments), err)
	}
}

func TestConcurrentClaimHasSingleWinner(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, run := createExperimentAndRun(t, store, 3)
	registerWorker(t, store, "worker-a")
	registerWorker(t, store, "worker-b")

	type claimResponse struct {
		claim ClaimResult
		err   error
	}
	start := make(chan struct{})
	responses := make(chan claimResponse, 2)
	for _, workerID := range []string{"worker-a", "worker-b"} {
		workerID := workerID
		go func() {
			<-start
			claim, err := store.Claim(context.Background(), ClaimParams{WorkerID: workerID, LeaseTTL: time.Minute})
			responses <- claimResponse{claim: claim, err: err}
		}()
	}
	close(start)
	winners := 0
	noWork := 0
	var winner ClaimResult
	for range 2 {
		response := <-responses
		switch {
		case response.err == nil:
			winners++
			winner = response.claim
		case errors.Is(response.err, ErrNoWork):
			noWork++
		default:
			t.Fatalf("Claim() unexpected error = %v", response.err)
		}
	}
	if winners != 1 || noWork != 1 {
		t.Fatalf("claims: winners=%d no_work=%d, want 1/1", winners, noWork)
	}
	stored, err := store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.RunActive || stored.ActiveAttemptID != winner.Attempt.ID || stored.AttemptCount != 1 {
		t.Fatalf("stored run = %#v", stored)
	}
}

func TestLeaseExpiryRetriesAndFencesOldWorker(t *testing.T) {
	store, clock, _ := newTestStore(t)
	_, run := createExperimentAndRun(t, store, 2)
	registerWorker(t, store, "worker-a")
	registerWorker(t, store, "worker-b")
	ctx := context.Background()

	first, err := store.Claim(ctx, ClaimParams{WorkerID: "worker-a", LeaseTTL: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Second)
	expired, err := store.ExpireLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired.Expired) != 1 || !expired.Expired[0].RetryScheduled {
		t.Fatalf("ExpireLeases() = %#v", expired)
	}
	second, err := store.Claim(ctx, ClaimParams{WorkerID: "worker-b", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if second.Fence <= first.Fence || second.Attempt.ID == first.Attempt.ID {
		t.Fatalf("second claim fence/id = %d/%s, first = %d/%s", second.Fence, second.Attempt.ID, first.Fence, first.Attempt.ID)
	}

	_, err = store.CompleteAttempt(ctx, CompleteAttemptParams{
		LeaseOperationParams: LeaseOperationParams{
			RunID: run.ID, AttemptID: first.Attempt.ID, WorkerID: "worker-a", Fence: first.Fence, LeaseToken: first.LeaseToken,
		},
		Outcome: domain.OutcomeSucceeded,
	})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late CompleteAttempt() error = %v, want ErrLeaseLost", err)
	}
	if _, err := store.Heartbeat(ctx, HeartbeatParams{
		LeaseOperationParams: LeaseOperationParams{RunID: run.ID, AttemptID: second.Attempt.ID, Fence: first.Fence, LeaseToken: second.LeaseToken},
		ExtendBy:             time.Minute,
	}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong fence Heartbeat() error = %v, want ErrLeaseLost", err)
	}
	current, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ActiveAttemptID != second.Attempt.ID || current.State != domain.RunActive {
		t.Fatalf("run after stale completion = %#v", current)
	}
}

func TestCancelRejectsLateSuccess(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, run := createExperimentAndRun(t, store, 1)
	registerWorker(t, store, "worker-a")
	ctx := context.Background()
	claim, err := store.Claim(ctx, ClaimParams{WorkerID: "worker-a", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(ctx, LeaseOperationParams{
		RunID: run.ID, AttemptID: claim.Attempt.ID, WorkerID: "worker-a", Fence: claim.Fence, LeaseToken: claim.LeaseToken,
	}); err != nil {
		t.Fatal(err)
	}

	cancelling, err := store.CancelRun(ctx, CancelRunParams{RunID: run.ID, Reason: "user requested"})
	if err != nil {
		t.Fatal(err)
	}
	if cancelling.State != domain.RunActive || cancelling.CancelRequestedAt == nil {
		t.Fatalf("CancelRun() = %#v; want active with requested marker", cancelling)
	}
	completed, err := store.CompleteAttempt(ctx, CompleteAttemptParams{
		LeaseOperationParams: LeaseOperationParams{
			RunID: run.ID, AttemptID: claim.Attempt.ID, WorkerID: "worker-a", Fence: claim.Fence, LeaseToken: claim.LeaseToken,
		},
		Outcome: domain.OutcomeSucceeded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.State != domain.RunCancelled || completed.Run.Result == nil || completed.Run.Result.Outcome != domain.OutcomeCancelled {
		t.Fatalf("late success completed run = %#v", completed.Run)
	}
	if completed.Attempt.State != domain.AttemptCancelled || completed.Attempt.Result == nil || completed.Attempt.Result.Outcome != domain.OutcomeCancelled {
		t.Fatalf("late success attempt = %#v", completed.Attempt)
	}

	replayed, err := store.CompleteAttempt(ctx, CompleteAttemptParams{
		LeaseOperationParams: LeaseOperationParams{RunID: run.ID, AttemptID: claim.Attempt.ID, Fence: claim.Fence, LeaseToken: claim.LeaseToken},
		Outcome:              domain.OutcomeSucceeded,
	})
	if err != nil {
		t.Fatalf("replayed late success error = %v", err)
	}
	if replayed.Run.State != domain.RunCancelled || replayed.Attempt.State != domain.AttemptCancelled {
		t.Fatalf("replayed late success = %#v", replayed)
	}
}

func TestCompleteAttemptReplayPersistsAcrossRestart(t *testing.T) {
	store, clock, path := newTestStore(t)
	_, run := createExperimentAndRun(t, store, 1)
	registerWorker(t, store, "worker-a")
	ctx := context.Background()
	claim, err := store.Claim(ctx, ClaimParams{WorkerID: "worker-a", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(ctx, LeaseOperationParams{
		RunID: run.ID, AttemptID: claim.Attempt.ID, Fence: claim.Fence, LeaseToken: claim.LeaseToken,
	}); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	params := CompleteAttemptParams{
		LeaseOperationParams: LeaseOperationParams{
			RunID: run.ID, AttemptID: claim.Attempt.ID, WorkerID: "worker-a",
			Fence: claim.Fence, LeaseToken: claim.LeaseToken,
		},
		Outcome: domain.OutcomeSucceeded, ExitCode: &exitCode,
		Metrics:   map[string]float64{"throughput": 42.5},
		Artifacts: []domain.Artifact{{Name: "result.json", URI: "file:///tmp/result.json", SHA256: "abc", SizeBytes: 12}},
	}
	first, err := store.CompleteAttempt(ctx, params)
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := NewJSONStore(path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	replayed, err := reopened.CompleteAttempt(ctx, params)
	if err != nil {
		t.Fatalf("replayed CompleteAttempt() error = %v", err)
	}
	if replayed.Run.ID != first.Run.ID || replayed.Run.State != first.Run.State ||
		replayed.Attempt.ID != first.Attempt.ID || replayed.RetryScheduled != first.RetryScheduled {
		t.Fatalf("replayed completion = %#v, first = %#v", replayed, first)
	}
	if replayed.Run.Result == nil || first.Run.Result == nil ||
		replayed.Run.Result.Outcome != first.Run.Result.Outcome ||
		replayed.Run.Result.Metrics["throughput"] != first.Run.Result.Metrics["throughput"] ||
		len(replayed.Run.Result.Artifacts) != len(first.Run.Result.Artifacts) {
		t.Fatalf("replayed run result = %#v, first = %#v", replayed.Run.Result, first.Run.Result)
	}
	events, err := reopened.ListEvents(ctx, run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	completedEvents := 0
	for _, event := range events {
		if event.Type == "attempt.completed" {
			completedEvents++
		}
	}
	if completedEvents != 1 {
		t.Fatalf("attempt.completed events = %d, want 1", completedEvents)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stringContains(string(contents), claim.LeaseToken) {
		t.Fatal("completion receipt contains plaintext lease token")
	}
}

func TestCompleteAttemptExitCodeDoesNotAliasCaller(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, run := createExperimentAndRun(t, store, 1)
	registerWorker(t, store, "worker-a")
	claim, err := store.Claim(context.Background(), ClaimParams{WorkerID: "worker-a", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(context.Background(), LeaseOperationParams{
		RunID: run.ID, AttemptID: claim.Attempt.ID, WorkerID: "worker-a", Fence: claim.Fence, LeaseToken: claim.LeaseToken,
	}); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := store.CompleteAttempt(context.Background(), CompleteAttemptParams{
		LeaseOperationParams: LeaseOperationParams{
			RunID: run.ID, AttemptID: claim.Attempt.ID, WorkerID: "worker-a", Fence: claim.Fence, LeaseToken: claim.LeaseToken,
		},
		Outcome: domain.OutcomeSucceeded, ExitCode: &exitCode,
	}); err != nil {
		t.Fatal(err)
	}
	exitCode = 99
	storedRun, err := store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.Result == nil || storedRun.Result.ExitCode == nil || *storedRun.Result.ExitCode != 0 {
		t.Fatalf("stored exit code = %#v", storedRun.Result)
	}
}

func TestCompleteAttemptReplayWithDifferentPayloadConflicts(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, run := createExperimentAndRun(t, store, 1)
	registerWorker(t, store, "worker-a")
	ctx := context.Background()
	claim, err := store.Claim(ctx, ClaimParams{WorkerID: "worker-a", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(ctx, LeaseOperationParams{
		RunID: run.ID, AttemptID: claim.Attempt.ID, Fence: claim.Fence, LeaseToken: claim.LeaseToken,
	}); err != nil {
		t.Fatal(err)
	}
	params := CompleteAttemptParams{
		LeaseOperationParams: LeaseOperationParams{
			RunID: run.ID, AttemptID: claim.Attempt.ID, Fence: claim.Fence, LeaseToken: claim.LeaseToken,
		},
		Outcome: domain.OutcomeSucceeded, Metrics: map[string]float64{"score": 1},
	}
	if _, err := store.CompleteAttempt(ctx, params); err != nil {
		t.Fatal(err)
	}
	params.RunID = "different-run"
	if _, err := store.CompleteAttempt(ctx, params); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-run replay error = %v, want ErrLeaseLost", err)
	}
	params.RunID = run.ID
	params.Metrics["score"] = 2
	if _, err := store.CompleteAttempt(ctx, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay error = %v, want ErrConflict", err)
	}
	params.Metrics["score"] = 1
	params.LeaseToken = "wrong-token"
	if _, err := store.CompleteAttempt(ctx, params); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-token replay error = %v, want ErrLeaseLost", err)
	}
}

func TestHeartbeatExtendsTenMinuteLeaseByRequestedTTL(t *testing.T) {
	store, clock, _ := newTestStore(t)
	_, run := createExperimentAndRun(t, store, 1)
	registerWorker(t, store, "worker-a")
	ctx := context.Background()
	claim, err := store.Claim(ctx, ClaimParams{WorkerID: "worker-a", LeaseTTL: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(200 * time.Second)
	lease, err := store.Heartbeat(ctx, HeartbeatParams{
		LeaseOperationParams: LeaseOperationParams{
			RunID: run.ID, AttemptID: claim.Attempt.ID, Fence: claim.Fence, LeaseToken: claim.LeaseToken,
		},
		ExtendBy: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := lease.ExpiresAt, clock.Now().Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("lease expiry = %s, want %s", got, want)
	}
	clock.Advance(200 * time.Second)
	if _, err := store.Heartbeat(ctx, HeartbeatParams{
		LeaseOperationParams: LeaseOperationParams{
			RunID: run.ID, AttemptID: claim.Attempt.ID, Fence: claim.Fence, LeaseToken: claim.LeaseToken,
		},
		ExtendBy: 10 * time.Minute,
	}); err != nil {
		t.Fatalf("second heartbeat after 200s lost lease: %v", err)
	}
}

func TestLeaseTokenIsHashedInSnapshot(t *testing.T) {
	store, _, path := newTestStore(t)
	createExperimentAndRun(t, store, 1)
	registerWorker(t, store, "worker-a")
	claim, err := store.Claim(context.Background(), ClaimParams{WorkerID: "worker-a", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if json.Valid(contents) == false {
		t.Fatal("snapshot is invalid JSON")
	}
	if stringContains(string(contents), claim.LeaseToken) {
		t.Fatal("snapshot contains plaintext lease token")
	}
}

func stringContains(text, substring string) bool {
	for index := 0; index+len(substring) <= len(text); index++ {
		if text[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}

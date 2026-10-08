package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"
)

type oneShotClient struct {
	mu              sync.Mutex
	claims          int
	heartbeat       int
	registerRequest RegisterRequest
	heartbeatInput  WorkerHeartbeatRequest
	claimSessionID  string
	heartbeatCh     chan WorkerHeartbeatRequest
	runState        string
}

func (c *oneShotClient) Register(_ context.Context, input RegisterRequest) (WorkerRecord, error) {
	c.mu.Lock()
	c.registerRequest = input
	c.mu.Unlock()
	return WorkerRecord{ID: "worker-one"}, nil
}
func (c *oneShotClient) WorkerHeartbeat(_ context.Context, _ string, input WorkerHeartbeatRequest) error {
	c.mu.Lock()
	c.heartbeat++
	c.heartbeatInput = input
	if c.heartbeatCh != nil {
		select {
		case c.heartbeatCh <- input:
		default:
		}
	}
	c.mu.Unlock()
	return nil
}
func (c *oneShotClient) Claim(_ context.Context, _, sessionID string, _ time.Duration, _ time.Duration) (*Claim, error) {
	c.mu.Lock()
	c.claims++
	c.claimSessionID = sessionID
	c.mu.Unlock()
	return nil, nil
}

type sequenceProbe struct {
	mu      sync.Mutex
	results []WorkerResources
	index   int
}

func (p *sequenceProbe) Probe(context.Context) WorkerResources {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := p.results[p.index]
	if p.index < len(p.results)-1 {
		p.index++
	}
	return result
}
func (*oneShotClient) Start(context.Context, Claim) error { return nil }
func (*oneShotClient) AttemptHeartbeat(_ context.Context, _ Claim, extendBy time.Duration) (time.Time, error) {
	return time.Now().Add(extendBy), nil
}
func (*oneShotClient) Complete(context.Context, Claim, Completion) error  { return nil }
func (c *oneShotClient) RunState(context.Context, string) (string, error) { return c.runState, nil }

func TestOneShotWorkerClaimsOnceAndExitsWhenQueueIsEmpty(t *testing.T) {
	t.Parallel()
	client := &oneShotClient{}
	runner, err := NewRunner(client, Config{
		Name: "one-shot", Adapter: AdapterDemoSleep, OneShot: true,
		ClaimWait: time.Millisecond, HeartbeatInterval: time.Millisecond,
		SessionID: "session-fixed", ResourceProbe: &sequenceProbe{results: []WorkerResources{{
			GPUs: []GPUResource{}, ObservedAt: time.Unix(10, 0), ProbeStatus: ResourceProbeUnavailable,
		}}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.claims != 1 {
		t.Fatalf("claims = %d, want 1", client.claims)
	}
	if client.registerRequest.SessionID != "session-fixed" || client.claimSessionID != "session-fixed" {
		t.Fatalf("session IDs were not propagated: register=%q claim=%q", client.registerRequest.SessionID, client.claimSessionID)
	}
	if client.registerRequest.Resources.ProbeStatus != ResourceProbeUnavailable {
		t.Fatalf("registered resources = %#v", client.registerRequest.Resources)
	}
}

func TestRunnerKeepsLastGoodGPUsWhenProbeErrors(t *testing.T) {
	t.Parallel()
	goodGPU := GPUResource{ID: "GPU-a", Index: 2, Name: "RTX", Vendor: "NVIDIA", TotalMemoryBytes: 10, FreeMemoryBytes: 8}
	probe := &sequenceProbe{results: []WorkerResources{
		{GPUs: []GPUResource{goodGPU}, ObservedAt: time.Unix(10, 0), ProbeStatus: ResourceProbeOK},
		{GPUs: []GPUResource{}, ObservedAt: time.Unix(20, 0), ProbeStatus: ResourceProbeError},
	}}
	runner, err := NewRunner(&oneShotClient{}, Config{
		Name: "worker", Adapter: AdapterDemoSleep, SessionID: "session", ResourceProbe: probe,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	first := runner.observeResources(context.Background())
	second := runner.observeResources(context.Background())
	if len(first.GPUs) != 1 || len(second.GPUs) != 1 || second.GPUs[0].ID != goodGPU.ID {
		t.Fatalf("last good resources not retained: first=%#v second=%#v", first, second)
	}
	if second.ProbeStatus != ResourceProbeError || !second.ObservedAt.Equal(time.Unix(20, 0)) {
		t.Fatalf("error observation metadata not retained: %#v", second)
	}
}

func TestWorkerHeartbeatProbesResourcesAndSendsSession(t *testing.T) {
	t.Parallel()
	client := &oneShotClient{heartbeatCh: make(chan WorkerHeartbeatRequest, 1)}
	observedAt := time.Unix(30, 0)
	runner, err := NewRunner(client, Config{
		Name: "worker", Adapter: AdapterDemoSleep, SessionID: "session-heartbeat",
		HeartbeatInterval: time.Millisecond,
		ResourceProbe: &sequenceProbe{results: []WorkerResources{{
			GPUs:       []GPUResource{{ID: "GPU-heartbeat", Index: 0, Name: "RTX", Vendor: "NVIDIA"}},
			ObservedAt: observedAt, ProbeStatus: ResourceProbeOK,
		}}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runner.workerHeartbeatLoop(ctx, "worker-one")
		close(done)
	}()
	select {
	case heartbeat := <-client.heartbeatCh:
		if heartbeat.SessionID != "session-heartbeat" || heartbeat.Resources.ProbeStatus != ResourceProbeOK || len(heartbeat.Resources.GPUs) != 1 || heartbeat.Resources.GPUs[0].ID != "GPU-heartbeat" || !heartbeat.Resources.ObservedAt.Equal(observedAt) {
			t.Fatalf("heartbeat = %#v", heartbeat)
		}
	case <-time.After(time.Second):
		t.Fatal("worker heartbeat was not sent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker heartbeat loop did not stop")
	}
}

func TestEnvironmentForAllocationOverridesRecipeGPUSelection(t *testing.T) {
	t.Parallel()
	got, err := environmentForAllocation([]string{"PATH=/bin", "CUDA_VISIBLE_DEVICES=99", "cuda_visible_devices=98"}, ResourceAllocation{
		GPUs: []GPUResource{{ID: "GPU-a", Index: 3}, {ID: "GPU-b", Index: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCUDA := "CUDA_VISIBLE_DEVICES=3,1"
	found := 0
	for _, entry := range got {
		if entry == wantCUDA {
			found++
		}
		if entry == "CUDA_VISIBLE_DEVICES=99" || entry == "cuda_visible_devices=98" {
			t.Fatalf("recipe GPU selection survived: %q", got)
		}
	}
	if found != 1 {
		t.Fatalf("environment = %q, want one %q", got, wantCUDA)
	}
}

func TestEnvironmentForEmptyAllocationPreservesCompatibility(t *testing.T) {
	t.Parallel()
	input := []string{"PATH=/bin", "CUDA_VISIBLE_DEVICES=99"}
	got, err := environmentForAllocation(input, ResourceAllocation{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(input) || got[0] != input[0] || got[1] != input[1] {
		t.Fatalf("environment = %q, want %q", got, input)
	}
	got[0] = "changed"
	if input[0] == got[0] {
		t.Fatal("environment result aliases the recipe input")
	}
}

func TestNewRunnerGeneratesSessionID(t *testing.T) {
	t.Parallel()
	runner, err := NewRunner(&oneShotClient{}, Config{
		Name: "worker", Adapter: AdapterDemoSleep,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if runner.config.SessionID == "" {
		t.Fatal("session ID was not generated")
	}
}

func TestRunnerAddsPlatformAndDetectedAcceleratorLabels(t *testing.T) {
	t.Parallel()
	runner, err := NewRunner(&oneShotClient{}, Config{
		Name: "worker", Adapter: AdapterDemoSleep, Labels: map[string]string{"site": "lab"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	labels := runner.labelsFor(WorkerResources{GPUs: []GPUResource{{ID: "GPU-a"}}})
	if labels["os"] != runtime.GOOS || labels["arch"] != runtime.GOARCH || labels["accelerator"] != "nvidia" || labels["site"] != "lab" {
		t.Fatalf("labels = %#v", labels)
	}
	if _, mutated := runner.config.Labels["os"]; mutated {
		t.Fatalf("config labels were mutated: %#v", runner.config.Labels)
	}
}

func TestRunnerRejectsHeartbeatIntervalBeyondLeaseSafetyWindow(t *testing.T) {
	t.Parallel()
	_, err := NewRunner(&oneShotClient{}, Config{
		Name: "unsafe", Adapter: AdapterDemoSleep,
		LeaseTTL: 10 * time.Minute, HeartbeatInterval: 201 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewRunner() accepted heartbeat interval greater than one third of lease TTL")
	}
}

func TestMonitorReportsOperatorCancellation(t *testing.T) {
	t.Parallel()
	client := &oneShotClient{runState: "cancel_requested"}
	runner, err := NewRunner(client, Config{
		Name: "worker", Adapter: AdapterDemoSleep, CancelPollInterval: time.Millisecond,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = runner.monitor(ctx, cancel, Claim{RunID: "run-one"})
	if !errors.Is(err, errCancellationRequested) {
		t.Fatalf("monitor error = %v", err)
	}
}

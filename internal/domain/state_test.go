package domain

import (
	"errors"
	"testing"
	"time"
)

func TestTerminalRunCannotBeRequeued(t *testing.T) {
	run := Run{State: RunSucceeded}
	err := TransitionRun(&run, RunQueued, time.Now())
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("TransitionRun() error = %v, want ErrInvalidTransition", err)
	}
}

func TestAttemptTransitionsAreOneWay(t *testing.T) {
	now := time.Now()
	attempt := Attempt{State: AttemptLeased}
	if err := TransitionAttempt(&attempt, AttemptRunning, now); err != nil {
		t.Fatal(err)
	}
	if err := TransitionAttempt(&attempt, AttemptSucceeded, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := TransitionAttempt(&attempt, AttemptRunning, now.Add(2*time.Second)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal transition error = %v, want ErrInvalidTransition", err)
	}
}

func TestWorkerResourcesRejectDuplicateIndexesAndUnavailableGPUs(t *testing.T) {
	duplicate := WorkerResources{ProbeStatus: ResourceProbeOK, GPUs: []GPUResource{
		{ID: "GPU-a", Index: 0, Name: "A", Vendor: "NVIDIA", TotalMemoryBytes: 10, FreeMemoryBytes: 8},
		{ID: "GPU-b", Index: 0, Name: "B", Vendor: "NVIDIA", TotalMemoryBytes: 10, FreeMemoryBytes: 8},
	}}
	if err := ValidateWorkerResources(duplicate); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate index error = %v", err)
	}
	unavailable := WorkerResources{ProbeStatus: ResourceProbeUnavailable, GPUs: []GPUResource{
		{ID: "GPU-a", Index: 0, Name: "A", Vendor: "NVIDIA", TotalMemoryBytes: 10, FreeMemoryBytes: 8},
	}}
	if err := ValidateWorkerResources(unavailable); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unavailable resources error = %v", err)
	}
}

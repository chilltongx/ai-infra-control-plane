package domain

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

var (
	ErrInvalid           = errors.New("invalid domain value")
	ErrInvalidTransition = errors.New("invalid state transition")
)

func ValidateRecipe(recipe Recipe) error {
	if strings.TrimSpace(recipe.Adapter) == "" {
		return fmt.Errorf("%w: recipe adapter is required", ErrInvalid)
	}
	if len(recipe.Command) == 0 {
		return fmt.Errorf("%w: recipe command is required", ErrInvalid)
	}
	for i, part := range recipe.Command {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf("%w: recipe command item %d is empty", ErrInvalid, i)
		}
	}
	if recipe.TimeoutSeconds < 0 {
		return fmt.Errorf("%w: recipe timeout cannot be negative", ErrInvalid)
	}
	return nil
}

func ValidateWorkerState(state WorkerState) error {
	switch state {
	case WorkerOnline, WorkerDraining, WorkerOffline:
		return nil
	default:
		return fmt.Errorf("%w: unknown worker state %q", ErrInvalid, state)
	}
}

func ValidateResourceRequirements(requirements ResourceRequirements) error {
	if requirements.GPUCount < 0 || requirements.GPUCount > 16 {
		return fmt.Errorf("%w: gpu_count must be between 0 and 16", ErrInvalid)
	}
	if requirements.MinFreeGPUMemoryBytes < 0 {
		return fmt.Errorf("%w: min_free_gpu_memory_bytes cannot be negative", ErrInvalid)
	}
	if requirements.MinFreeGPUMemoryBytes > 0 && requirements.GPUCount == 0 {
		return fmt.Errorf("%w: gpu_count is required when minimum free GPU memory is set", ErrInvalid)
	}
	return nil
}

func ValidateWorkerResources(resources WorkerResources) error {
	switch resources.ProbeStatus {
	case "", ResourceProbeOK, ResourceProbeUnavailable, ResourceProbeError:
	default:
		return fmt.Errorf("%w: unknown resource probe status %q", ErrInvalid, resources.ProbeStatus)
	}
	seen := make(map[string]struct{}, len(resources.GPUs))
	seenIndexes := make(map[int]struct{}, len(resources.GPUs))
	if (resources.ProbeStatus == ResourceProbeUnavailable || resources.ProbeStatus == "") && len(resources.GPUs) > 0 {
		return fmt.Errorf("%w: probe status %q cannot report GPUs", ErrInvalid, resources.ProbeStatus)
	}
	for index, gpu := range resources.GPUs {
		if strings.TrimSpace(gpu.ID) == "" || strings.TrimSpace(gpu.Name) == "" || strings.TrimSpace(gpu.Vendor) == "" {
			return fmt.Errorf("%w: gpu %d requires id, name, and vendor", ErrInvalid, index)
		}
		if _, exists := seen[gpu.ID]; exists {
			return fmt.Errorf("%w: duplicate gpu id %q", ErrInvalid, gpu.ID)
		}
		seen[gpu.ID] = struct{}{}
		if _, exists := seenIndexes[gpu.Index]; exists {
			return fmt.Errorf("%w: duplicate gpu index %d", ErrInvalid, gpu.Index)
		}
		seenIndexes[gpu.Index] = struct{}{}
		if gpu.Index < 0 || gpu.TotalMemoryBytes < 0 || gpu.FreeMemoryBytes < 0 || gpu.FreeMemoryBytes > gpu.TotalMemoryBytes {
			return fmt.Errorf("%w: gpu %q has invalid memory or index", ErrInvalid, gpu.ID)
		}
		if math.IsNaN(gpu.UtilizationPercent) || math.IsInf(gpu.UtilizationPercent, 0) || gpu.UtilizationPercent < 0 || gpu.UtilizationPercent > 100 {
			return fmt.Errorf("%w: gpu %q utilization must be between 0 and 100", ErrInvalid, gpu.ID)
		}
		if math.IsNaN(gpu.TemperatureCelsius) || math.IsInf(gpu.TemperatureCelsius, 0) || gpu.TemperatureCelsius < 0 || gpu.TemperatureCelsius > 200 {
			return fmt.Errorf("%w: gpu %q temperature must be between 0 and 200", ErrInvalid, gpu.ID)
		}
	}
	return nil
}

func CanTransitionRun(from, to RunState) bool {
	if from == to {
		return true
	}
	switch from {
	case RunQueued:
		return to == RunActive || to == RunCancelled
	case RunActive:
		return to == RunQueued || to == RunSucceeded || to == RunFailed || to == RunCancelled
	default:
		return false
	}
}

func TransitionRun(run *Run, to RunState, now time.Time) error {
	if run == nil || !CanTransitionRun(run.State, to) {
		from := RunState("")
		if run != nil {
			from = run.State
		}
		return fmt.Errorf("%w: run %q -> %q", ErrInvalidTransition, from, to)
	}
	if run.State == to {
		return nil
	}
	run.State = to
	run.UpdatedAt = now
	if to.Terminal() {
		finished := now
		run.FinishedAt = &finished
	} else {
		run.FinishedAt = nil
	}
	return nil
}

func CanTransitionAttempt(from, to AttemptState) bool {
	if from == to {
		return true
	}
	switch from {
	case AttemptLeased:
		return to == AttemptRunning || to == AttemptCancelled || to == AttemptLost
	case AttemptRunning:
		return to == AttemptSucceeded || to == AttemptFailed || to == AttemptCancelled || to == AttemptLost
	default:
		return false
	}
}

func TransitionAttempt(attempt *Attempt, to AttemptState, now time.Time) error {
	if attempt == nil || !CanTransitionAttempt(attempt.State, to) {
		from := AttemptState("")
		if attempt != nil {
			from = attempt.State
		}
		return fmt.Errorf("%w: attempt %q -> %q", ErrInvalidTransition, from, to)
	}
	if attempt.State == to {
		return nil
	}
	attempt.State = to
	attempt.UpdatedAt = now
	if to == AttemptRunning && attempt.StartedAt == nil {
		started := now
		attempt.StartedAt = &started
	}
	if to.Terminal() {
		finished := now
		attempt.FinishedAt = &finished
	}
	return nil
}

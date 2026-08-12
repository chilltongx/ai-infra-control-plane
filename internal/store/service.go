package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chilltongx/ai-infra-control-plane/internal/api"
	"github.com/chilltongx/ai-infra-control-plane/internal/domain"
)

// APIService adapts the durable domain store to the deliberately independent
// HTTP DTOs. Keeping this mapping here prevents transport concerns from
// entering the scheduling core.
type APIService struct {
	store *JSONStore
}

func NewAPIService(repository *JSONStore) *APIService {
	if repository == nil {
		panic("store.NewAPIService: nil JSONStore")
	}
	return &APIService{store: repository}
}

func (s *APIService) Ready(ctx context.Context) error {
	return mapAPIError(s.store.Ready(ctx))
}

func (s *APIService) CreateExperiment(ctx context.Context, input api.CreateExperimentInput) (api.Experiment, error) {
	result, err := s.store.CreateExperiment(ctx, CreateExperimentParams{
		Name: input.Name, Description: input.Description, Labels: input.Labels,
		IdempotencyKey: api.IdempotencyKey(ctx),
	})
	return experimentToAPI(result.Experiment), mapAPIError(err)
}

func (s *APIService) ListExperiments(ctx context.Context, limit int) ([]api.Experiment, error) {
	items, err := s.store.ListExperiments(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	result := make([]api.Experiment, len(items))
	for i, item := range items {
		result[i] = experimentToAPI(item)
	}
	return result, nil
}

func (s *APIService) GetExperiment(ctx context.Context, id string) (api.Experiment, error) {
	item, err := s.store.GetExperiment(ctx, id)
	return experimentToAPI(item), mapAPIError(err)
}

func (s *APIService) CreateRun(ctx context.Context, input api.CreateRunInput) (api.Run, error) {
	result, err := s.store.CreateRun(ctx, CreateRunParams{
		ExperimentID: input.ExperimentID, Recipe: recipeFromAPI(input.Recipe),
		RequiredLabels: input.RequiredLabels, ResourceRequirements: requirementsFromAPI(input.ResourceRequirements), Priority: input.Priority,
		MaxAttempts: input.MaxAttempts, IdempotencyKey: api.IdempotencyKey(ctx),
	})
	return runToAPI(result.Run), mapAPIError(err)
}

func (s *APIService) ListRuns(ctx context.Context, experimentID string, limit int) ([]api.Run, error) {
	items, err := s.store.ListRuns(ctx, experimentID)
	if err != nil {
		return nil, mapAPIError(err)
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	result := make([]api.Run, len(items))
	for i, item := range items {
		result[i] = runToAPI(item)
	}
	return result, nil
}

func (s *APIService) GetRun(ctx context.Context, id string) (api.Run, error) {
	item, err := s.store.GetRun(ctx, id)
	return runToAPI(item), mapAPIError(err)
}

func (s *APIService) ListAttempts(ctx context.Context, runID string, limit int) ([]api.Attempt, error) {
	items, err := s.store.ListAttempts(ctx, runID)
	if err != nil {
		return nil, mapAPIError(err)
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	result := make([]api.Attempt, len(items))
	for i, item := range items {
		result[i] = attemptToAPI(item)
	}
	return result, nil
}

func (s *APIService) CancelRun(ctx context.Context, id, reason string) (api.Run, error) {
	item, err := s.store.CancelRun(ctx, CancelRunParams{RunID: id, Reason: reason})
	return runToAPI(item), mapAPIError(err)
}

func (s *APIService) RegisterWorker(ctx context.Context, input api.RegisterWorkerInput) (api.Worker, error) {
	item, err := s.store.RegisterWorker(ctx, RegisterWorkerParams{
		ID: input.ID, Name: input.Name, Adapter: input.Adapter,
		Labels: input.Labels, Version: input.Version, SessionID: input.SessionID,
		Resources: resourcesFromAPI(input.Resources),
	})
	return workerToAPI(item), mapAPIError(err)
}

func (s *APIService) ListWorkers(ctx context.Context, limit int) ([]api.Worker, error) {
	items, err := s.store.ListWorkers(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	result := make([]api.Worker, len(items))
	for i, item := range items {
		result[i] = workerToAPI(item)
	}
	return result, nil
}

func (s *APIService) HeartbeatWorker(ctx context.Context, workerID string, input api.WorkerHeartbeatInput) (api.Worker, error) {
	item, err := s.store.HeartbeatWorker(ctx, WorkerHeartbeatParams{
		WorkerID: workerID, State: domain.WorkerState(input.State),
		SessionID: input.SessionID, Labels: input.Labels, Version: input.Version,
		Resources: resourcesPointerFromAPI(input.Resources),
	})
	return workerToAPI(item), mapAPIError(err)
}

func (s *APIService) Claim(ctx context.Context, input api.ClaimInput) (api.Claim, bool, error) {
	deadline := time.Time{}
	if input.WaitTimeout > 0 {
		deadline = time.Now().Add(input.WaitTimeout)
	}
	for {
		result, err := s.store.Claim(ctx, ClaimParams{WorkerID: input.WorkerID, SessionID: input.SessionID, LeaseTTL: input.LeaseTTL})
		if err == nil {
			return api.Claim{
				AttemptID: result.Attempt.ID, RunID: result.Run.ID, Fence: result.Fence,
				LeaseToken: result.LeaseToken, ExpiresAt: result.LeaseExpiresAt,
				Recipe:     recipeToAPI(result.Recipe),
				Allocation: allocationToAPI(result.Allocation),
			}, true, nil
		}
		if !errors.Is(err, ErrNoWork) {
			return api.Claim{}, false, mapAPIError(err)
		}
		if deadline.IsZero() || !time.Now().Before(deadline) {
			return api.Claim{}, false, nil
		}
		remaining := time.Until(deadline)
		delay := 100 * time.Millisecond
		if remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.Claim{}, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *APIService) StartAttempt(ctx context.Context, input api.AttemptMutationInput) (api.Run, error) {
	run, err := s.store.StartAttempt(ctx, LeaseOperationParams{
		RunID: input.RunID, AttemptID: input.AttemptID,
		Fence: input.Fence, LeaseToken: input.LeaseToken,
	})
	return runToAPI(run), mapAPIError(err)
}

func (s *APIService) HeartbeatAttempt(ctx context.Context, input api.AttemptMutationInput) (api.Lease, error) {
	lease, err := s.store.Heartbeat(ctx, HeartbeatParams{
		LeaseOperationParams: LeaseOperationParams{
			RunID: input.RunID, AttemptID: input.AttemptID,
			Fence: input.Fence, LeaseToken: input.LeaseToken,
		},
		ExtendBy: time.Duration(input.ExtendSeconds) * time.Second,
	})
	return api.Lease{
		AttemptID: lease.AttemptID, RunID: lease.RunID,
		Fence: lease.Fence, ExpiresAt: lease.ExpiresAt,
	}, mapAPIError(err)
}

func (s *APIService) CompleteAttempt(ctx context.Context, input api.CompleteAttemptInput) (api.Run, error) {
	artifacts := make([]domain.Artifact, len(input.Artifacts))
	for i, artifact := range input.Artifacts {
		artifacts[i] = domain.Artifact{
			Name: artifact.Name, URI: artifact.URI, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes,
		}
	}
	result, err := s.store.CompleteAttempt(ctx, CompleteAttemptParams{
		LeaseOperationParams: LeaseOperationParams{
			RunID: input.RunID, AttemptID: input.AttemptID,
			Fence: input.Fence, LeaseToken: input.LeaseToken,
		},
		Outcome: domain.Outcome(input.Outcome), ExitCode: input.ExitCode,
		ErrorMessage: input.Error, Metrics: input.Metrics, Artifacts: artifacts,
	})
	return runToAPI(result.Run), mapAPIError(err)
}

func (s *APIService) ListEvents(ctx context.Context, runID string, after int64, limit int) ([]api.Event, error) {
	items, err := s.store.ListEvents(ctx, runID, after, limit)
	if err != nil {
		return nil, mapAPIError(err)
	}
	result := make([]api.Event, len(items))
	for i, item := range items {
		result[i] = api.Event{
			Sequence: item.Sequence, RunID: item.RunID, AttemptID: item.AttemptID, Type: item.Type,
			Timestamp: item.Timestamp, Data: item.Data,
		}
	}
	return result, nil
}

func experimentToAPI(item domain.Experiment) api.Experiment {
	return api.Experiment{
		ID: item.ID, Name: item.Name, Description: item.Description,
		Labels: copyStringMap(item.Labels), CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func recipeFromAPI(item api.Recipe) domain.Recipe {
	return domain.Recipe{
		Adapter: item.Adapter, Command: append([]string(nil), item.Command...),
		WorkingDir: item.WorkingDir, Environment: copyStringMap(item.Environment),
		ExpectedOutputs: append([]string(nil), item.ExpectedOutputs...), TimeoutSeconds: item.TimeoutSeconds,
	}
}

func recipeToAPI(item domain.Recipe) api.Recipe {
	return api.Recipe{
		Adapter: item.Adapter, Command: append([]string(nil), item.Command...),
		WorkingDir: item.WorkingDir, Environment: copyStringMap(item.Environment),
		ExpectedOutputs: append([]string(nil), item.ExpectedOutputs...), TimeoutSeconds: item.TimeoutSeconds,
	}
}

func runToAPI(item domain.Run) api.Run {
	state := string(item.State)
	if item.CancelRequestedAt != nil && item.State == domain.RunActive {
		state = "cancel_requested"
	}
	result := api.Run{
		ID: item.ID, ExperimentID: item.ExperimentID, State: state,
		Recipe: recipeToAPI(item.Recipe), RequiredLabels: copyStringMap(item.RequiredLabels),
		ResourceRequirements: requirementsToAPI(item.ResourceRequirements),
		Priority:             item.Priority, MaxAttempts: item.MaxAttempts, AttemptCount: item.AttemptCount,
		ActiveAttemptID: item.ActiveAttemptID, CancelRequestedAt: cloneTimePointer(item.CancelRequestedAt),
		CancellationReason: item.CancellationReason, CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt, FinishedAt: cloneTimePointer(item.FinishedAt),
	}
	if item.LastAllocation != nil {
		allocation := allocationToAPI(*item.LastAllocation)
		result.LastAllocation = &allocation
	}
	if item.Result != nil {
		artifacts := make([]api.Artifact, len(item.Result.Artifacts))
		for i, artifact := range item.Result.Artifacts {
			artifacts[i] = api.Artifact{
				Name: artifact.Name, URI: artifact.URI, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes,
			}
		}
		result.Result = &api.RunResult{
			Outcome: string(item.Result.Outcome), ExitCode: item.Result.ExitCode,
			ErrorMessage: item.Result.ErrorMessage, Metrics: cloneValue(item.Result.Metrics), Artifacts: artifacts,
		}
	}
	return result
}

func workerToAPI(item domain.Worker) api.Worker {
	return api.Worker{
		ID: item.ID, Name: item.Name, Adapter: item.Adapter,
		Labels: copyStringMap(item.Labels), State: string(item.State), Version: item.Version,
		Resources:   resourcesToAPI(item.Resources),
		ActiveRunID: item.ActiveRunID, ActiveAttemptID: item.ActiveAttemptID,
		LastHeartbeatAt: item.LastHeartbeatAt, RegisteredAt: item.RegisteredAt,
	}
}

func requirementsFromAPI(item api.ResourceRequirements) domain.ResourceRequirements {
	return domain.ResourceRequirements{GPUCount: item.GPUCount, MinFreeGPUMemoryBytes: item.MinFreeGPUMemoryBytes}
}
func requirementsToAPI(item domain.ResourceRequirements) api.ResourceRequirements {
	return api.ResourceRequirements{GPUCount: item.GPUCount, MinFreeGPUMemoryBytes: item.MinFreeGPUMemoryBytes}
}
func resourcesFromAPI(item api.WorkerResources) domain.WorkerResources {
	gpus := make([]domain.GPUResource, len(item.GPUs))
	for i, gpu := range item.GPUs {
		gpus[i] = domain.GPUResource{ID: gpu.ID, Index: gpu.Index, Name: gpu.Name, Vendor: gpu.Vendor, TotalMemoryBytes: gpu.TotalMemoryBytes, FreeMemoryBytes: gpu.FreeMemoryBytes, UtilizationPercent: gpu.UtilizationPercent, TemperatureCelsius: gpu.TemperatureCelsius}
	}
	return domain.WorkerResources{GPUs: gpus, ObservedAt: item.ObservedAt, ProbeStatus: item.ProbeStatus}
}
func resourcesPointerFromAPI(item *api.WorkerResources) *domain.WorkerResources {
	if item == nil {
		return nil
	}
	value := resourcesFromAPI(*item)
	return &value
}
func resourcesToAPI(item domain.WorkerResources) api.WorkerResources {
	gpus := make([]api.GPUResource, len(item.GPUs))
	for i, gpu := range item.GPUs {
		gpus[i] = api.GPUResource{ID: gpu.ID, Index: gpu.Index, Name: gpu.Name, Vendor: gpu.Vendor, TotalMemoryBytes: gpu.TotalMemoryBytes, FreeMemoryBytes: gpu.FreeMemoryBytes, UtilizationPercent: gpu.UtilizationPercent, TemperatureCelsius: gpu.TemperatureCelsius}
	}
	return api.WorkerResources{GPUs: gpus, ObservedAt: item.ObservedAt, ProbeStatus: item.ProbeStatus}
}
func allocationToAPI(item domain.ResourceAllocation) api.ResourceAllocation {
	resources := resourcesToAPI(domain.WorkerResources{GPUs: item.GPUs})
	return api.ResourceAllocation{WorkerID: item.WorkerID, GPUIDs: append([]string(nil), item.GPUIDs...), GPUs: resources.GPUs, ResourcesObservedAt: item.ResourcesObservedAt}
}
func resultToAPI(item *domain.RunResult) *api.RunResult {
	if item == nil {
		return nil
	}
	artifacts := make([]api.Artifact, len(item.Artifacts))
	for i, artifact := range item.Artifacts {
		artifacts[i] = api.Artifact{Name: artifact.Name, URI: artifact.URI, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes}
	}
	return &api.RunResult{Outcome: string(item.Outcome), ExitCode: item.ExitCode, ErrorMessage: item.ErrorMessage, Metrics: cloneValue(item.Metrics), Artifacts: artifacts}
}
func attemptToAPI(item domain.Attempt) api.Attempt {
	result := api.Attempt{ID: item.ID, RunID: item.RunID, Number: item.Number, WorkerID: item.WorkerID, State: string(item.State), Fence: item.Fence, LeaseExpiresAt: item.LeaseExpiresAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, StartedAt: cloneTimePointer(item.StartedAt), FinishedAt: cloneTimePointer(item.FinishedAt), Result: resultToAPI(item.Result)}
	if item.Allocation != nil {
		allocation := allocationToAPI(*item.Allocation)
		result.Allocation = &allocation
	}
	return result
}

func cloneTimePointer(input *time.Time) *time.Time {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func mapAPIError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: %v", api.ErrNotFound, err)
	case errors.Is(err, ErrLeaseLost):
		return fmt.Errorf("%w: %v", api.ErrLeaseLost, err)
	case errors.Is(err, ErrInvalid), errors.Is(err, domain.ErrInvalid):
		return fmt.Errorf("%w: %v", api.ErrInvalid, err)
	case errors.Is(err, ErrConflict), errors.Is(err, ErrIdempotencyConflict),
		errors.Is(err, ErrInvalidTransition), errors.Is(err, domain.ErrInvalidTransition):
		return fmt.Errorf("%w: %v", api.ErrConflict, err)
	default:
		return err
	}
}

var _ api.Service = (*APIService)(nil)

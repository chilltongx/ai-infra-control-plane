package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chilltongx/ai-infra-control-plane/internal/domain"
)

func (s *JSONStore) RegisterWorker(ctx context.Context, params RegisterWorkerParams) (domain.Worker, error) {
	params.ID = strings.TrimSpace(params.ID)
	params.Name = strings.TrimSpace(params.Name)
	params.Adapter = strings.TrimSpace(params.Adapter)
	params.Version = strings.TrimSpace(params.Version)
	params.SessionID = strings.TrimSpace(params.SessionID)
	if params.Name == "" || params.Adapter == "" {
		return domain.Worker{}, fmt.Errorf("%w: worker name and adapter are required", ErrInvalid)
	}
	if err := domain.ValidateWorkerResources(params.Resources); err != nil {
		return domain.Worker{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var result domain.Worker
	err := s.mutate(ctx, func(state *snapshot) error {
		id := params.ID
		if id == "" {
			var err error
			id, err = s.id("worker")
			if err != nil {
				return fmt.Errorf("generate worker ID: %w", err)
			}
		}
		now := s.now().UTC()
		if current, exists := state.Workers[id]; exists {
			if current.Adapter != params.Adapter {
				return fmt.Errorf("%w: worker adapter is immutable", ErrConflict)
			}
			if current.ActiveAttemptID != "" && current.SessionID != params.SessionID {
				return fmt.Errorf("%w: worker has an active allocation owned by another session", ErrConflict)
			}
			current.Name = params.Name
			current.Labels = copyStringMap(params.Labels)
			current.Version = params.Version
			current.SessionID = params.SessionID
			current.Resources = normalizeResources(params.Resources, now)
			current.State = domain.WorkerOnline
			current.LastHeartbeatAt = now
			state.Workers[id] = current
			result = cloneValue(current)
			return nil
		}
		worker := domain.Worker{
			ID: id, Name: params.Name, Adapter: params.Adapter,
			Labels: copyStringMap(params.Labels), State: domain.WorkerOnline,
			Version: params.Version, SessionID: params.SessionID, Resources: normalizeResources(params.Resources, now),
			LastHeartbeatAt: now, RegisteredAt: now,
		}
		state.Workers[id] = worker
		result = cloneValue(worker)
		return nil
	})
	return result, err
}

func (s *JSONStore) HeartbeatWorker(ctx context.Context, params WorkerHeartbeatParams) (domain.Worker, error) {
	params.WorkerID = strings.TrimSpace(params.WorkerID)
	params.SessionID = strings.TrimSpace(params.SessionID)
	if params.WorkerID == "" {
		return domain.Worker{}, fmt.Errorf("%w: worker ID is required", ErrInvalid)
	}
	if params.Resources != nil {
		if err := domain.ValidateWorkerResources(*params.Resources); err != nil {
			return domain.Worker{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if params.State != "" {
		if err := domain.ValidateWorkerState(params.State); err != nil {
			return domain.Worker{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	var result domain.Worker
	err := s.mutate(ctx, func(state *snapshot) error {
		worker, exists := state.Workers[params.WorkerID]
		if !exists {
			return ErrNotFound
		}
		if worker.SessionID != params.SessionID {
			return fmt.Errorf("%w: stale worker session", ErrConflict)
		}
		if params.State != "" {
			worker.State = params.State
		}
		if params.Labels != nil {
			worker.Labels = copyStringMap(params.Labels)
		}
		if params.Version != "" {
			worker.Version = strings.TrimSpace(params.Version)
		}
		now := s.now().UTC()
		if params.Resources != nil {
			worker.Resources = normalizeResources(*params.Resources, now)
		}
		worker.LastHeartbeatAt = now
		state.Workers[worker.ID] = worker
		result = cloneValue(worker)
		return nil
	})
	return result, err
}

func (s *JSONStore) ListWorkers(ctx context.Context) ([]domain.Worker, error) {
	var result []domain.Worker
	err := s.read(ctx, func(state snapshot) error {
		result = make([]domain.Worker, 0, len(state.Workers))
		now := s.now().UTC()
		for _, worker := range state.Workers {
			projected := cloneValue(worker)
			if !workerFresh(projected, now, s.workerHeartbeatTTL) {
				projected.State = domain.WorkerOffline
			}
			result = append(result, projected)
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].RegisteredAt.Equal(result[j].RegisteredAt) {
				return result[i].ID > result[j].ID
			}
			return result[i].RegisteredAt.After(result[j].RegisteredAt)
		})
		return nil
	})
	return result, err
}

func normalizeResources(resources domain.WorkerResources, observedAt time.Time) domain.WorkerResources {
	resources.ObservedAt = observedAt
	resources.GPUs = cloneValue(resources.GPUs)
	return resources
}

func workerFresh(worker domain.Worker, now time.Time, ttl time.Duration) bool {
	return !worker.LastHeartbeatAt.IsZero() && worker.LastHeartbeatAt.Add(ttl).After(now)
}

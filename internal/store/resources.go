package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/chilltongx/ai-infra-control-plane/internal/domain"
)

func (s *JSONStore) CreateExperiment(ctx context.Context, params CreateExperimentParams) (CreateExperimentResult, error) {
	params.Name = strings.TrimSpace(params.Name)
	params.Description = strings.TrimSpace(params.Description)
	params.IdempotencyKey = strings.TrimSpace(params.IdempotencyKey)
	if params.Name == "" {
		return CreateExperimentResult{}, fmt.Errorf("%w: experiment name is required", ErrInvalid)
	}
	requestHash, err := fingerprint(struct {
		Name        string
		Description string
		Labels      map[string]string
	}{params.Name, params.Description, params.Labels})
	if err != nil {
		return CreateExperimentResult{}, err
	}
	var result CreateExperimentResult
	err = s.mutate(ctx, func(state *snapshot) error {
		resourceID, replayed, err := checkIdempotency(*state, "create_experiment", params.IdempotencyKey, requestHash)
		if err != nil {
			return err
		}
		if replayed {
			experiment, exists := state.Experiments[resourceID]
			if !exists {
				return fmt.Errorf("%w: idempotency record points to experiment %q", ErrNotFound, resourceID)
			}
			result = CreateExperimentResult{Experiment: cloneValue(experiment), Replayed: true}
			return nil
		}
		id, err := s.id("exp")
		if err != nil {
			return fmt.Errorf("generate experiment ID: %w", err)
		}
		if _, exists := state.Experiments[id]; exists {
			return fmt.Errorf("%w: generated duplicate experiment ID", ErrConflict)
		}
		now := s.now().UTC()
		experiment := domain.Experiment{
			ID: id, Name: params.Name, Description: params.Description,
			Labels: copyStringMap(params.Labels), CreatedAt: now, UpdatedAt: now,
		}
		state.Experiments[id] = experiment
		rememberIdempotency(state, "create_experiment", params.IdempotencyKey, requestHash, id, now)
		result = CreateExperimentResult{Experiment: cloneValue(experiment)}
		return nil
	})
	return result, err
}

func (s *JSONStore) ListExperiments(ctx context.Context) ([]domain.Experiment, error) {
	var result []domain.Experiment
	err := s.read(ctx, func(state snapshot) error {
		result = sortedExperiments(state.Experiments)
		return nil
	})
	return result, err
}

func (s *JSONStore) GetExperiment(ctx context.Context, id string) (domain.Experiment, error) {
	var result domain.Experiment
	err := s.read(ctx, func(state snapshot) error {
		experiment, exists := state.Experiments[id]
		if !exists {
			return ErrNotFound
		}
		result = cloneValue(experiment)
		return nil
	})
	return result, err
}

func (s *JSONStore) CreateRun(ctx context.Context, params CreateRunParams) (CreateRunResult, error) {
	params.ExperimentID = strings.TrimSpace(params.ExperimentID)
	params.IdempotencyKey = strings.TrimSpace(params.IdempotencyKey)
	if params.ExperimentID == "" {
		return CreateRunResult{}, fmt.Errorf("%w: experiment ID is required", ErrInvalid)
	}
	if err := domain.ValidateRecipe(params.Recipe); err != nil {
		return CreateRunResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := domain.ValidateResourceRequirements(params.ResourceRequirements); err != nil {
		return CreateRunResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if params.MaxAttempts == 0 {
		params.MaxAttempts = domain.DefaultMaxAttempts
	}
	if params.MaxAttempts < 1 {
		return CreateRunResult{}, fmt.Errorf("%w: max attempts must be positive", ErrInvalid)
	}
	requestHash, err := fingerprint(struct {
		ExperimentID         string
		Recipe               domain.Recipe
		RequiredLabels       map[string]string
		ResourceRequirements domain.ResourceRequirements
		Priority             int
		MaxAttempts          int
	}{params.ExperimentID, params.Recipe, params.RequiredLabels, params.ResourceRequirements, params.Priority, params.MaxAttempts})
	if err != nil {
		return CreateRunResult{}, err
	}
	var result CreateRunResult
	err = s.mutate(ctx, func(state *snapshot) error {
		if _, exists := state.Experiments[params.ExperimentID]; !exists {
			return ErrNotFound
		}
		resourceID, replayed, err := checkIdempotency(*state, "create_run", params.IdempotencyKey, requestHash)
		if err != nil {
			return err
		}
		if replayed {
			run, exists := state.Runs[resourceID]
			if !exists {
				return fmt.Errorf("%w: idempotency record points to run %q", ErrNotFound, resourceID)
			}
			result = CreateRunResult{Run: cloneValue(run), Replayed: true}
			return nil
		}
		id, err := s.id("run")
		if err != nil {
			return fmt.Errorf("generate run ID: %w", err)
		}
		if _, exists := state.Runs[id]; exists {
			return fmt.Errorf("%w: generated duplicate run ID", ErrConflict)
		}
		now := s.now().UTC()
		run := domain.Run{
			ID: id, ExperimentID: params.ExperimentID, State: domain.RunQueued,
			Recipe: cloneValue(params.Recipe), RequiredLabels: copyStringMap(params.RequiredLabels),
			ResourceRequirements: params.ResourceRequirements,
			Priority:             params.Priority, MaxAttempts: params.MaxAttempts,
			CreatedAt: now, UpdatedAt: now,
		}
		state.Runs[id] = run
		appendEvent(state, id, "", "run.created", now, map[string]any{"state": string(run.State)})
		rememberIdempotency(state, "create_run", params.IdempotencyKey, requestHash, id, now)
		result = CreateRunResult{Run: cloneValue(run)}
		return nil
	})
	return result, err
}

func (s *JSONStore) RetryRun(ctx context.Context, params RetryRunParams) (CreateRunResult, error) {
	params.RunID = strings.TrimSpace(params.RunID)
	params.IdempotencyKey = strings.TrimSpace(params.IdempotencyKey)
	if params.RunID == "" {
		return CreateRunResult{}, fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	requestHash, err := fingerprint(struct{ RunID string }{params.RunID})
	if err != nil {
		return CreateRunResult{}, err
	}
	var result CreateRunResult
	err = s.mutate(ctx, func(state *snapshot) error {
		source, exists := state.Runs[params.RunID]
		if !exists {
			return ErrNotFound
		}
		if source.State != domain.RunFailed && source.State != domain.RunCancelled {
			return fmt.Errorf("%w: only failed or cancelled runs can be manually retried", ErrInvalidTransition)
		}
		resourceID, replayed, err := checkIdempotency(*state, "retry_run", params.IdempotencyKey, requestHash)
		if err != nil {
			return err
		}
		if replayed {
			run, exists := state.Runs[resourceID]
			if !exists {
				return ErrNotFound
			}
			result = CreateRunResult{Run: cloneValue(run), Replayed: true}
			return nil
		}
		id, err := s.id("run")
		if err != nil {
			return err
		}
		now := s.now().UTC()
		run := domain.Run{
			ID: id, ExperimentID: source.ExperimentID, State: domain.RunQueued,
			Recipe: cloneValue(source.Recipe), RequiredLabels: copyStringMap(source.RequiredLabels),
			ResourceRequirements: source.ResourceRequirements,
			Priority:             source.Priority, MaxAttempts: source.MaxAttempts,
			CreatedAt: now, UpdatedAt: now,
		}
		state.Runs[id] = run
		appendEvent(state, id, "", "run.created", now, map[string]any{"retry_of": source.ID})
		rememberIdempotency(state, "retry_run", params.IdempotencyKey, requestHash, id, now)
		result = CreateRunResult{Run: cloneValue(run)}
		return nil
	})
	return result, err
}

func (s *JSONStore) ListRuns(ctx context.Context, experimentID string) ([]domain.Run, error) {
	var result []domain.Run
	err := s.read(ctx, func(state snapshot) error {
		if experimentID != "" {
			if _, exists := state.Experiments[experimentID]; !exists {
				return ErrNotFound
			}
		}
		for _, run := range state.Runs {
			if experimentID == "" || run.ExperimentID == experimentID {
				result = append(result, cloneValue(run))
			}
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].CreatedAt.Equal(result[j].CreatedAt) {
				return result[i].ID > result[j].ID
			}
			return result[i].CreatedAt.After(result[j].CreatedAt)
		})
		return nil
	})
	return result, err
}

func (s *JSONStore) GetRun(ctx context.Context, id string) (domain.Run, error) {
	var result domain.Run
	err := s.read(ctx, func(state snapshot) error {
		run, exists := state.Runs[id]
		if !exists {
			return ErrNotFound
		}
		result = cloneValue(run)
		return nil
	})
	return result, err
}

func (s *JSONStore) GetAttempt(ctx context.Context, id string) (domain.Attempt, error) {
	var result domain.Attempt
	err := s.read(ctx, func(state snapshot) error {
		attempt, exists := state.Attempts[id]
		if !exists {
			return ErrNotFound
		}
		result = cloneValue(attempt)
		return nil
	})
	return result, err
}

func (s *JSONStore) ListAttempts(ctx context.Context, runID string) ([]domain.Attempt, error) {
	var result []domain.Attempt
	err := s.read(ctx, func(state snapshot) error {
		if _, exists := state.Runs[runID]; !exists {
			return ErrNotFound
		}
		for _, attempt := range state.Attempts {
			if attempt.RunID == runID {
				result = append(result, cloneValue(attempt))
			}
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].Number != result[j].Number {
				return result[i].Number < result[j].Number
			}
			return result[i].ID < result[j].ID
		})
		return nil
	})
	return result, err
}

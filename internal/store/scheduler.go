package store

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chilltongx/ai-infra-control-plane/internal/domain"
)

func (s *JSONStore) Claim(ctx context.Context, params ClaimParams) (ClaimResult, error) {
	params.WorkerID = strings.TrimSpace(params.WorkerID)
	params.SessionID = strings.TrimSpace(params.SessionID)
	if params.WorkerID == "" {
		return ClaimResult{}, fmt.Errorf("%w: worker ID is required", ErrInvalid)
	}
	if params.LeaseTTL == 0 {
		params.LeaseTTL = s.defaultLeaseTTL
	}
	if params.LeaseTTL <= 0 {
		return ClaimResult{}, fmt.Errorf("%w: lease TTL must be positive", ErrInvalid)
	}
	var result ClaimResult
	err := s.mutate(ctx, func(state *snapshot) error {
		worker, exists := state.Workers[params.WorkerID]
		if !exists {
			return ErrNotFound
		}
		if worker.State != domain.WorkerOnline {
			return fmt.Errorf("%w: worker is not online", ErrConflict)
		}
		if worker.SessionID != params.SessionID {
			return fmt.Errorf("%w: stale worker session", ErrConflict)
		}
		now := s.now().UTC()
		if !workerFresh(worker, now, s.workerHeartbeatTTL) {
			return fmt.Errorf("%w: worker heartbeat is stale", ErrConflict)
		}
		for _, attempt := range state.Attempts {
			if attempt.WorkerID == worker.ID && attempt.State.Active() {
				return ErrNoWork
			}
		}
		type candidate struct {
			run        domain.Run
			allocation domain.ResourceAllocation
		}
		candidates := make([]candidate, 0)
		for _, run := range state.Runs {
			if run.State != domain.RunQueued || run.ActiveAttemptID != "" || run.AttemptCount >= run.MaxAttempts {
				continue
			}
			if run.Recipe.Adapter != worker.Adapter || !labelsMatch(run.RequiredLabels, worker.Labels) {
				continue
			}
			allocation, matches := selectAllocation(worker, run.ResourceRequirements, now, s.workerHeartbeatTTL)
			if !matches {
				continue
			}
			candidates = append(candidates, candidate{run: run, allocation: allocation})
		}
		if len(candidates) == 0 {
			return ErrNoWork
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].run.Priority != candidates[j].run.Priority {
				return candidates[i].run.Priority > candidates[j].run.Priority
			}
			if !candidates[i].run.CreatedAt.Equal(candidates[j].run.CreatedAt) {
				return candidates[i].run.CreatedAt.Before(candidates[j].run.CreatedAt)
			}
			return candidates[i].run.ID < candidates[j].run.ID
		})
		run := candidates[0].run
		allocation := cloneValue(candidates[0].allocation)
		experiment, exists := state.Experiments[run.ExperimentID]
		if !exists {
			return fmt.Errorf("%w: run experiment is missing", ErrNotFound)
		}
		attemptID, err := s.id("attempt")
		if err != nil {
			return fmt.Errorf("generate attempt ID: %w", err)
		}
		token, err := s.token()
		if err != nil {
			return fmt.Errorf("generate lease token: %w", err)
		}
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("generate lease token: empty token")
		}
		run.NextFence++
		run.AttemptCount++
		if err := domain.TransitionRun(&run, domain.RunActive, now); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
		}
		run.ActiveAttemptID = attemptID
		runAllocation := cloneValue(allocation)
		run.LastAllocation = &runAllocation
		attempt := domain.Attempt{
			ID: attemptID, RunID: run.ID, Number: run.AttemptCount,
			WorkerID: worker.ID, State: domain.AttemptLeased, Fence: run.NextFence,
			LeaseExpiresAt: now.Add(params.LeaseTTL), CreatedAt: now, UpdatedAt: now,
			Allocation: func() *domain.ResourceAllocation {
				value := cloneValue(allocation)
				return &value
			}(),
		}
		worker.ActiveRunID = run.ID
		worker.ActiveAttemptID = attempt.ID
		state.Runs[run.ID] = run
		state.Attempts[attempt.ID] = attempt
		state.Workers[worker.ID] = worker
		state.LeaseTokens[attempt.ID] = tokenHash(token)
		appendEvent(state, run.ID, attempt.ID, "attempt.leased", now, map[string]any{
			"worker_id": worker.ID, "fence": attempt.Fence, "expires_at": attempt.LeaseExpiresAt, "gpu_ids": allocation.GPUIDs,
		})
		result = ClaimResult{
			Experiment: cloneValue(experiment), Run: cloneValue(run), Attempt: cloneValue(attempt),
			Recipe: cloneValue(run.Recipe), LeaseToken: token, Fence: attempt.Fence,
			LeaseExpiresAt: attempt.LeaseExpiresAt,
			Allocation:     cloneValue(allocation),
		}
		return nil
	})
	return result, err
}

func selectAllocation(worker domain.Worker, requirements domain.ResourceRequirements, now time.Time, freshnessTTL time.Duration) (domain.ResourceAllocation, bool) {
	allocation := domain.ResourceAllocation{WorkerID: worker.ID, ResourcesObservedAt: worker.Resources.ObservedAt}
	if requirements.GPUCount == 0 {
		return allocation, true
	}
	if worker.Resources.ProbeStatus != domain.ResourceProbeOK || worker.Resources.ObservedAt.IsZero() || !worker.Resources.ObservedAt.Add(freshnessTTL).After(now) {
		return domain.ResourceAllocation{}, false
	}
	eligible := make([]domain.GPUResource, 0, len(worker.Resources.GPUs))
	for _, gpu := range worker.Resources.GPUs {
		if gpu.FreeMemoryBytes >= requirements.MinFreeGPUMemoryBytes {
			eligible = append(eligible, gpu)
		}
	}
	if len(eligible) < requirements.GPUCount {
		return domain.ResourceAllocation{}, false
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].FreeMemoryBytes != eligible[j].FreeMemoryBytes {
			return eligible[i].FreeMemoryBytes < eligible[j].FreeMemoryBytes
		}
		return eligible[i].ID < eligible[j].ID
	})
	allocation.GPUs = cloneValue(eligible[:requirements.GPUCount])
	allocation.GPUIDs = make([]string, len(allocation.GPUs))
	for index, gpu := range allocation.GPUs {
		allocation.GPUIDs[index] = gpu.ID
	}
	return allocation, true
}

func releaseWorkerReservation(state *snapshot, attempt domain.Attempt) {
	worker, exists := state.Workers[attempt.WorkerID]
	if !exists || worker.ActiveAttemptID != attempt.ID {
		return
	}
	worker.ActiveAttemptID = ""
	worker.ActiveRunID = ""
	state.Workers[worker.ID] = worker
}

func labelsMatch(required, actual map[string]string) bool {
	for key, expected := range required {
		if actual[key] != expected {
			return false
		}
	}
	return true
}

func (s *JSONStore) StartAttempt(ctx context.Context, params LeaseOperationParams) (domain.Run, error) {
	var result domain.Run
	err := s.mutate(ctx, func(state *snapshot) error {
		run, attempt, err := s.authorizeLease(*state, params)
		if err != nil {
			return err
		}
		if attempt.State == domain.AttemptRunning {
			result = cloneValue(run)
			return nil
		}
		if attempt.State != domain.AttemptLeased {
			return fmt.Errorf("%w: attempt state %q cannot start", ErrInvalidTransition, attempt.State)
		}
		now := s.now().UTC()
		if err := domain.TransitionAttempt(&attempt, domain.AttemptRunning, now); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
		}
		state.Attempts[attempt.ID] = attempt
		appendEvent(state, run.ID, attempt.ID, "attempt.started", now, map[string]any{"fence": attempt.Fence})
		result = cloneValue(run)
		return nil
	})
	return result, err
}

func (s *JSONStore) Heartbeat(ctx context.Context, params HeartbeatParams) (domain.Lease, error) {
	if params.ExtendBy == 0 {
		params.ExtendBy = s.defaultLeaseTTL
	}
	if params.ExtendBy <= 0 {
		return domain.Lease{}, fmt.Errorf("%w: heartbeat extension must be positive", ErrInvalid)
	}
	var result domain.Lease
	err := s.mutate(ctx, func(state *snapshot) error {
		run, attempt, err := s.authorizeLease(*state, params.LeaseOperationParams)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		attempt.LeaseExpiresAt = now.Add(params.ExtendBy)
		attempt.UpdatedAt = now
		state.Attempts[attempt.ID] = attempt
		result = domain.Lease{
			AttemptID: attempt.ID, RunID: run.ID, WorkerID: attempt.WorkerID,
			Fence: attempt.Fence, ExpiresAt: attempt.LeaseExpiresAt,
		}
		return nil
	})
	return result, err
}

func (s *JSONStore) CompleteAttempt(ctx context.Context, params CompleteAttemptParams) (CompleteAttemptResult, error) {
	if params.Outcome != domain.OutcomeSucceeded && params.Outcome != domain.OutcomeFailed && params.Outcome != domain.OutcomeCancelled {
		return CompleteAttemptResult{}, fmt.Errorf("%w: unsupported outcome %q", ErrInvalid, params.Outcome)
	}
	params.RunID = strings.TrimSpace(params.RunID)
	params.AttemptID = strings.TrimSpace(params.AttemptID)
	requestHash, err := completionRequestHash(params)
	if err != nil {
		return CompleteAttemptResult{}, fmt.Errorf("%w: fingerprint completion: %v", ErrInvalid, err)
	}
	var result CompleteAttemptResult
	err = s.mutate(ctx, func(state *snapshot) error {
		if receipt, exists := state.Completions[params.AttemptID]; exists {
			if !completionCredentialMatches(receipt, params) {
				return ErrLeaseLost
			}
			if receipt.RequestHash != requestHash {
				return fmt.Errorf("%w: completion payload differs from accepted request", ErrConflict)
			}
			result = cloneValue(receipt.Result)
			return nil
		}
		run, attempt, err := s.authorizeLease(*state, params.LeaseOperationParams)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		var exitCode *int
		if params.ExitCode != nil {
			value := *params.ExitCode
			exitCode = &value
		}
		resultValue := domain.RunResult{
			Outcome: params.Outcome, ExitCode: exitCode,
			ErrorMessage: params.ErrorMessage, Metrics: cloneValue(params.Metrics), Artifacts: cloneValue(params.Artifacts),
		}
		attemptState := domain.AttemptFailed
		switch params.Outcome {
		case domain.OutcomeSucceeded:
			attemptState = domain.AttemptSucceeded
		case domain.OutcomeCancelled:
			attemptState = domain.AttemptCancelled
		}
		if err := domain.TransitionAttempt(&attempt, attemptState, now); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
		}
		attempt.Result = &resultValue
		run.ActiveAttemptID = ""
		delete(state.LeaseTokens, attempt.ID)
		cancelled := run.CancelRequestedAt != nil
		if cancelled {
			cancelResult := resultValue
			cancelResult.Outcome = domain.OutcomeCancelled
			if err := domain.TransitionRun(&run, domain.RunCancelled, now); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
			}
			run.Result = &cancelResult
			// A cooperative completion after cancel is observed as cancelled even
			// when the executor reports success. This is the terminal-state fence.
			if attempt.State != domain.AttemptCancelled {
				attempt.State = domain.AttemptCancelled
				attempt.Result = &cancelResult
			}
		} else if params.Outcome == domain.OutcomeSucceeded {
			if err := domain.TransitionRun(&run, domain.RunSucceeded, now); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
			}
			run.Result = &resultValue
		} else if params.Outcome == domain.OutcomeCancelled {
			if err := domain.TransitionRun(&run, domain.RunCancelled, now); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
			}
			run.Result = &resultValue
		} else if run.AttemptCount < run.MaxAttempts {
			if err := domain.TransitionRun(&run, domain.RunQueued, now); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
			}
			result.RetryScheduled = true
		} else {
			if err := domain.TransitionRun(&run, domain.RunFailed, now); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
			}
			run.Result = &resultValue
		}
		state.Attempts[attempt.ID] = attempt
		state.Runs[run.ID] = run
		releaseWorkerReservation(state, attempt)
		appendEvent(state, run.ID, attempt.ID, "attempt.completed", now, map[string]any{
			"outcome": string(attempt.Result.Outcome), "run_state": string(run.State), "retry_scheduled": result.RetryScheduled,
		})
		result.Run = cloneValue(run)
		result.Attempt = cloneValue(attempt)
		state.Completions[attempt.ID] = completionRecord{
			RunID: run.ID, AttemptID: attempt.ID, Fence: attempt.Fence,
			TokenHash: tokenHash(params.LeaseToken), RequestHash: requestHash,
			Result: cloneValue(result), CreatedAt: now,
		}
		return nil
	})
	return result, err
}

func completionRequestHash(params CompleteAttemptParams) (string, error) {
	return fingerprint(struct {
		RunID        string             `json:"run_id"`
		AttemptID    string             `json:"attempt_id"`
		Fence        int64              `json:"fence"`
		Outcome      domain.Outcome     `json:"outcome"`
		ExitCode     *int               `json:"exit_code,omitempty"`
		ErrorMessage string             `json:"error_message,omitempty"`
		Metrics      map[string]float64 `json:"metrics,omitempty"`
		Artifacts    []domain.Artifact  `json:"artifacts,omitempty"`
	}{
		RunID: params.RunID, AttemptID: params.AttemptID, Fence: params.Fence,
		Outcome: params.Outcome, ExitCode: params.ExitCode, ErrorMessage: params.ErrorMessage,
		Metrics: params.Metrics, Artifacts: params.Artifacts,
	})
}

func completionCredentialMatches(receipt completionRecord, params CompleteAttemptParams) bool {
	if params.AttemptID == "" || params.Fence <= 0 || strings.TrimSpace(params.LeaseToken) == "" {
		return false
	}
	if params.RunID != "" && params.RunID != receipt.RunID {
		return false
	}
	if params.Fence != receipt.Fence {
		return false
	}
	providedHash := tokenHash(params.LeaseToken)
	return subtle.ConstantTimeCompare([]byte(receipt.TokenHash), []byte(providedHash)) == 1
}

func (s *JSONStore) CancelRun(ctx context.Context, params CancelRunParams) (domain.Run, error) {
	params.RunID = strings.TrimSpace(params.RunID)
	if params.RunID == "" {
		return domain.Run{}, fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	var result domain.Run
	err := s.mutate(ctx, func(state *snapshot) error {
		run, exists := state.Runs[params.RunID]
		if !exists {
			return ErrNotFound
		}
		if run.State == domain.RunCancelled {
			result = cloneValue(run)
			return nil
		}
		if run.State.Terminal() {
			return fmt.Errorf("%w: terminal run cannot be cancelled", ErrInvalidTransition)
		}
		now := s.now().UTC()
		run.CancelRequestedAt = &now
		run.CancellationReason = strings.TrimSpace(params.Reason)
		if run.ActiveAttemptID == "" {
			if err := domain.TransitionRun(&run, domain.RunCancelled, now); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
			}
			run.Result = &domain.RunResult{Outcome: domain.OutcomeCancelled, ErrorMessage: run.CancellationReason}
		}
		run.UpdatedAt = now
		state.Runs[run.ID] = run
		appendEvent(state, run.ID, run.ActiveAttemptID, "run.cancel_requested", now, map[string]any{"reason": run.CancellationReason})
		result = cloneValue(run)
		return nil
	})
	return result, err
}

func (s *JSONStore) ExpireLeases(ctx context.Context) (ExpireLeasesResult, error) {
	var result ExpireLeasesResult
	err := s.mutate(ctx, func(state *snapshot) error {
		now := s.now().UTC()
		attemptIDs := make([]string, 0)
		for id, attempt := range state.Attempts {
			if attempt.State.Active() && !attempt.LeaseExpiresAt.After(now) {
				attemptIDs = append(attemptIDs, id)
			}
		}
		sort.Strings(attemptIDs)
		for _, attemptID := range attemptIDs {
			attempt := state.Attempts[attemptID]
			run, exists := state.Runs[attempt.RunID]
			if !exists || run.ActiveAttemptID != attempt.ID {
				continue
			}
			if err := domain.TransitionAttempt(&attempt, domain.AttemptLost, now); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
			}
			attempt.Result = &domain.RunResult{Outcome: domain.OutcomeFailed, ErrorMessage: "lease expired"}
			run.ActiveAttemptID = ""
			delete(state.LeaseTokens, attempt.ID)
			retry := false
			if run.CancelRequestedAt != nil {
				if err := domain.TransitionRun(&run, domain.RunCancelled, now); err != nil {
					return err
				}
				run.Result = &domain.RunResult{Outcome: domain.OutcomeCancelled, ErrorMessage: run.CancellationReason}
			} else if run.AttemptCount < run.MaxAttempts {
				if err := domain.TransitionRun(&run, domain.RunQueued, now); err != nil {
					return err
				}
				retry = true
			} else {
				if err := domain.TransitionRun(&run, domain.RunFailed, now); err != nil {
					return err
				}
				run.Result = &domain.RunResult{Outcome: domain.OutcomeFailed, ErrorMessage: "lease expired"}
			}
			state.Attempts[attempt.ID] = attempt
			state.Runs[run.ID] = run
			releaseWorkerReservation(state, attempt)
			appendEvent(state, run.ID, attempt.ID, "attempt.lease_expired", now, map[string]any{"retry_scheduled": retry})
			result.Expired = append(result.Expired, ExpiredLease{RunID: run.ID, AttemptID: attempt.ID, RetryScheduled: retry})
		}
		return nil
	})
	return result, err
}

func (s *JSONStore) authorizeLease(state snapshot, params LeaseOperationParams) (domain.Run, domain.Attempt, error) {
	if params.AttemptID == "" || params.Fence <= 0 || strings.TrimSpace(params.LeaseToken) == "" {
		return domain.Run{}, domain.Attempt{}, fmt.Errorf("%w: attempt ID, fence, and lease token are required", ErrInvalid)
	}
	attempt, exists := state.Attempts[params.AttemptID]
	if !exists {
		return domain.Run{}, domain.Attempt{}, ErrLeaseLost
	}
	run, exists := state.Runs[attempt.RunID]
	if !exists {
		return domain.Run{}, domain.Attempt{}, ErrLeaseLost
	}
	if params.RunID != "" && params.RunID != run.ID {
		return domain.Run{}, domain.Attempt{}, ErrLeaseLost
	}
	if params.WorkerID != "" && params.WorkerID != attempt.WorkerID {
		return domain.Run{}, domain.Attempt{}, ErrLeaseLost
	}
	if !attempt.State.Active() || run.State != domain.RunActive || run.ActiveAttemptID != attempt.ID || attempt.Fence != params.Fence {
		return domain.Run{}, domain.Attempt{}, ErrLeaseLost
	}
	if !attempt.LeaseExpiresAt.After(s.now().UTC()) {
		return domain.Run{}, domain.Attempt{}, ErrLeaseLost
	}
	expectedHash, exists := state.LeaseTokens[attempt.ID]
	providedHash := tokenHash(params.LeaseToken)
	if !exists || subtle.ConstantTimeCompare([]byte(expectedHash), []byte(providedHash)) != 1 {
		return domain.Run{}, domain.Attempt{}, ErrLeaseLost
	}
	return run, attempt, nil
}

func (s *JSONStore) ListEvents(ctx context.Context, runID string, after int64, limit int) ([]domain.Event, error) {
	if after < 0 {
		return nil, fmt.Errorf("%w: event cursor cannot be negative", ErrInvalid)
	}
	if limit <= 0 {
		limit = 100
	}
	var result []domain.Event
	err := s.read(ctx, func(state snapshot) error {
		if _, exists := state.Runs[runID]; !exists {
			return ErrNotFound
		}
		for _, event := range state.Events {
			if event.RunID == runID && event.Sequence > after {
				result = append(result, cloneValue(event))
				if len(result) == limit {
					break
				}
			}
		}
		return nil
	})
	return result, err
}

var _ Store = (*JSONStore)(nil)

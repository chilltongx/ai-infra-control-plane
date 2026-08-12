// Package store provides the durable scheduling boundary for the control
// plane. JSONStore is intentionally single-process: every mutation is applied
// to a private snapshot, fsynced, atomically renamed, and only then published
// to readers in memory.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chilltongx/ai-infra-control-plane/internal/domain"
)

var (
	ErrNotFound            = errors.New("store: not found")
	ErrNoWork              = errors.New("store: no eligible work")
	ErrConflict            = errors.New("store: conflict")
	ErrInvalid             = errors.New("store: invalid argument")
	ErrIdempotencyConflict = errors.New("store: idempotency key reused with different request")
	ErrLeaseLost           = errors.New("store: lease lost")
	ErrInvalidTransition   = errors.New("store: invalid state transition")
)

// Store is the transport-independent control-plane persistence contract.
type Store interface {
	Ready(context.Context) error
	CreateExperiment(context.Context, CreateExperimentParams) (CreateExperimentResult, error)
	ListExperiments(context.Context) ([]domain.Experiment, error)
	GetExperiment(context.Context, string) (domain.Experiment, error)
	CreateRun(context.Context, CreateRunParams) (CreateRunResult, error)
	RetryRun(context.Context, RetryRunParams) (CreateRunResult, error)
	ListRuns(context.Context, string) ([]domain.Run, error)
	GetRun(context.Context, string) (domain.Run, error)
	GetAttempt(context.Context, string) (domain.Attempt, error)
	ListAttempts(context.Context, string) ([]domain.Attempt, error)
	RegisterWorker(context.Context, RegisterWorkerParams) (domain.Worker, error)
	HeartbeatWorker(context.Context, WorkerHeartbeatParams) (domain.Worker, error)
	ListWorkers(context.Context) ([]domain.Worker, error)
	Claim(context.Context, ClaimParams) (ClaimResult, error)
	StartAttempt(context.Context, LeaseOperationParams) (domain.Run, error)
	Heartbeat(context.Context, HeartbeatParams) (domain.Lease, error)
	CompleteAttempt(context.Context, CompleteAttemptParams) (CompleteAttemptResult, error)
	CancelRun(context.Context, CancelRunParams) (domain.Run, error)
	ExpireLeases(context.Context) (ExpireLeasesResult, error)
	ListEvents(context.Context, string, int64, int) ([]domain.Event, error)
}

type CreateExperimentParams struct {
	Name           string
	Description    string
	Labels         map[string]string
	IdempotencyKey string
}

type CreateExperimentResult struct {
	Experiment domain.Experiment
	Replayed   bool
}

type CreateRunParams struct {
	ExperimentID         string
	Recipe               domain.Recipe
	RequiredLabels       map[string]string
	ResourceRequirements domain.ResourceRequirements
	Priority             int
	MaxAttempts          int
	IdempotencyKey       string
}

type CreateRunResult struct {
	Run      domain.Run
	Replayed bool
}

type RetryRunParams struct {
	RunID          string
	IdempotencyKey string
}

type RegisterWorkerParams struct {
	ID        string
	Name      string
	Adapter   string
	Labels    map[string]string
	Version   string
	SessionID string
	Resources domain.WorkerResources
}

type WorkerHeartbeatParams struct {
	WorkerID  string
	SessionID string
	State     domain.WorkerState
	Labels    map[string]string
	Version   string
	Resources *domain.WorkerResources
}

type ClaimParams struct {
	WorkerID  string
	SessionID string
	LeaseTTL  time.Duration
}

type ClaimResult struct {
	Experiment     domain.Experiment         `json:"experiment"`
	Run            domain.Run                `json:"run"`
	Attempt        domain.Attempt            `json:"attempt"`
	Recipe         domain.Recipe             `json:"recipe"`
	LeaseToken     string                    `json:"lease_token"`
	Fence          int64                     `json:"fence"`
	LeaseExpiresAt time.Time                 `json:"lease_expires_at"`
	Allocation     domain.ResourceAllocation `json:"allocation"`
}

type LeaseOperationParams struct {
	RunID      string
	AttemptID  string
	WorkerID   string
	Fence      int64
	LeaseToken string
}

type HeartbeatParams struct {
	LeaseOperationParams
	ExtendBy time.Duration
}

type CompleteAttemptParams struct {
	LeaseOperationParams
	Outcome      domain.Outcome
	ExitCode     *int
	ErrorMessage string
	Metrics      map[string]float64
	Artifacts    []domain.Artifact
}

type CompleteAttemptResult struct {
	Run            domain.Run
	Attempt        domain.Attempt
	RetryScheduled bool
}

type CancelRunParams struct {
	RunID  string
	Reason string
}

type ExpiredLease struct {
	RunID          string
	AttemptID      string
	RetryScheduled bool
}

type ExpireLeasesResult struct {
	Expired []ExpiredLease
}

type Option func(*options) error

type options struct {
	now                func() time.Time
	id                 func(string) (string, error)
	token              func() (string, error)
	defaultLeaseTTL    time.Duration
	workerHeartbeatTTL time.Duration
}

func WithWorkerHeartbeatTTL(ttl time.Duration) Option {
	return func(o *options) error {
		if ttl <= 0 {
			return fmt.Errorf("%w: worker heartbeat TTL must be positive", ErrInvalid)
		}
		o.workerHeartbeatTTL = ttl
		return nil
	}
}

func WithClock(now func() time.Time) Option {
	return func(o *options) error {
		if now == nil {
			return fmt.Errorf("%w: clock is nil", ErrInvalid)
		}
		o.now = now
		return nil
	}
}

func WithIDGenerator(generator func(string) (string, error)) Option {
	return func(o *options) error {
		if generator == nil {
			return fmt.Errorf("%w: id generator is nil", ErrInvalid)
		}
		o.id = generator
		return nil
	}
}

func WithTokenGenerator(generator func() (string, error)) Option {
	return func(o *options) error {
		if generator == nil {
			return fmt.Errorf("%w: token generator is nil", ErrInvalid)
		}
		o.token = generator
		return nil
	}
}

func WithDefaultLeaseTTL(ttl time.Duration) Option {
	return func(o *options) error {
		if ttl <= 0 {
			return fmt.Errorf("%w: lease TTL must be positive", ErrInvalid)
		}
		o.defaultLeaseTTL = ttl
		return nil
	}
}

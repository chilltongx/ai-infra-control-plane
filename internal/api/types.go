package api

import (
	"context"
	"errors"
	"time"
)

// Service is the control-plane boundary consumed by the HTTP transport.
// Implementations own persistence, state transitions, lease validation, and
// idempotency; handlers only validate and translate the wire protocol.
type Service interface {
	CreateExperiment(context.Context, CreateExperimentInput) (Experiment, error)
	ListExperiments(context.Context, int) ([]Experiment, error)
	GetExperiment(context.Context, string) (Experiment, error)
	CreateRun(context.Context, CreateRunInput) (Run, error)
	ListRuns(context.Context, string, int) ([]Run, error)
	GetRun(context.Context, string) (Run, error)
	ListAttempts(context.Context, string, int) ([]Attempt, error)
	CancelRun(context.Context, string, string) (Run, error)
	RegisterWorker(context.Context, RegisterWorkerInput) (Worker, error)
	ListWorkers(context.Context, int) ([]Worker, error)
	HeartbeatWorker(context.Context, string, WorkerHeartbeatInput) (Worker, error)
	Claim(context.Context, ClaimInput) (Claim, bool, error)
	StartAttempt(context.Context, AttemptMutationInput) (Run, error)
	HeartbeatAttempt(context.Context, AttemptMutationInput) (Lease, error)
	CompleteAttempt(context.Context, CompleteAttemptInput) (Run, error)
	ListEvents(context.Context, string, int64, int) ([]Event, error)
	Ready(context.Context) error
}

var (
	ErrNotFound    = errors.New("not found")
	ErrConflict    = errors.New("conflict")
	ErrInvalid     = errors.New("invalid argument")
	ErrLeaseLost   = errors.New("lease lost")
	ErrUnavailable = errors.New("unavailable")
)

type Experiment struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type CreateExperimentInput struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type Run struct {
	ID                   string               `json:"id"`
	ExperimentID         string               `json:"experiment_id"`
	State                string               `json:"state"`
	Recipe               Recipe               `json:"recipe"`
	RequiredLabels       map[string]string    `json:"required_labels,omitempty"`
	ResourceRequirements ResourceRequirements `json:"resource_requirements,omitempty"`
	Priority             int                  `json:"priority"`
	MaxAttempts          int                  `json:"max_attempts"`
	AttemptCount         int                  `json:"attempt_count"`
	ActiveAttemptID      string               `json:"active_attempt_id,omitempty"`
	CancelRequestedAt    *time.Time           `json:"cancel_requested_at,omitempty"`
	CancellationReason   string               `json:"cancellation_reason,omitempty"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
	FinishedAt           *time.Time           `json:"finished_at,omitempty"`
	Result               *RunResult           `json:"result,omitempty"`
	LastAllocation       *ResourceAllocation  `json:"last_allocation,omitempty"`
}

type Recipe struct {
	Adapter         string            `json:"adapter"`
	Command         []string          `json:"command"`
	WorkingDir      string            `json:"working_dir,omitempty"`
	Environment     map[string]string `json:"environment,omitempty"`
	ExpectedOutputs []string          `json:"expected_outputs,omitempty"`
	TimeoutSeconds  int               `json:"timeout_seconds,omitempty"`
}

type CreateRunInput struct {
	ExperimentID         string               `json:"experiment_id"`
	Recipe               Recipe               `json:"recipe"`
	RequiredLabels       map[string]string    `json:"required_labels,omitempty"`
	ResourceRequirements ResourceRequirements `json:"resource_requirements,omitempty"`
	Priority             int                  `json:"priority,omitempty"`
	MaxAttempts          int                  `json:"max_attempts,omitempty"`
}

type ResourceRequirements struct {
	GPUCount              int   `json:"gpu_count,omitempty"`
	MinFreeGPUMemoryBytes int64 `json:"min_free_gpu_memory_bytes,omitempty"`
}

type RunResult struct {
	Outcome      string             `json:"outcome"`
	ExitCode     *int               `json:"exit_code,omitempty"`
	ErrorMessage string             `json:"error_message,omitempty"`
	Metrics      map[string]float64 `json:"metrics,omitempty"`
	Artifacts    []Artifact         `json:"artifacts,omitempty"`
}

type Artifact struct {
	Name      string `json:"name"`
	URI       string `json:"uri"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

type Worker struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Adapter         string            `json:"adapter"`
	Labels          map[string]string `json:"labels,omitempty"`
	State           string            `json:"state"`
	Version         string            `json:"version,omitempty"`
	Resources       WorkerResources   `json:"resources,omitempty"`
	ActiveRunID     string            `json:"active_run_id,omitempty"`
	ActiveAttemptID string            `json:"active_attempt_id,omitempty"`
	LastHeartbeatAt time.Time         `json:"last_heartbeat_at"`
	RegisteredAt    time.Time         `json:"registered_at"`
}

type RegisterWorkerInput struct {
	ID        string            `json:"id,omitempty"`
	Name      string            `json:"name"`
	Adapter   string            `json:"adapter"`
	Labels    map[string]string `json:"labels,omitempty"`
	Version   string            `json:"version,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Resources WorkerResources   `json:"resources,omitempty"`
}

type WorkerHeartbeatInput struct {
	State     string            `json:"state,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Version   string            `json:"version,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Resources *WorkerResources  `json:"resources,omitempty"`
}

type WorkerResources struct {
	GPUs        []GPUResource `json:"gpus,omitempty"`
	ObservedAt  time.Time     `json:"observed_at,omitempty"`
	ProbeStatus string        `json:"probe_status,omitempty"`
}

type GPUResource struct {
	ID                 string  `json:"id"`
	Index              int     `json:"index"`
	Name               string  `json:"name"`
	Vendor             string  `json:"vendor"`
	TotalMemoryBytes   int64   `json:"total_memory_bytes"`
	FreeMemoryBytes    int64   `json:"free_memory_bytes"`
	UtilizationPercent float64 `json:"utilization_percent,omitempty"`
	TemperatureCelsius float64 `json:"temperature_celsius,omitempty"`
}

type ResourceAllocation struct {
	WorkerID            string        `json:"worker_id"`
	GPUIDs              []string      `json:"gpu_ids,omitempty"`
	GPUs                []GPUResource `json:"gpus,omitempty"`
	ResourcesObservedAt time.Time     `json:"resources_observed_at,omitempty"`
}

type ClaimInput struct {
	WorkerID    string        `json:"-"`
	SessionID   string        `json:"-"`
	LeaseTTL    time.Duration `json:"-"`
	WaitTimeout time.Duration `json:"-"`
}

type Claim struct {
	AttemptID  string             `json:"attempt_id"`
	RunID      string             `json:"run_id"`
	Fence      int64              `json:"fence"`
	LeaseToken string             `json:"lease_token"`
	ExpiresAt  time.Time          `json:"expires_at"`
	Recipe     Recipe             `json:"recipe"`
	Allocation ResourceAllocation `json:"allocation"`
}

type Attempt struct {
	ID             string              `json:"id"`
	RunID          string              `json:"run_id"`
	Number         int                 `json:"number"`
	WorkerID       string              `json:"worker_id"`
	State          string              `json:"state"`
	Fence          int64               `json:"fence"`
	LeaseExpiresAt time.Time           `json:"lease_expires_at"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	StartedAt      *time.Time          `json:"started_at,omitempty"`
	FinishedAt     *time.Time          `json:"finished_at,omitempty"`
	Result         *RunResult          `json:"result,omitempty"`
	Allocation     *ResourceAllocation `json:"allocation,omitempty"`
}

type AttemptMutationInput struct {
	RunID         string `json:"-"`
	AttemptID     string `json:"-"`
	LeaseToken    string `json:"lease_token"`
	Fence         int64  `json:"fence"`
	ExtendSeconds int    `json:"extend_seconds,omitempty"`
}

type CompleteAttemptInput struct {
	RunID      string             `json:"-"`
	AttemptID  string             `json:"-"`
	LeaseToken string             `json:"lease_token"`
	Fence      int64              `json:"fence"`
	Outcome    string             `json:"outcome"`
	ExitCode   *int               `json:"exit_code,omitempty"`
	Error      string             `json:"error,omitempty"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Artifacts  []Artifact         `json:"artifacts,omitempty"`
}

type Lease struct {
	AttemptID string    `json:"attempt_id"`
	RunID     string    `json:"run_id"`
	Fence     int64     `json:"fence"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Event struct {
	Sequence  int64          `json:"sequence"`
	RunID     string         `json:"run_id"`
	AttemptID string         `json:"attempt_id,omitempty"`
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}

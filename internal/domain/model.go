package domain

import "time"

const DefaultMaxAttempts = 3

// Experiment groups independently schedulable runs under one user-visible
// intent. Its mutable fields are metadata only; execution state lives on Run.
type Experiment struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type Recipe struct {
	Adapter         string            `json:"adapter"`
	Command         []string          `json:"command"`
	WorkingDir      string            `json:"working_dir,omitempty"`
	Environment     map[string]string `json:"environment,omitempty"`
	ExpectedOutputs []string          `json:"expected_outputs,omitempty"`
	TimeoutSeconds  int               `json:"timeout_seconds,omitempty"`
}

type RunState string

const (
	RunQueued    RunState = "queued"
	RunActive    RunState = "active"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
)

func (s RunState) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCancelled
}

type Run struct {
	ID                   string               `json:"id"`
	ExperimentID         string               `json:"experiment_id"`
	State                RunState             `json:"state"`
	Recipe               Recipe               `json:"recipe"`
	RequiredLabels       map[string]string    `json:"required_labels,omitempty"`
	ResourceRequirements ResourceRequirements `json:"resource_requirements,omitempty"`
	Priority             int                  `json:"priority"`
	MaxAttempts          int                  `json:"max_attempts"`
	AttemptCount         int                  `json:"attempt_count"`
	ActiveAttemptID      string               `json:"active_attempt_id,omitempty"`
	NextFence            int64                `json:"next_fence"`
	CancelRequestedAt    *time.Time           `json:"cancel_requested_at,omitempty"`
	CancellationReason   string               `json:"cancellation_reason,omitempty"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
	FinishedAt           *time.Time           `json:"finished_at,omitempty"`
	Result               *RunResult           `json:"result,omitempty"`
	LastAllocation       *ResourceAllocation  `json:"last_allocation,omitempty"`
}

// ResourceRequirements is deliberately small in ForgeGrid V0. Static
// placement such as OS and accelerator vendor remains in RequiredLabels;
// dynamic capacity belongs here instead of being encoded into labels.
type ResourceRequirements struct {
	GPUCount              int   `json:"gpu_count,omitempty"`
	MinFreeGPUMemoryBytes int64 `json:"min_free_gpu_memory_bytes,omitempty"`
}

type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeCancelled Outcome = "cancelled"
)

type RunResult struct {
	Outcome      Outcome            `json:"outcome"`
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

type AttemptState string

const (
	AttemptLeased    AttemptState = "leased"
	AttemptRunning   AttemptState = "running"
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptCancelled AttemptState = "cancelled"
	AttemptLost      AttemptState = "lost"
)

func (s AttemptState) Active() bool {
	return s == AttemptLeased || s == AttemptRunning
}

func (s AttemptState) Terminal() bool {
	return s == AttemptSucceeded || s == AttemptFailed || s == AttemptCancelled || s == AttemptLost
}

type Attempt struct {
	ID             string              `json:"id"`
	RunID          string              `json:"run_id"`
	Number         int                 `json:"number"`
	WorkerID       string              `json:"worker_id"`
	State          AttemptState        `json:"state"`
	Fence          int64               `json:"fence"`
	LeaseExpiresAt time.Time           `json:"lease_expires_at"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	StartedAt      *time.Time          `json:"started_at,omitempty"`
	FinishedAt     *time.Time          `json:"finished_at,omitempty"`
	Result         *RunResult          `json:"result,omitempty"`
	Allocation     *ResourceAllocation `json:"allocation,omitempty"`
}

type WorkerState string

const (
	WorkerOnline   WorkerState = "online"
	WorkerDraining WorkerState = "draining"
	WorkerOffline  WorkerState = "offline"
)

type Worker struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Adapter         string            `json:"adapter"`
	Labels          map[string]string `json:"labels,omitempty"`
	State           WorkerState       `json:"state"`
	Version         string            `json:"version,omitempty"`
	SessionID       string            `json:"session_id,omitempty"`
	Resources       WorkerResources   `json:"resources,omitempty"`
	ActiveRunID     string            `json:"active_run_id,omitempty"`
	ActiveAttemptID string            `json:"active_attempt_id,omitempty"`
	LastHeartbeatAt time.Time         `json:"last_heartbeat_at"`
	RegisteredAt    time.Time         `json:"registered_at"`
}

const (
	ResourceProbeOK          = "ok"
	ResourceProbeUnavailable = "unavailable"
	ResourceProbeError       = "error"
)

// WorkerResources separates frequently changing availability from stable
// labels. Every report is a point-in-time observation made by the worker.
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

// ResourceAllocation is the immutable scheduling evidence captured when an
// attempt is leased. The worker's live report may change after this snapshot.
type ResourceAllocation struct {
	WorkerID            string        `json:"worker_id"`
	GPUIDs              []string      `json:"gpu_ids,omitempty"`
	GPUs                []GPUResource `json:"gpus,omitempty"`
	ResourcesObservedAt time.Time     `json:"resources_observed_at,omitempty"`
}

type Lease struct {
	AttemptID string    `json:"attempt_id"`
	RunID     string    `json:"run_id"`
	WorkerID  string    `json:"worker_id"`
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

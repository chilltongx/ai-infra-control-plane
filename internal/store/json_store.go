package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chilltongx/ai-infra-control-plane/internal/domain"
)

const snapshotSchemaVersion = 1

type idempotencyRecord struct {
	Operation   string    `json:"operation"`
	Key         string    `json:"key"`
	RequestHash string    `json:"request_hash"`
	ResourceID  string    `json:"resource_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// completionRecord is the durable receipt for an accepted attempt completion.
// TokenHash retains only the one-way lease credential proof needed to safely
// recognize a transport retry after the active lease has been retired.
type completionRecord struct {
	RunID       string                `json:"run_id"`
	AttemptID   string                `json:"attempt_id"`
	Fence       int64                 `json:"fence"`
	TokenHash   string                `json:"lease_token_hash"`
	RequestHash string                `json:"request_hash"`
	Result      CompleteAttemptResult `json:"result"`
	CreatedAt   time.Time             `json:"created_at"`
}

type snapshot struct {
	SchemaVersion int                          `json:"schema_version"`
	Revision      uint64                       `json:"revision"`
	Experiments   map[string]domain.Experiment `json:"experiments"`
	Runs          map[string]domain.Run        `json:"runs"`
	Attempts      map[string]domain.Attempt    `json:"attempts"`
	Workers       map[string]domain.Worker     `json:"workers"`
	LeaseTokens   map[string]string            `json:"lease_token_hashes"`
	Completions   map[string]completionRecord  `json:"completion_receipts"`
	Idempotency   map[string]idempotencyRecord `json:"idempotency"`
	Events        []domain.Event               `json:"events"`
	NextEventSeq  int64                        `json:"next_event_sequence"`
}

func emptySnapshot() snapshot {
	return snapshot{
		SchemaVersion: snapshotSchemaVersion,
		Experiments:   make(map[string]domain.Experiment),
		Runs:          make(map[string]domain.Run),
		Attempts:      make(map[string]domain.Attempt),
		Workers:       make(map[string]domain.Worker),
		LeaseTokens:   make(map[string]string),
		Completions:   make(map[string]completionRecord),
		Idempotency:   make(map[string]idempotencyRecord),
		Events:        make([]domain.Event, 0),
	}
}

// JSONStore owns one snapshot file. It is safe for concurrent goroutines but
// must not be opened by multiple processes at once.
type JSONStore struct {
	mu                 sync.RWMutex
	path               string
	state              snapshot
	now                func() time.Time
	id                 func(string) (string, error)
	token              func() (string, error)
	defaultLeaseTTL    time.Duration
	workerHeartbeatTTL time.Duration
}

func NewJSONStore(path string, configured ...Option) (*JSONStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("%w: snapshot path is required", ErrInvalid)
	}
	opts := options{
		now:                time.Now,
		id:                 randomPrefixedID,
		token:              randomToken,
		defaultLeaseTTL:    30 * time.Second,
		workerHeartbeatTTL: 30 * time.Second,
	}
	for _, option := range configured {
		if option == nil {
			continue
		}
		if err := option(&opts); err != nil {
			return nil, err
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o750); err != nil {
		return nil, fmt.Errorf("create snapshot directory: %w", err)
	}
	state, err := readSnapshot(absolute)
	if err != nil {
		return nil, err
	}
	return &JSONStore{
		path:               absolute,
		state:              state,
		now:                opts.now,
		id:                 opts.id,
		token:              opts.token,
		defaultLeaseTTL:    opts.defaultLeaseTTL,
		workerHeartbeatTTL: opts.workerHeartbeatTTL,
	}, nil
}

func readSnapshot(path string) (snapshot, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptySnapshot(), nil
	}
	if err != nil {
		return snapshot{}, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 256<<20))
	decoder.DisallowUnknownFields()
	var state snapshot
	if err := decoder.Decode(&state); err != nil {
		return snapshot{}, fmt.Errorf("decode snapshot: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return snapshot{}, fmt.Errorf("decode snapshot: trailing JSON data")
	}
	if state.SchemaVersion != snapshotSchemaVersion {
		return snapshot{}, fmt.Errorf("unsupported snapshot schema version %d", state.SchemaVersion)
	}
	initializeMaps(&state)
	migrateSnapshot(&state)
	if err := validateSnapshot(state); err != nil {
		return snapshot{}, fmt.Errorf("validate snapshot: %w", err)
	}
	return state, nil
}

// migrateSnapshot upgrades additive invariants that were not represented in
// earlier schema-v1 files. Migration runs only on the private snapshot loaded
// from disk; validation remains read-only and is therefore safe under RLock.
func migrateSnapshot(state *snapshot) {
	activeByWorker := make(map[string]domain.Attempt)
	for id, attempt := range state.Attempts {
		if attempt.State.Active() {
			// Pre-ForgeGrid schema-v1 snapshots did not persist allocation
			// evidence. They can only represent unconstrained legacy runs, for
			// which the worker identity is the complete allocation.
			if attempt.Allocation == nil {
				allocation := domain.ResourceAllocation{WorkerID: attempt.WorkerID}
				attempt.Allocation = &allocation
				state.Attempts[id] = attempt
				run := state.Runs[attempt.RunID]
				if run.LastAllocation == nil {
					runAllocation := allocation
					run.LastAllocation = &runAllocation
					state.Runs[run.ID] = run
				}
			}
			activeByWorker[attempt.WorkerID] = attempt
		}
	}
	for id, worker := range state.Workers {
		attempt, active := activeByWorker[id]
		if active && worker.ActiveAttemptID == "" {
			worker.ActiveAttemptID = attempt.ID
			worker.ActiveRunID = attempt.RunID
			state.Workers[id] = worker
		}
	}
}

func validateAllocation(allocation domain.ResourceAllocation, requirements domain.ResourceRequirements, workerID string) error {
	if allocation.WorkerID != workerID || len(allocation.GPUIDs) != len(allocation.GPUs) {
		return errors.New("worker or GPU ID list is inconsistent")
	}
	if requirements.GPUCount != len(allocation.GPUs) {
		return fmt.Errorf("contains %d GPUs, want %d", len(allocation.GPUs), requirements.GPUCount)
	}
	seen := make(map[string]struct{}, len(allocation.GPUs))
	seenIndexes := make(map[int]struct{}, len(allocation.GPUs))
	for index, gpu := range allocation.GPUs {
		if allocation.GPUIDs[index] != gpu.ID {
			return errors.New("GPU ID list does not match snapshots")
		}
		if _, duplicate := seen[gpu.ID]; duplicate {
			return fmt.Errorf("GPU %q is allocated more than once", gpu.ID)
		}
		seen[gpu.ID] = struct{}{}
		if _, duplicate := seenIndexes[gpu.Index]; duplicate {
			return fmt.Errorf("GPU index %d is allocated more than once", gpu.Index)
		}
		seenIndexes[gpu.Index] = struct{}{}
		if gpu.FreeMemoryBytes < requirements.MinFreeGPUMemoryBytes {
			return fmt.Errorf("GPU %q did not satisfy the memory requirement at claim", gpu.ID)
		}
	}
	return nil
}

func initializeMaps(state *snapshot) {
	if state.Experiments == nil {
		state.Experiments = make(map[string]domain.Experiment)
	}
	if state.Runs == nil {
		state.Runs = make(map[string]domain.Run)
	}
	if state.Attempts == nil {
		state.Attempts = make(map[string]domain.Attempt)
	}
	if state.Workers == nil {
		state.Workers = make(map[string]domain.Worker)
	}
	if state.LeaseTokens == nil {
		state.LeaseTokens = make(map[string]string)
	}
	if state.Completions == nil {
		state.Completions = make(map[string]completionRecord)
	}
	if state.Idempotency == nil {
		state.Idempotency = make(map[string]idempotencyRecord)
	}
	if state.Events == nil {
		state.Events = make([]domain.Event, 0)
	}
}

func validateSnapshot(state snapshot) error {
	activeByRun := make(map[string]string)
	for id, run := range state.Runs {
		if id != run.ID {
			return fmt.Errorf("run map key %q does not match ID %q", id, run.ID)
		}
		if _, ok := state.Experiments[run.ExperimentID]; !ok {
			return fmt.Errorf("run %q refers to missing experiment", id)
		}
		if err := domain.ValidateResourceRequirements(run.ResourceRequirements); err != nil {
			return fmt.Errorf("run %q resource requirements: %w", id, err)
		}
	}
	activeByWorker := make(map[string]string)
	for id, attempt := range state.Attempts {
		if id != attempt.ID {
			return fmt.Errorf("attempt map key %q does not match ID %q", id, attempt.ID)
		}
		run, ok := state.Runs[attempt.RunID]
		if !ok {
			return fmt.Errorf("attempt %q refers to missing run", id)
		}
		if attempt.State.Active() {
			worker, exists := state.Workers[attempt.WorkerID]
			if !exists {
				return fmt.Errorf("active attempt %q refers to missing worker %q", id, attempt.WorkerID)
			}
			if attempt.Allocation == nil || attempt.Allocation.WorkerID != attempt.WorkerID {
				return fmt.Errorf("active attempt %q has inconsistent allocation worker", id)
			}
			if run.LastAllocation == nil || run.LastAllocation.WorkerID != attempt.WorkerID {
				return fmt.Errorf("active run %q has inconsistent last allocation", run.ID)
			}
			if !allocationsEqual(*run.LastAllocation, *attempt.Allocation) {
				return fmt.Errorf("active run %q and attempt %q have different allocation evidence", run.ID, id)
			}
			if err := validateAllocation(*attempt.Allocation, run.ResourceRequirements, worker.ID); err != nil {
				return fmt.Errorf("active attempt %q allocation: %w", id, err)
			}
			if run.State != domain.RunActive {
				return fmt.Errorf("active attempt %q belongs to non-active run %q", id, run.ID)
			}
			if prior := activeByRun[attempt.RunID]; prior != "" {
				return fmt.Errorf("run %q has active attempts %q and %q", attempt.RunID, prior, id)
			}
			activeByRun[attempt.RunID] = id
			if prior := activeByWorker[attempt.WorkerID]; prior != "" {
				return fmt.Errorf("worker %q has active attempts %q and %q", attempt.WorkerID, prior, id)
			}
			activeByWorker[attempt.WorkerID] = id
			if run.ActiveAttemptID != id {
				return fmt.Errorf("run %q active attempt pointer is inconsistent", run.ID)
			}
			if _, ok := state.LeaseTokens[id]; !ok {
				return fmt.Errorf("active attempt %q has no lease token hash", id)
			}
		}
	}
	for id, worker := range state.Workers {
		if id != worker.ID {
			return fmt.Errorf("worker map key %q does not match ID %q", id, worker.ID)
		}
		if err := domain.ValidateWorkerResources(worker.Resources); err != nil {
			return fmt.Errorf("worker %q resources: %w", id, err)
		}
		if worker.ActiveAttemptID != activeByWorker[id] {
			return fmt.Errorf("worker %q active attempt pointer is inconsistent", id)
		}
		if worker.ActiveAttemptID == "" && worker.ActiveRunID != "" {
			return fmt.Errorf("worker %q has run reservation without attempt", id)
		}
		if worker.ActiveAttemptID != "" {
			attempt := state.Attempts[worker.ActiveAttemptID]
			if worker.ActiveRunID != attempt.RunID {
				return fmt.Errorf("worker %q active run pointer is inconsistent", id)
			}
		}
	}
	for id, run := range state.Runs {
		if run.State == domain.RunActive {
			if run.ActiveAttemptID == "" || activeByRun[id] != run.ActiveAttemptID {
				return fmt.Errorf("active run %q has no consistent active attempt", id)
			}
		} else if run.ActiveAttemptID != "" {
			return fmt.Errorf("non-active run %q has an active attempt pointer", id)
		}
	}
	for attemptID := range state.LeaseTokens {
		attempt, ok := state.Attempts[attemptID]
		if !ok || !attempt.State.Active() {
			return fmt.Errorf("lease token exists for inactive attempt %q", attemptID)
		}
	}
	for attemptID, receipt := range state.Completions {
		attempt, ok := state.Attempts[attemptID]
		if !ok || !attempt.State.Terminal() {
			return fmt.Errorf("completion receipt exists for non-terminal attempt %q", attemptID)
		}
		if receipt.AttemptID != attemptID || receipt.RunID != attempt.RunID || receipt.Fence != attempt.Fence {
			return fmt.Errorf("completion receipt for attempt %q is inconsistent", attemptID)
		}
		if receipt.TokenHash == "" || receipt.RequestHash == "" || receipt.Result.Attempt.ID != attemptID || receipt.Result.Run.ID != attempt.RunID {
			return fmt.Errorf("completion receipt for attempt %q is incomplete", attemptID)
		}
		if _, ok := state.LeaseTokens[attemptID]; ok {
			return fmt.Errorf("attempt %q has both an active lease and completion receipt", attemptID)
		}
	}
	return nil
}

func allocationsEqual(left, right domain.ResourceAllocation) bool {
	return left.WorkerID == right.WorkerID && left.ResourcesObservedAt.Equal(right.ResourcesObservedAt) &&
		slices.Equal(left.GPUIDs, right.GPUIDs) && slices.Equal(left.GPUs, right.GPUs)
}

func (s *JSONStore) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := validateSnapshot(s.state); err != nil {
		return err
	}
	info, err := os.Stat(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("snapshot parent is not a directory")
	}
	return nil
}

func (s *JSONStore) mutate(ctx context.Context, mutation func(*snapshot) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := cloneSnapshot(s.state)
	if err != nil {
		return err
	}
	if err := mutation(&next); err != nil {
		return err
	}
	next.Revision++
	if err := validateSnapshot(next); err != nil {
		return fmt.Errorf("store invariant: %w", err)
	}
	if err := writeSnapshotAtomic(s.path, next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func cloneSnapshot(source snapshot) (snapshot, error) {
	encoded, err := json.Marshal(source)
	if err != nil {
		return snapshot{}, fmt.Errorf("clone snapshot: %w", err)
	}
	var destination snapshot
	if err := json.Unmarshal(encoded, &destination); err != nil {
		return snapshot{}, fmt.Errorf("clone snapshot: %w", err)
	}
	initializeMaps(&destination)
	return destination, nil
}

func writeSnapshotAtomic(path string, state snapshot) error {
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	encoded = append(encoded, '\n')
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".control-plane-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary snapshot: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write temporary snapshot: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary snapshot: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary snapshot: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace snapshot: %w", err)
	}
	removeTemporary = false
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

func (s *JSONStore) read(ctx context.Context, read func(snapshot) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return read(s.state)
}

func cloneValue[T any](value T) T {
	encoded, _ := json.Marshal(value)
	var cloned T
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func randomPrefixedID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(raw), nil
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func fingerprint(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func idempotencyIndex(operation, key string) string { return operation + "\x00" + key }

func checkIdempotency(state snapshot, operation, key, requestHash string) (string, bool, error) {
	if key == "" {
		return "", false, nil
	}
	record, exists := state.Idempotency[idempotencyIndex(operation, key)]
	if !exists {
		return "", false, nil
	}
	if record.RequestHash != requestHash {
		return "", false, ErrIdempotencyConflict
	}
	return record.ResourceID, true, nil
}

func rememberIdempotency(state *snapshot, operation, key, requestHash, resourceID string, now time.Time) {
	if key == "" {
		return
	}
	state.Idempotency[idempotencyIndex(operation, key)] = idempotencyRecord{
		Operation: operation, Key: key, RequestHash: requestHash, ResourceID: resourceID, CreatedAt: now,
	}
}

func appendEvent(state *snapshot, runID, attemptID, eventType string, now time.Time, data map[string]any) {
	state.NextEventSeq++
	state.Events = append(state.Events, domain.Event{
		Sequence: state.NextEventSeq, RunID: runID, AttemptID: attemptID,
		Type: eventType, Timestamp: now, Data: data,
	})
}

func copyStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func sortedExperiments(values map[string]domain.Experiment) []domain.Experiment {
	result := make([]domain.Experiment, 0, len(values))
	for _, item := range values {
		result = append(result, cloneValue(item))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

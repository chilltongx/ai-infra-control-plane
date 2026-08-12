package worker

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	ResourceProbeOK          = "ok"
	ResourceProbeUnavailable = "unavailable"
	ResourceProbeError       = "error"
)

// ResourceProbe observes the resources available to a worker. Probe failures
// are represented by ProbeStatus so a transient hardware/tooling issue does
// not stop worker heartbeats.
type ResourceProbe interface {
	Probe(context.Context) WorkerResources
}

// NVIDIAResourceProbe queries nvidia-smi directly without involving a shell.
type NVIDIAResourceProbe struct {
	executable string
	timeout    time.Duration
	now        func() time.Time
	run        func(context.Context, string, ...string) ([]byte, error)
}

func NewNVIDIAResourceProbe() *NVIDIAResourceProbe {
	return &NVIDIAResourceProbe{
		executable: "nvidia-smi",
		timeout:    5 * time.Second,
		now:        time.Now,
		run: func(ctx context.Context, executable string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, executable, args...).Output()
		},
	}
}

func (p *NVIDIAResourceProbe) Probe(ctx context.Context) WorkerResources {
	observedAt := time.Now().UTC()
	if p != nil && p.now != nil {
		observedAt = p.now().UTC()
	}
	resources := WorkerResources{GPUs: []GPUResource{}, ObservedAt: observedAt, ProbeStatus: ResourceProbeError}
	if p == nil || p.run == nil {
		return resources
	}
	executable := strings.TrimSpace(p.executable)
	if executable == "" {
		executable = "nvidia-smi"
	}
	timeout := p.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := p.run(probeCtx, executable,
		"--query-gpu=index,uuid,name,memory.total,memory.free,utilization.gpu,temperature.gpu",
		"--format=csv,noheader,nounits",
	)
	if err != nil {
		var execError *exec.Error
		if errors.Is(err, exec.ErrNotFound) || (errors.As(err, &execError) && errors.Is(execError.Err, exec.ErrNotFound)) {
			resources.ProbeStatus = ResourceProbeUnavailable
		}
		return resources
	}
	gpus, err := parseNVIDIACSV(output)
	if err != nil {
		return resources
	}
	resources.GPUs = gpus
	resources.ProbeStatus = ResourceProbeOK
	return resources
}

func parseNVIDIACSV(output []byte) ([]GPUResource, error) {
	if len(bytes.TrimSpace(output)) == 0 {
		return nil, errors.New("nvidia-smi returned empty output")
	}
	reader := csv.NewReader(bytes.NewReader(output))
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = 7
	rows, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse nvidia-smi CSV: %w", err)
	}
	gpus := make([]GPUResource, 0, len(rows))
	seenIDs := make(map[string]struct{}, len(rows))
	seenIndexes := make(map[int]struct{}, len(rows))
	for rowNumber, row := range rows {
		for index := range row {
			row[index] = strings.TrimSpace(row[index])
		}
		gpu, err := parseNVIDIARow(row)
		if err != nil {
			return nil, fmt.Errorf("parse nvidia-smi row %d: %w", rowNumber+1, err)
		}
		if _, exists := seenIDs[gpu.ID]; exists {
			return nil, fmt.Errorf("parse nvidia-smi row %d: duplicate GPU ID %q", rowNumber+1, gpu.ID)
		}
		if _, exists := seenIndexes[gpu.Index]; exists {
			return nil, fmt.Errorf("parse nvidia-smi row %d: duplicate GPU index %d", rowNumber+1, gpu.Index)
		}
		seenIDs[gpu.ID] = struct{}{}
		seenIndexes[gpu.Index] = struct{}{}
		gpus = append(gpus, gpu)
	}
	return gpus, nil
}

func parseNVIDIARow(row []string) (GPUResource, error) {
	if len(row) != 7 {
		return GPUResource{}, fmt.Errorf("expected 7 fields, got %d", len(row))
	}
	index, err := strconv.Atoi(row[0])
	if err != nil || index < 0 {
		return GPUResource{}, fmt.Errorf("invalid GPU index %q", row[0])
	}
	if row[1] == "" || row[2] == "" {
		return GPUResource{}, errors.New("GPU UUID and name are required")
	}
	totalMiB, err := parseNonNegativeInt(row[3], "total memory")
	if err != nil {
		return GPUResource{}, err
	}
	freeMiB, err := parseNonNegativeInt(row[4], "free memory")
	if err != nil {
		return GPUResource{}, err
	}
	if freeMiB > totalMiB {
		return GPUResource{}, errors.New("free GPU memory exceeds total memory")
	}
	utilization, err := parseOptionalBoundedFloat(row[5], "utilization", 0, 100)
	if err != nil {
		return GPUResource{}, err
	}
	temperature, err := parseOptionalBoundedFloat(row[6], "temperature", 0, 200)
	if err != nil {
		return GPUResource{}, err
	}
	const bytesPerMiB = int64(1024 * 1024)
	if totalMiB > math.MaxInt64/bytesPerMiB {
		return GPUResource{}, errors.New("total GPU memory overflows bytes")
	}
	return GPUResource{
		ID: row[1], Index: index, Name: row[2], Vendor: "NVIDIA",
		TotalMemoryBytes: totalMiB * bytesPerMiB, FreeMemoryBytes: freeMiB * bytesPerMiB,
		UtilizationPercent: utilization, TemperatureCelsius: temperature,
	}, nil
}

func parseNonNegativeInt(input, field string) (int64, error) {
	value, err := strconv.ParseInt(input, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid %s %q", field, input)
	}
	return value, nil
}

func parseBoundedFloat(input, field string, minimum, maximum float64) (float64, error) {
	value, err := strconv.ParseFloat(input, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("invalid %s %q", field, input)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid %s %q", field, input)
	}
	return value, nil
}

func parseOptionalBoundedFloat(input, field string, minimum, maximum float64) (float64, error) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "n/a", "[n/a]", "not supported":
		return 0, nil
	default:
		return parseBoundedFloat(input, field, minimum, maximum)
	}
}

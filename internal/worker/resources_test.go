package worker

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestNVIDIAResourceProbeParsesCSV(t *testing.T) {
	t.Parallel()
	observedAt := time.Date(2026, 8, 12, 1, 2, 3, 0, time.FixedZone("test", 8*60*60))
	probe := &NVIDIAResourceProbe{
		executable: "nvidia-smi",
		now:        func() time.Time { return observedAt },
		run: func(_ context.Context, executable string, args ...string) ([]byte, error) {
			if executable != "nvidia-smi" {
				t.Fatalf("executable = %q", executable)
			}
			wantArgs := []string{
				"--query-gpu=index,uuid,name,memory.total,memory.free,utilization.gpu,temperature.gpu",
				"--format=csv,noheader,nounits",
			}
			if !reflect.DeepEqual(args, wantArgs) {
				t.Fatalf("args = %q, want %q", args, wantArgs)
			}
			return []byte("0, GPU-aaa, NVIDIA RTX 4090, 24564, 2048, 17.5, 51\n1, GPU-bbb, NVIDIA A10, 23028, 22000, 0, 31\n"), nil
		},
	}
	got := probe.Probe(context.Background())
	if got.ProbeStatus != ResourceProbeOK || !got.ObservedAt.Equal(observedAt.UTC()) || len(got.GPUs) != 2 {
		t.Fatalf("resources = %#v", got)
	}
	if got.GPUs[0].ID != "GPU-aaa" || got.GPUs[0].Index != 0 || got.GPUs[0].Vendor != "NVIDIA" || got.GPUs[0].TotalMemoryBytes != 24564*1024*1024 || got.GPUs[0].FreeMemoryBytes != 2048*1024*1024 || got.GPUs[0].UtilizationPercent != 17.5 || got.GPUs[0].TemperatureCelsius != 51 {
		t.Fatalf("GPU = %#v", got.GPUs[0])
	}
}

func TestNVIDIAResourceProbeReportsUnavailableWhenCommandMissing(t *testing.T) {
	t.Parallel()
	probe := &NVIDIAResourceProbe{
		now: func() time.Time { return time.Unix(1, 0) },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}
		},
	}
	got := probe.Probe(context.Background())
	if got.ProbeStatus != ResourceProbeUnavailable || len(got.GPUs) != 0 {
		t.Fatalf("resources = %#v", got)
	}
}

func TestNVIDIAResourceProbeReportsErrorForBadOutput(t *testing.T) {
	t.Parallel()
	probe := &NVIDIAResourceProbe{
		now: func() time.Time { return time.Unix(1, 0) },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("not,a,valid,row\n"), nil
		},
	}
	got := probe.Probe(context.Background())
	if got.ProbeStatus != ResourceProbeError || len(got.GPUs) != 0 {
		t.Fatalf("resources = %#v", got)
	}
}

func TestNVIDIAResourceProbeReportsErrorWhenCommandFails(t *testing.T) {
	t.Parallel()
	probe := &NVIDIAResourceProbe{
		now: func() time.Time { return time.Unix(1, 0) },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("driver communication failed")
		},
	}
	got := probe.Probe(context.Background())
	if got.ProbeStatus != ResourceProbeError || len(got.GPUs) != 0 {
		t.Fatalf("resources = %#v", got)
	}
}

func TestNVIDIAResourceProbeHandlesQuotedNamesCRLFAndOptionalMetrics(t *testing.T) {
	t.Parallel()
	probe := &NVIDIAResourceProbe{
		now: func() time.Time { return time.Unix(1, 0) },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("0, GPU-a, \"NVIDIA, RTX Test\", 24576, 24000, N/A, [N/A]\r\n"), nil
		},
	}
	got := probe.Probe(context.Background())
	if got.ProbeStatus != ResourceProbeOK || len(got.GPUs) != 1 {
		t.Fatalf("resources = %#v", got)
	}
	if got.GPUs[0].Name != "NVIDIA, RTX Test" || got.GPUs[0].UtilizationPercent != 0 || got.GPUs[0].TemperatureCelsius != 0 {
		t.Fatalf("GPU = %#v", got.GPUs[0])
	}
}

func TestNVIDIAResourceProbeTimesOutWithoutCrashingWorker(t *testing.T) {
	t.Parallel()
	probe := &NVIDIAResourceProbe{
		timeout: 10 * time.Millisecond,
		now:     func() time.Time { return time.Unix(1, 0) },
		run: func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	started := time.Now()
	got := probe.Probe(context.Background())
	if got.ProbeStatus != ResourceProbeError || len(got.GPUs) != 0 {
		t.Fatalf("resources = %#v", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("probe timeout took %s", elapsed)
	}
}

package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPrepareDemoSleepUsesFixedExecutable(t *testing.T) {
	got, err := PrepareRecipe(Recipe{Adapter: AdapterDemoSleep, Command: []string{"--duration", "1.5s"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got.Program != executable || !reflect.DeepEqual(got.Args, []string{InternalSleepCommand, "1.5s"}) {
		t.Fatalf("unexpected command: %q %q", got.Program, got.Args)
	}
}

func TestPrepareRecipeDoesNotPassControlPlaneTokenToWorkload(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_TOKEN", "must-not-reach-workload")
	got, err := PrepareRecipe(Recipe{Adapter: AdapterDemoSleep, Command: []string{"1s"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range got.Env {
		if entry == "CONTROL_PLANE_API_TOKEN=must-not-reach-workload" {
			t.Fatal("control-plane API token leaked into workload environment")
		}
	}
}

func TestPrepareSGLangBuildsSafeArgvAndOwnsOutput(t *testing.T) {
	dir := t.TempDir()
	got, err := PrepareRecipe(Recipe{Adapter: AdapterSGLang, Command: []string{
		"python3", "-m", "sglang.benchmark.serving", "--base-url", "http://127.0.0.1:30000", "--dataset-name", "random", "--num-prompts", "8", "--disable-tqdm",
	}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-m", "sglang.benchmark.serving", "--base-url", "http://127.0.0.1:30000", "--dataset-name", "random", "--num-prompts", "8", "--disable-tqdm", "--output-file", filepath.Join(dir, "benchmark.jsonl")}
	if got.Program != "python3" || !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("unexpected argv:\n got: %q %q\nwant: python3 %q", got.Program, got.Args, want)
	}
}

func TestPrepareSGLangRejectsShellAndOutputOverride(t *testing.T) {
	for _, command := range [][]string{
		{"--num-prompts", "1", ";", "rm", "-rf", "/"},
		{"--output-file", "/tmp/stolen.jsonl"},
		{"--num-prompts", "$(whoami)"},
	} {
		if _, err := PrepareRecipe(Recipe{Adapter: AdapterSGLang, Command: command}, t.TempDir()); err == nil {
			t.Fatalf("expected command to be rejected: %q", command)
		}
	}
}

func TestParseSGLangMetricsUsesLastJSONLObject(t *testing.T) {
	data := []byte("progress\n{\"completed\":1}\n{\"completed\":8,\"request_throughput\":12.5,\"latency\":{\"p99\":4.25},\"backend\":\"sglang\"}\n")
	got, err := ParseSGLangMetrics(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"completed": 8, "request_throughput": 12.5, "latency.p99": 4.25}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseSGLangMetricsRejectsMissingNumericMetrics(t *testing.T) {
	if _, err := ParseSGLangMetrics([]byte(`{"backend":"sglang"}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveExpectedOutputsRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveExpectedOutputs(root, []string{outside}); err == nil {
		t.Fatal("expected absolute path to be rejected")
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveExpectedOutputs(root, []string{"link"}); err == nil {
		t.Fatal("expected escaping symlink to be rejected")
	}
}

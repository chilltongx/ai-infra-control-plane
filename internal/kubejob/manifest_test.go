package kubejob

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRenderOneShotGPUJob(t *testing.T) {
	t.Parallel()

	manifest, err := Render(Config{
		Namespace:             "experiments",
		LaunchID:              "launch/sglang:42",
		Image:                 "registry.example/ai-infra-worker:v1",
		Adapter:               "sglang.serving-benchmark",
		ControlPlaneURL:       "https://control-plane.example.internal",
		WorkerLabels:          map[string]string{"zone": "cn-a", "accelerator": "nvidia"},
		Labels:                map[string]string{"team": "ai-infra"},
		NodeSelector:          map[string]string{"accelerator": "nvidia"},
		GPUCount:              2,
		ActiveDeadlineSeconds: 1800,
		LeaseTTL:              45 * time.Second,
		ClaimWait:             15 * time.Second,
		APITokenSecret:        &SecretKeyRef{Name: "control-plane-api", Key: "token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), "plaintext-token") {
		t.Fatal("manifest contains a plaintext token")
	}

	var job map[string]any
	if err := json.Unmarshal(manifest, &job); err != nil {
		t.Fatal(err)
	}
	metadata := object(t, job, "metadata")
	jobName := stringValue(t, metadata, "name")
	if len(jobName) > 63 || !strings.HasPrefix(jobName, "aicp-launch-sglang-42-") {
		t.Fatalf("job name = %q", jobName)
	}
	labels := object(t, metadata, "labels")
	if labels["team"] != "ai-infra" || labels["app.kubernetes.io/managed-by"] != "ai-infra-control-plane" {
		t.Fatalf("labels = %#v", labels)
	}
	if got := labels["ai-infra.chilltongx.dev/launch-id"]; got != safeLabelValue("launch/sglang:42") {
		t.Fatalf("safe launch label = %q", got)
	}

	spec := object(t, job, "spec")
	if spec["backoffLimit"] != float64(0) || spec["activeDeadlineSeconds"] != float64(1800) || spec["parallelism"] != float64(1) {
		t.Fatalf("job spec = %#v", spec)
	}
	template := object(t, spec, "template")
	podSpec := object(t, template, "spec")
	if podSpec["restartPolicy"] != "Never" || podSpec["automountServiceAccountToken"] != false || podSpec["enableServiceLinks"] != false {
		t.Fatalf("pod spec = %#v", podSpec)
	}
	containers := array(t, podSpec, "containers")
	container := containers[0].(map[string]any)
	if got := stringSlice(t, container, "command"); !reflect.DeepEqual(got, []string{"/usr/local/bin/worker"}) {
		t.Fatalf("command = %#v", got)
	}
	args := stringSlice(t, container, "args")
	wantArgs := []string{
		"--one-shot",
		"--id", "k8s-" + jobName,
		"--name", "k8s-" + jobName,
		"--adapter", "sglang.serving-benchmark",
		"--artifacts", "/artifacts",
		"--lease-ttl", "45s",
		"--claim-wait", "15s",
		"--labels", "accelerator=nvidia,zone=cn-a",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}
	resources := object(t, container, "resources")
	if object(t, resources, "requests")["nvidia.com/gpu"] != "2" || object(t, resources, "limits")["nvidia.com/gpu"] != "2" {
		t.Fatalf("resources = %#v", resources)
	}
	env := array(t, container, "env")
	if len(env) != 2 {
		t.Fatalf("env = %#v", env)
	}
	controlPlaneEnv := env[0].(map[string]any)
	if controlPlaneEnv["name"] != "CONTROL_PLANE_URL" || controlPlaneEnv["value"] != "https://control-plane.example.internal" {
		t.Fatalf("control-plane env = %#v", controlPlaneEnv)
	}
	tokenEnv := env[1].(map[string]any)
	if tokenEnv["name"] != apiTokenEnv {
		t.Fatalf("token env = %#v", tokenEnv)
	}
	secretRef := object(t, object(t, tokenEnv, "valueFrom"), "secretKeyRef")
	if secretRef["name"] != "control-plane-api" || secretRef["key"] != "token" {
		t.Fatalf("secret ref = %#v", secretRef)
	}
	if _, exists := tokenEnv["value"]; exists {
		t.Fatalf("token env embeds credential value: %#v", tokenEnv)
	}
}

func TestRenderIsDeterministicAndDoesNotMutateConfig(t *testing.T) {
	t.Parallel()

	firstLabels := map[string]string{"z": "last", "a": "first"}
	firstWorkers := map[string]string{"zone": "a", "accelerator": "nvidia"}
	first := Config{
		LaunchID:              "launch-1",
		Image:                 "worker:test",
		Adapter:               "demo.sleep",
		ControlPlaneURL:       "http://control-plane:8080",
		Labels:                firstLabels,
		WorkerLabels:          firstWorkers,
		ActiveDeadlineSeconds: 60,
		LeaseTTL:              30 * time.Second,
		ClaimWait:             time.Second,
	}
	manifestA, err := Render(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstLabels) != 2 || len(firstWorkers) != 2 {
		t.Fatalf("Render mutated input maps: labels=%#v workers=%#v", firstLabels, firstWorkers)
	}

	manifestB, err := Render(Config{
		LaunchID:              "launch-1",
		Image:                 "worker:test",
		Adapter:               "demo.sleep",
		ControlPlaneURL:       "http://control-plane:8080",
		Labels:                map[string]string{"a": "first", "z": "last"},
		WorkerLabels:          map[string]string{"accelerator": "nvidia", "zone": "a"},
		ActiveDeadlineSeconds: 60,
		LeaseTTL:              30 * time.Second,
		ClaimWait:             time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(manifestA) != string(manifestB) {
		t.Fatalf("render is not deterministic:\n%s\n---\n%s", manifestA, manifestB)
	}
}

func TestRenderCPUJobOmitsGPUResources(t *testing.T) {
	t.Parallel()

	manifest, err := Render(Config{
		LaunchID:              "launch-1",
		Image:                 "worker:test",
		Adapter:               "demo.sleep",
		ControlPlaneURL:       "http://control-plane:8080",
		ActiveDeadlineSeconds: 60,
		LeaseTTL:              30 * time.Second,
		ClaimWait:             time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(manifest, &job); err != nil {
		t.Fatal(err)
	}
	container := array(t, object(t, object(t, object(t, job, "spec"), "template"), "spec"), "containers")[0].(map[string]any)
	if _, exists := container["resources"]; exists {
		t.Fatalf("CPU job has resources: %#v", container["resources"])
	}
	if got := array(t, container, "env"); len(got) != 1 {
		t.Fatalf("CPU job env = %#v", got)
	}
}

func TestRenderRejectsUnsafeOrIncompleteInput(t *testing.T) {
	t.Parallel()

	base := Config{
		LaunchID:              "launch-1",
		Image:                 "worker:test",
		Adapter:               "demo.sleep",
		ControlPlaneURL:       "http://control-plane:8080",
		ActiveDeadlineSeconds: 60,
		LeaseTTL:              30 * time.Second,
		ClaimWait:             time.Second,
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"credentials in URL", func(config *Config) { config.ControlPlaneURL = "https://token@example.test" }, "cannot contain credentials"},
		{"invalid URL", func(config *Config) { config.ControlPlaneURL = "control-plane:8080" }, "absolute HTTP(S)"},
		{"plaintext image ambiguity", func(config *Config) { config.Image = "worker:test another" }, "cannot contain whitespace"},
		{"negative GPU", func(config *Config) { config.GPUCount = -1 }, "cannot be negative"},
		{"partial API token secret", func(config *Config) { config.APITokenSecret = &SecretKeyRef{Name: "worker-secret"} }, "secret key is required"},
		{"reserved label", func(config *Config) {
			config.Labels = map[string]string{"app.kubernetes.io/managed-by": "someone-else"}
		}, "reserved"},
		{"worker label comma", func(config *Config) { config.WorkerLabels = map[string]string{"zone": "a,b"} }, "cannot be represented"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := base
			test.mutate(&config)
			_, err := Render(config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestNameForLaunchAvoidsNormalizationCollisions(t *testing.T) {
	t.Parallel()

	first, err := NameForLaunch("launch/a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NameForLaunch("launch:a")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) > 63 || len(second) > 63 {
		t.Fatalf("names = %q, %q", first, second)
	}
}

func object(t *testing.T, input map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := input[key].(map[string]any)
	if !ok {
		t.Fatalf("%q = %#v, want object", key, input[key])
	}
	return value
}

func array(t *testing.T, input map[string]any, key string) []any {
	t.Helper()
	value, ok := input[key].([]any)
	if !ok {
		t.Fatalf("%q = %#v, want array", key, input[key])
	}
	return value
}

func stringValue(t *testing.T, input map[string]any, key string) string {
	t.Helper()
	value, ok := input[key].(string)
	if !ok {
		t.Fatalf("%q = %#v, want string", key, input[key])
	}
	return value
}

func stringSlice(t *testing.T, input map[string]any, key string) []string {
	t.Helper()
	raw := array(t, input, key)
	values := make([]string, len(raw))
	for index, value := range raw {
		var ok bool
		values[index], ok = value.(string)
		if !ok {
			t.Fatalf("%q[%d] = %#v, want string", key, index, value)
		}
	}
	return values
}

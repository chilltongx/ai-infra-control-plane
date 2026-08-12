package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := run([]string{
		"render",
		"--namespace", "experiments",
		"--launch-id", "launch-1",
		"--image", "worker:test",
		"--adapter", "demo.sleep",
		"--control-plane", "http://controlplane:8080",
		"--worker-label", "zone=a",
		"--api-token-secret", "control-plane-api",
		"--active-deadline-seconds", "90",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v; stderr=%s", err, stderr.String())
	}
	var manifest map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v\n%s", err, stdout.String())
	}
	if manifest["apiVersion"] != "batch/v1" || manifest["kind"] != "Job" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if !strings.Contains(stdout.String(), `"--one-shot"`) {
		t.Fatalf("manifest does not launch a one-shot worker: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"secretKeyRef"`) || strings.Contains(stdout.String(), "Bearer ") {
		t.Fatalf("manifest does not contain a safe SecretKeyRef: %s", stdout.String())
	}
}

func TestDeleteCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := run([]string{"delete-command", "--namespace", "experiments", "--launch-id", "launch-1"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v; stderr=%s", err, stderr.String())
	}
	var command struct {
		Program string   `json:"program"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &command); err != nil {
		t.Fatal(err)
	}
	if command.Program != "kubectl" || len(command.Args) != 8 || command.Args[2] != "delete" || command.Args[6] != "--wait=true" {
		t.Fatalf("command = %#v", command)
	}
}

func TestRenderCommandRequiresIdentityAndTypedAdapter(t *testing.T) {
	t.Parallel()

	err := run([]string{"render", "--image", "worker:test"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--launch-id") {
		t.Fatalf("error = %v", err)
	}
}

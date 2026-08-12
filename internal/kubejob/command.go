package kubejob

import (
	"errors"
	"fmt"
	"strings"
)

// Command is an exec-safe process description. Args are never joined into a
// shell command, and Stdin carries a rendered manifest for kubectl apply.
type Command struct {
	Program string
	Args    []string
	Stdin   []byte
}

// ApplyCommand describes kubectl apply with a manifest on standard input.
func ApplyCommand(kubectl, namespace string, manifest []byte) (Command, error) {
	kubectl = strings.TrimSpace(kubectl)
	namespace = strings.TrimSpace(namespace)
	if kubectl == "" {
		return Command{}, errors.New("kubectl program is required")
	}
	if err := validateDNSLabel(namespace, "namespace"); err != nil {
		return Command{}, err
	}
	if len(manifest) == 0 {
		return Command{}, errors.New("manifest is required")
	}
	return Command{
		Program: kubectl,
		Args:    []string{"--namespace", namespace, "apply", "--filename", "-"},
		Stdin:   append([]byte(nil), manifest...),
	}, nil
}

// ForegroundDeleteCommand describes an idempotent, blocking Job deletion.
// Foreground cascading keeps the Job present until dependent Pods are gone.
func ForegroundDeleteCommand(kubectl, namespace, jobName string) (Command, error) {
	kubectl = strings.TrimSpace(kubectl)
	namespace = strings.TrimSpace(namespace)
	jobName = strings.TrimSpace(jobName)
	if kubectl == "" {
		return Command{}, errors.New("kubectl program is required")
	}
	if err := validateDNSLabel(namespace, "namespace"); err != nil {
		return Command{}, err
	}
	if err := validateDNSLabel(jobName, "Job name"); err != nil {
		return Command{}, fmt.Errorf("delete target: %w", err)
	}
	return Command{
		Program: kubectl,
		Args: []string{
			"--namespace", namespace,
			"delete", "job", jobName,
			"--cascade=foreground",
			"--wait=true",
			"--ignore-not-found=true",
		},
	}, nil
}

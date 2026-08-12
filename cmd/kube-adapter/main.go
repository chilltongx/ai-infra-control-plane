// kube-adapter renders dependency-free Kubernetes Job manifests and safe
// kubectl command descriptions for one-shot pull workers.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/chilltongx/ai-infra-control-plane/internal/kubejob"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "kube-adapter:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("command is required")
	}
	switch args[0] {
	case "render":
		return renderCommand(args[1:], stdout, stderr)
	case "delete-command":
		return deleteCommand(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func renderCommand(args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("kube-adapter render", flag.ContinueOnError)
	set.SetOutput(stderr)
	var (
		namespace       = set.String("namespace", envOr("KUBE_NAMESPACE", "default"), "Job namespace")
		launchID        = set.String("launch-id", "", "stable external launch ID (required)")
		workerID        = set.String("worker-id", "", "stable worker ID (derived from launch ID by default)")
		workerName      = set.String("worker-name", "", "worker display name (defaults to worker ID)")
		image           = set.String("image", "", "worker container image (required)")
		imagePullPolicy = set.String("image-pull-policy", "IfNotPresent", "Always, IfNotPresent, or Never")
		workerBinary    = set.String("worker-binary", "/usr/local/bin/worker", "worker executable in the image")
		adapter         = set.String("adapter", "", "typed worker adapter (required)")
		controlPlane    = set.String("control-plane", envOr("CONTROL_PLANE_URL", "http://controlplane:8080"), "control-plane URL reachable by the Pod")
		gpuCount        = set.Int64("gpus", 0, "GPU count requested and limited")
		gpuResource     = set.String("gpu-resource", "nvidia.com/gpu", "Kubernetes extended GPU resource name")
		deadline        = set.Int64("active-deadline-seconds", 3600, "hard Job deadline")
		leaseTTL        = set.Duration("lease-ttl", 30*time.Second, "attempt lease TTL")
		claimWait       = set.Duration("claim-wait", 20*time.Second, "one-shot claim long-poll wait")
		apiTokenSecret  = set.String("api-token-secret", "", "Secret containing CONTROL_PLANE_API_TOKEN")
		apiTokenKey     = set.String("api-token-secret-key", "token", "key in the API token Secret")
		serviceAccount  = set.String("service-account", "", "Pod service account name")
	)
	labels := stringMapFlag{}
	workerLabels := stringMapFlag{}
	nodeSelector := stringMapFlag{}
	set.Var(&labels, "label", "Kubernetes metadata label key=value (repeatable)")
	set.Var(&workerLabels, "worker-label", "worker scheduling label key=value (repeatable)")
	set.Var(&nodeSelector, "node-selector", "Pod node selector key=value (repeatable)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(set.Args(), " "))
	}
	if strings.TrimSpace(*launchID) == "" || strings.TrimSpace(*image) == "" || strings.TrimSpace(*adapter) == "" {
		return errors.New("--launch-id, --image, and --adapter are required")
	}
	var tokenSecret *kubejob.SecretKeyRef
	if strings.TrimSpace(*apiTokenSecret) != "" {
		tokenSecret = &kubejob.SecretKeyRef{Name: *apiTokenSecret, Key: *apiTokenKey}
	}
	manifest, err := kubejob.Render(kubejob.Config{
		Namespace:             *namespace,
		LaunchID:              *launchID,
		WorkerID:              *workerID,
		WorkerName:            *workerName,
		Image:                 *image,
		ImagePullPolicy:       *imagePullPolicy,
		WorkerBinary:          *workerBinary,
		Adapter:               *adapter,
		ControlPlaneURL:       *controlPlane,
		WorkerLabels:          map[string]string(workerLabels),
		Labels:                map[string]string(labels),
		NodeSelector:          map[string]string(nodeSelector),
		GPUCount:              *gpuCount,
		GPUResourceName:       *gpuResource,
		ActiveDeadlineSeconds: *deadline,
		LeaseTTL:              *leaseTTL,
		ClaimWait:             *claimWait,
		APITokenSecret:        tokenSecret,
		ServiceAccountName:    *serviceAccount,
	})
	if err != nil {
		return err
	}
	_, err = stdout.Write(manifest)
	return err
}

func deleteCommand(args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("kube-adapter delete-command", flag.ContinueOnError)
	set.SetOutput(stderr)
	namespace := set.String("namespace", envOr("KUBE_NAMESPACE", "default"), "Job namespace")
	kubectl := set.String("kubectl", "kubectl", "kubectl executable")
	launchID := set.String("launch-id", "", "launch ID whose Job should be deleted")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(set.Args(), " "))
	}
	name, err := kubejob.NameForLaunch(*launchID)
	if err != nil {
		return err
	}
	command, err := kubejob.ForegroundDeleteCommand(*kubectl, *namespace, name)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		Program string   `json:"program"`
		Args    []string `json:"args"`
	}{Program: command.Program, Args: command.Args})
}

type stringMapFlag map[string]string

func (values *stringMapFlag) String() string {
	if values == nil {
		return ""
	}
	return fmt.Sprint(map[string]string(*values))
}

func (values *stringMapFlag) Set(input string) error {
	key, value, ok := strings.Cut(input, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if !ok || key == "" || value == "" {
		return fmt.Errorf("invalid key=value %q", input)
	}
	if *values == nil {
		*values = make(map[string]string)
	}
	(*values)[key] = value
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func printUsage(output io.Writer) {
	_, _ = fmt.Fprintln(output, `Usage:
  kube-adapter render [flags]
  kube-adapter delete-command --launch-id ID [flags]

render writes a Kubernetes batch/v1 Job JSON manifest to stdout. It creates a
generic one-shot pull worker: the worker claims any eligible matching run and
may exit successfully without work. It does not bind a Job to an attempt.`)
}

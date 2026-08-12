// Package kubejob builds dependency-free Kubernetes Job manifests for
// generic one-shot pull workers. It deliberately models only the small subset
// of the Kubernetes API that this control plane owns.
package kubejob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	apiTokenEnv        = "CONTROL_PLANE_API_TOKEN"
	defaultGPUResource = "nvidia.com/gpu"
	defaultPullPolicy  = "IfNotPresent"
	defaultWorkerPath  = "/usr/local/bin/worker"
	maxJobNameLength   = 63
)

// SecretKeyRef identifies a key in a Kubernetes Secret. The credential value
// is deliberately absent so it cannot enter a manifest, label, or argv.
type SecretKeyRef struct {
	Name string
	Key  string
}

var reservedLabels = map[string]string{
	"app.kubernetes.io/name":       "ai-infra-worker",
	"app.kubernetes.io/component":  "experiment-worker",
	"app.kubernetes.io/managed-by": "ai-infra-control-plane",
}

// Config is the complete input to a deterministic one-shot worker Job.
// Durations passed to the worker remain explicit so the Job can be reproduced
// without depending on process environment outside the manifest.
type Config struct {
	Namespace             string
	LaunchID              string
	WorkerID              string
	WorkerName            string
	Image                 string
	ImagePullPolicy       string
	WorkerBinary          string
	Adapter               string
	ControlPlaneURL       string
	WorkerLabels          map[string]string
	Labels                map[string]string
	NodeSelector          map[string]string
	GPUCount              int64
	GPUResourceName       string
	ActiveDeadlineSeconds int64
	LeaseTTL              time.Duration
	ClaimWait             time.Duration
	APITokenSecret        *SecretKeyRef
	ServiceAccountName    string
}

// Render validates config and returns a stable, indented JSON manifest. JSON
// is used rather than YAML to keep the adapter dependency-free; kubectl accepts
// both formats.
func Render(config Config) ([]byte, error) {
	job, err := build(config)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Kubernetes Job: %w", err)
	}
	return append(data, '\n'), nil
}

// NameForLaunch returns a collision-resistant DNS label derived from a launch
// ID. A hash is always included because normalization can otherwise map
// distinct launch IDs to the same Job name.
func NameForLaunch(launchID string) (string, error) {
	launchID = strings.TrimSpace(launchID)
	if launchID == "" {
		return "", errors.New("launch ID is required")
	}
	slug := dnsSlug(launchID)
	if slug == "" {
		slug = "launch"
	}
	hash := shortHash(launchID, 10)
	const prefix = "aicp-"
	maxSlug := maxJobNameLength - len(prefix) - 1 - len(hash)
	if len(slug) > maxSlug {
		slug = strings.TrimRight(slug[:maxSlug], "-")
	}
	return prefix + slug + "-" + hash, nil
}

// WorkerIDForLaunch gives one Job a stable worker identity across safe
// manifest re-renders or kubectl apply retries.
func WorkerIDForLaunch(launchID string) (string, error) {
	name, err := NameForLaunch(launchID)
	if err != nil {
		return "", err
	}
	return "k8s-" + name, nil
}

func build(config Config) (jobManifest, error) {
	normalized, err := normalize(config)
	if err != nil {
		return jobManifest{}, err
	}
	jobName, _ := NameForLaunch(normalized.LaunchID)

	labels := cloneMap(normalized.Labels)
	if labels == nil {
		labels = make(map[string]string, len(reservedLabels)+2)
	}
	for key, value := range reservedLabels {
		if current, exists := labels[key]; exists && current != value {
			return jobManifest{}, fmt.Errorf("label %q is reserved and must equal %q", key, value)
		}
		labels[key] = value
	}
	labels["ai-infra.chilltongx.dev/launch-id"] = safeLabelValue(normalized.LaunchID)
	labels["ai-infra.chilltongx.dev/worker-id"] = safeLabelValue(normalized.WorkerID)
	if err := validateLabels(labels); err != nil {
		return jobManifest{}, err
	}

	args := []string{
		"--one-shot",
		"--id", normalized.WorkerID,
		"--name", normalized.WorkerName,
		"--adapter", normalized.Adapter,
		"--artifacts", "/artifacts",
		"--lease-ttl", normalized.LeaseTTL.String(),
		"--claim-wait", normalized.ClaimWait.String(),
	}
	if len(normalized.WorkerLabels) > 0 {
		args = append(args, "--labels", sortedWorkerLabels(normalized.WorkerLabels))
	}

	environment := []envVar{{Name: "CONTROL_PLANE_URL", Value: normalized.ControlPlaneURL}}
	if normalized.APITokenSecret != nil {
		environment = append(environment, envVar{
			Name: apiTokenEnv,
			ValueFrom: &envVarSource{SecretKeyRef: &secretKeySelector{
				Name: normalized.APITokenSecret.Name,
				Key:  normalized.APITokenSecret.Key,
			}},
		})
	}
	container := containerSpec{
		Name:            "worker",
		Image:           normalized.Image,
		ImagePullPolicy: normalized.ImagePullPolicy,
		Command:         []string{normalized.WorkerBinary},
		Args:            args,
		Env:             environment,
		VolumeMounts: []volumeMount{{
			Name:      "artifacts",
			MountPath: "/artifacts",
		}},
	}
	if normalized.GPUCount > 0 {
		quantity := strconv.FormatInt(normalized.GPUCount, 10)
		container.Resources = &resourceRequirements{
			Requests: map[string]string{normalized.GPUResourceName: quantity},
			Limits:   map[string]string{normalized.GPUResourceName: quantity},
		}
	}

	zero := int32(0)
	one := int32(1)
	falseValue := false
	return jobManifest{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: objectMeta{
			Name:      jobName,
			Namespace: normalized.Namespace,
			Labels:    labels,
		},
		Spec: jobSpec{
			ActiveDeadlineSeconds: normalized.ActiveDeadlineSeconds,
			BackoffLimit:          &zero,
			Completions:           &one,
			Parallelism:           &one,
			Template: podTemplateSpec{
				Metadata: objectMeta{Labels: cloneMap(labels)},
				Spec: podSpec{
					AutomountServiceAccountToken: &falseValue,
					EnableServiceLinks:           &falseValue,
					RestartPolicy:                "Never",
					ServiceAccountName:           normalized.ServiceAccountName,
					NodeSelector:                 cloneMap(normalized.NodeSelector),
					Containers:                   []containerSpec{container},
					Volumes: []volume{{
						Name:     "artifacts",
						EmptyDir: &emptyDirVolumeSource{},
					}},
				},
			},
		},
	}, nil
}

func normalize(config Config) (Config, error) {
	config.Namespace = strings.TrimSpace(config.Namespace)
	config.LaunchID = strings.TrimSpace(config.LaunchID)
	config.WorkerID = strings.TrimSpace(config.WorkerID)
	config.WorkerName = strings.TrimSpace(config.WorkerName)
	config.Image = strings.TrimSpace(config.Image)
	config.ImagePullPolicy = strings.TrimSpace(config.ImagePullPolicy)
	config.WorkerBinary = strings.TrimSpace(config.WorkerBinary)
	config.Adapter = strings.TrimSpace(config.Adapter)
	config.ControlPlaneURL = strings.TrimSpace(config.ControlPlaneURL)
	config.GPUResourceName = strings.TrimSpace(config.GPUResourceName)
	config.ServiceAccountName = strings.TrimSpace(config.ServiceAccountName)

	if config.Namespace == "" {
		config.Namespace = "default"
	}
	if err := validateDNSLabel(config.Namespace, "namespace"); err != nil {
		return Config{}, err
	}
	if config.LaunchID == "" {
		return Config{}, errors.New("launch ID is required")
	}
	if config.WorkerID == "" {
		var err error
		config.WorkerID, err = WorkerIDForLaunch(config.LaunchID)
		if err != nil {
			return Config{}, err
		}
	}
	if config.WorkerName == "" {
		config.WorkerName = config.WorkerID
	}
	if config.Image == "" || strings.IndexFunc(config.Image, unicode.IsSpace) >= 0 {
		return Config{}, errors.New("container image is required and cannot contain whitespace")
	}
	if config.ImagePullPolicy == "" {
		config.ImagePullPolicy = defaultPullPolicy
	}
	if config.ImagePullPolicy != "Always" && config.ImagePullPolicy != "IfNotPresent" && config.ImagePullPolicy != "Never" {
		return Config{}, fmt.Errorf("unsupported image pull policy %q", config.ImagePullPolicy)
	}
	if config.WorkerBinary == "" {
		config.WorkerBinary = defaultWorkerPath
	}
	if config.Adapter == "" {
		return Config{}, errors.New("worker adapter is required")
	}
	if err := validateControlPlaneURL(config.ControlPlaneURL); err != nil {
		return Config{}, err
	}
	if config.ActiveDeadlineSeconds <= 0 {
		return Config{}, errors.New("active deadline seconds must be positive")
	}
	if config.LeaseTTL <= 0 {
		return Config{}, errors.New("lease TTL must be positive")
	}
	if config.ClaimWait <= 0 {
		return Config{}, errors.New("claim wait must be positive")
	}
	if config.GPUCount < 0 {
		return Config{}, errors.New("GPU count cannot be negative")
	}
	if config.GPUCount > 0 {
		if config.GPUResourceName == "" {
			config.GPUResourceName = defaultGPUResource
		}
		if err := validateLabelKey(config.GPUResourceName); err != nil {
			return Config{}, fmt.Errorf("GPU resource name: %w", err)
		}
	}
	if err := validateWorkerLabels(config.WorkerLabels); err != nil {
		return Config{}, err
	}
	if err := validateLabels(config.Labels); err != nil {
		return Config{}, err
	}
	if err := validateLabels(config.NodeSelector); err != nil {
		return Config{}, fmt.Errorf("node selector: %w", err)
	}
	if config.ServiceAccountName != "" {
		if err := validateDNSLabel(config.ServiceAccountName, "service account name"); err != nil {
			return Config{}, err
		}
	}
	if config.APITokenSecret != nil {
		secret := *config.APITokenSecret
		secret.Name = strings.TrimSpace(secret.Name)
		secret.Key = strings.TrimSpace(secret.Key)
		if err := validateDNSSubdomain(secret.Name, "API token secret name", 253); err != nil {
			return Config{}, err
		}
		if err := validateSecretKey(secret.Key); err != nil {
			return Config{}, err
		}
		config.APITokenSecret = &secret
	}
	return config, nil
}

func validateControlPlaneURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("control-plane URL must be an absolute HTTP(S) URL: %q", raw)
	}
	if parsed.User != nil {
		return errors.New("control-plane URL cannot contain credentials")
	}
	return nil
}

func validateWorkerLabels(labels map[string]string) error {
	for key, value := range labels {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return errors.New("worker label keys and values must be non-empty")
		}
		if strings.ContainsAny(key, "=,") || strings.Contains(value, ",") {
			return fmt.Errorf("worker label %q cannot be represented by the worker --labels flag", key)
		}
	}
	return nil
}

func sortedWorkerLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

func safeLabelValue(value string) string {
	if validateLabelValue(value) == nil {
		return value
	}
	slug := labelSlug(value)
	if slug == "" {
		slug = "id"
	}
	hash := shortHash(value, 8)
	maxSlug := 63 - 1 - len(hash)
	if len(slug) > maxSlug {
		slug = strings.TrimRight(slug[:maxSlug], "-_.")
	}
	return slug + "-" + hash
}

func dnsSlug(value string) string {
	var result strings.Builder
	separator := false
	for _, char := range strings.ToLower(value) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			result.WriteRune(char)
			separator = false
		} else if result.Len() > 0 && !separator {
			result.WriteByte('-')
			separator = true
		}
	}
	return strings.Trim(result.String(), "-")
}

func labelSlug(value string) string {
	var result strings.Builder
	separator := false
	for _, char := range value {
		valid := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.'
		if valid {
			result.WriteRune(char)
			separator = false
		} else if result.Len() > 0 && !separator {
			result.WriteByte('-')
			separator = true
		}
	}
	return strings.Trim(result.String(), "-_.")
}

func shortHash(value string, length int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:length]
}

func cloneMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

type jobManifest struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   objectMeta `json:"metadata"`
	Spec       jobSpec    `json:"spec"`
}

type objectMeta struct {
	Name      string            `json:"name,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type jobSpec struct {
	ActiveDeadlineSeconds int64           `json:"activeDeadlineSeconds"`
	BackoffLimit          *int32          `json:"backoffLimit"`
	Completions           *int32          `json:"completions"`
	Parallelism           *int32          `json:"parallelism"`
	Template              podTemplateSpec `json:"template"`
}

type podTemplateSpec struct {
	Metadata objectMeta `json:"metadata"`
	Spec     podSpec    `json:"spec"`
}

type podSpec struct {
	AutomountServiceAccountToken *bool             `json:"automountServiceAccountToken"`
	EnableServiceLinks           *bool             `json:"enableServiceLinks"`
	RestartPolicy                string            `json:"restartPolicy"`
	ServiceAccountName           string            `json:"serviceAccountName,omitempty"`
	NodeSelector                 map[string]string `json:"nodeSelector,omitempty"`
	Containers                   []containerSpec   `json:"containers"`
	Volumes                      []volume          `json:"volumes"`
}

type containerSpec struct {
	Name            string                `json:"name"`
	Image           string                `json:"image"`
	ImagePullPolicy string                `json:"imagePullPolicy"`
	Command         []string              `json:"command"`
	Args            []string              `json:"args"`
	Env             []envVar              `json:"env"`
	Resources       *resourceRequirements `json:"resources,omitempty"`
	VolumeMounts    []volumeMount         `json:"volumeMounts"`
}

type envVar struct {
	Name      string        `json:"name"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *envVarSource `json:"valueFrom,omitempty"`
}

type envVarSource struct {
	SecretKeyRef *secretKeySelector `json:"secretKeyRef,omitempty"`
}

type secretKeySelector struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type resourceRequirements struct {
	Limits   map[string]string `json:"limits"`
	Requests map[string]string `json:"requests"`
}

type volumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type volume struct {
	Name     string                `json:"name"`
	EmptyDir *emptyDirVolumeSource `json:"emptyDir,omitempty"`
}

type emptyDirVolumeSource struct{}

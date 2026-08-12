package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	AdapterDemoSleep = "demo.sleep"
	AdapterSGLang    = "sglang.serving-benchmark"

	// InternalSleepCommand is an implementation detail shared with cmd/worker.
	// It keeps the typed smoke recipe dependency-free and portable instead of
	// relying on a platform-specific external `sleep` executable.
	InternalSleepCommand = "__forgegrid-internal-sleep"
)

type PreparedRecipe struct {
	Program    string
	Args       []string
	WorkingDir string
	Env        []string
	Timeout    time.Duration
	ResultFile string
}

// PrepareRecipe turns typed recipe input into an argv vector. It never invokes
// a shell and deliberately accepts only known flags for built-in recipes.
func PrepareRecipe(recipe Recipe, artifactDir string) (PreparedRecipe, error) {
	workingDir, err := cleanWorkingDir(recipe.WorkingDir)
	if err != nil {
		return PreparedRecipe{}, err
	}
	env, err := safeEnvironment(recipe.Environment)
	if err != nil {
		return PreparedRecipe{}, err
	}
	timeout := time.Duration(recipe.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	switch recipe.Adapter {
	case AdapterDemoSleep:
		duration, err := parseSleep(recipe.Command)
		if err != nil {
			return PreparedRecipe{}, err
		}
		executable, err := os.Executable()
		if err != nil {
			return PreparedRecipe{}, fmt.Errorf("resolve worker executable: %w", err)
		}
		return PreparedRecipe{
			Program:    executable,
			Args:       []string{InternalSleepCommand, duration.String()},
			WorkingDir: workingDir,
			Env:        env,
			Timeout:    timeout,
		}, nil
	case AdapterSGLang:
		resultFile := filepath.Join(artifactDir, "benchmark.jsonl")
		args, err := buildSGLangArgs(recipe.Command, resultFile)
		if err != nil {
			return PreparedRecipe{}, err
		}
		return PreparedRecipe{
			Program:    "python3",
			Args:       append([]string{"-m", "sglang.benchmark.serving"}, args...),
			WorkingDir: workingDir,
			Env:        env,
			Timeout:    timeout,
			ResultFile: resultFile,
		}, nil
	default:
		return PreparedRecipe{}, fmt.Errorf("unsupported recipe adapter %q", recipe.Adapter)
	}
}

func parseSleep(command []string) (time.Duration, error) {
	args := stripCommandPrefix(command, AdapterDemoSleep)
	if len(args) == 2 && args[0] == "--duration" {
		args = args[1:]
	}
	if len(args) != 1 {
		return 0, errors.New("demo.sleep command must be [duration] or [--duration, duration]")
	}
	duration, err := time.ParseDuration(args[0])
	if err != nil {
		seconds, floatErr := strconv.ParseFloat(args[0], 64)
		if floatErr != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return 0, fmt.Errorf("invalid sleep duration %q", args[0])
		}
		duration = time.Duration(seconds * float64(time.Second))
	}
	if duration < 0 || duration > 24*time.Hour {
		return 0, errors.New("sleep duration must be between 0 and 24h")
	}
	return duration, nil
}

var sglangFlags = map[string]flagKind{
	"--backend": stringFlag, "--base-url": stringFlag, "--host": stringFlag,
	"--port": intFlag, "--ready-check-timeout-sec": intFlag,
	"--dataset-name": stringFlag, "--dataset-path": stringFlag,
	"--model": stringFlag, "--served-model-name": stringFlag, "--tokenizer": stringFlag,
	"--num-prompts": positiveIntFlag, "--random-input-len": positiveIntFlag,
	"--random-output-len": positiveIntFlag, "--request-rate": nonNegativeFloatFlag,
	"--max-concurrency": positiveIntFlag, "--seed": intFlag,
	"--temperature": nonNegativeFloatFlag, "--top-p": nonNegativeFloatFlag,
	"--disable-tqdm": boolFlag, "--disable-stream": boolFlag,
	"--disable-ignore-eos": boolFlag, "--apply-chat-template": boolFlag,
}

type flagKind uint8

const (
	stringFlag flagKind = iota
	intFlag
	positiveIntFlag
	nonNegativeFloatFlag
	boolFlag
)

func buildSGLangArgs(command []string, resultFile string) ([]string, error) {
	input := stripSGLangPrefix(command)
	output := make([]string, 0, len(input)+2)
	seen := make(map[string]struct{})
	for i := 0; i < len(input); i++ {
		flag := input[i]
		kind, ok := sglangFlags[flag]
		if !ok {
			return nil, fmt.Errorf("unsupported sglang flag %q", flag)
		}
		if _, duplicate := seen[flag]; duplicate {
			return nil, fmt.Errorf("duplicate sglang flag %q", flag)
		}
		seen[flag] = struct{}{}
		output = append(output, flag)
		if kind == boolFlag {
			continue
		}
		i++
		if i >= len(input) || strings.HasPrefix(input[i], "--") {
			return nil, fmt.Errorf("sglang flag %q requires a value", flag)
		}
		value := input[i]
		if err := validateFlagValue(flag, value, kind); err != nil {
			return nil, err
		}
		output = append(output, value)
	}
	// The worker owns the output location; callers cannot redirect it.
	return append(output, "--output-file", resultFile), nil
}

func validateFlagValue(flag, value string, kind flagKind) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s contains NUL", flag)
	}
	switch kind {
	case stringFlag:
		if value == "" {
			return fmt.Errorf("%s cannot be empty", flag)
		}
	case intFlag, positiveIntFlag:
		number, err := strconv.Atoi(value)
		if err != nil || (kind == positiveIntFlag && number <= 0) {
			return fmt.Errorf("invalid value %q for %s", value, flag)
		}
	case nonNegativeFloatFlag:
		number, err := strconv.ParseFloat(value, 64)
		if err != nil || number < 0 || math.IsNaN(number) || math.IsInf(number, -1) {
			return fmt.Errorf("invalid value %q for %s", value, flag)
		}
	}
	return nil
}

func stripSGLangPrefix(command []string) []string {
	if len(command) >= 3 && strings.HasPrefix(filepath.Base(command[0]), "python") && command[1] == "-m" && (command[2] == "sglang.benchmark.serving" || command[2] == "sglang.bench_serving") {
		return command[3:]
	}
	return stripCommandPrefix(command, AdapterSGLang)
}

func stripCommandPrefix(command []string, adapter string) []string {
	if len(command) > 0 && command[0] == adapter {
		return command[1:]
	}
	return command
}

func cleanWorkingDir(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(input)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect working directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("working directory is not a directory")
	}
	return absolute, nil
}

func safeEnvironment(input map[string]string) ([]string, error) {
	output := sanitizedBaseEnvironment(os.Environ())
	for key, value := range input {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid environment variable %q", key)
		}
		output = append(output, key+"="+value)
	}
	return output, nil
}

func sanitizedBaseEnvironment(input []string) []string {
	output := make([]string, 0, len(input))
	for _, entry := range input {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "CONTROL_PLANE_API_TOKEN" {
			continue
		}
		output = append(output, entry)
	}
	return output
}

// ParseSGLangMetrics reads the last valid object from the benchmark JSONL and
// flattens numeric scalars into the control-plane metric map.
func ParseSGLangMetrics(data []byte) (map[string]float64, error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var value map[string]any
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			continue
		}
		metrics := make(map[string]float64)
		flattenNumbers("", value, metrics)
		if len(metrics) == 0 {
			return nil, errors.New("benchmark result contains no numeric metrics")
		}
		return metrics, nil
	}
	return nil, errors.New("benchmark result contains no valid JSON object")
}

func flattenNumbers(prefix string, value map[string]any, output map[string]float64) {
	for key, raw := range value {
		name := key
		if prefix != "" {
			name = prefix + "." + key
		}
		switch typed := raw.(type) {
		case json.Number:
			if number, err := typed.Float64(); err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
				output[name] = number
			}
		case map[string]any:
			flattenNumbers(name, typed, output)
		}
	}
}

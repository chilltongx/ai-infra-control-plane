package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	got, err := parseConfig([]string{"-listen", "127.0.0.1:9000", "-data", "/tmp/control-plane-test.json", "-lease-sweep", "5s"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got.address != "127.0.0.1:9000" || got.dataPath != "/tmp/control-plane-test.json" || got.leaseSweep != 5*time.Second || got.workerStaleAfter != 30*time.Second {
		t.Fatalf("config = %+v", got)
	}
}

func TestParseConfigRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	if _, err := parseConfig([]string{"unexpected"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseConfigRequiresTokenForNonLoopbackListen(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_TOKEN", "")
	_, err := parseConfig([]string{"-listen", "0.0.0.0:8080", "-data", "/tmp/control-plane-test.json"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "CONTROL_PLANE_API_TOKEN") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseConfigAcceptsNonLoopbackWithEnvironmentToken(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_TOKEN", "secret")
	got, err := parseConfig([]string{"-listen", "[::]:8080", "-data", "/tmp/control-plane-test.json"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if got.apiToken != "secret" {
		t.Fatal("API token was not loaded from the environment")
	}
}

func TestIsLoopbackListenAddress(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		if got, err := isLoopbackListenAddress(address); err != nil || !got {
			t.Fatalf("%s: loopback=%v error=%v", address, got, err)
		}
	}
	if got, err := isLoopbackListenAddress("0.0.0.0:8080"); err != nil || got {
		t.Fatalf("wildcard: loopback=%v error=%v", got, err)
	}
}

func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	if got, err := parseLogLevel("warning"); err != nil || got != slog.LevelWarn {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := parseLogLevel("trace"); err == nil {
		t.Fatal("expected error")
	}
}

func TestWriteListenAddressPublishesAtomically(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "runtime", "listen-address")
	if err := writeListenAddress(path, "127.0.0.1:54321"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "127.0.0.1:54321" {
		t.Fatalf("address = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o", info.Mode().Perm())
	}
}

func TestRunPublishesEphemeralAddressAndRemovesItOnShutdown(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "control-plane.json")
	addressPath := filepath.Join(t.TempDir(), "runtime", "listen-address")
	done := make(chan error, 1)
	process, err := os.StartProcess(os.Args[0], []string{os.Args[0], "-test.run=TestControlPlaneHelperProcess", "--", "-listen", "127.0.0.1:0", "-data", dataPath, "-listen-address-file", addressPath}, &os.ProcAttr{
		Env:   append(os.Environ(), "GO_WANT_CONTROL_PLANE_HELPER=1"),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		state, waitErr := process.Wait()
		if waitErr != nil {
			done <- waitErr
			return
		}
		if !state.Success() {
			done <- &os.PathError{Op: "controlplane helper", Path: os.Args[0], Err: os.ErrInvalid}
			return
		}
		done <- nil
	}()
	defer func() { _ = process.Kill() }()

	deadline := time.Now().Add(5 * time.Second)
	var address string
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(addressPath)
		if readErr == nil {
			address = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if address == "" {
		t.Fatal("listen address file was not published")
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+address+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ready status = %d", response.StatusCode)
	}
	if err := process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control plane did not stop")
	}
	if _, err := os.Stat(addressPath); !os.IsNotExist(err) {
		t.Fatalf("listen address file still exists: %v", err)
	}
}

func TestControlPlaneHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CONTROL_PLANE_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		os.Exit(2)
	}
	if err := run(os.Args[separator+1:], os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

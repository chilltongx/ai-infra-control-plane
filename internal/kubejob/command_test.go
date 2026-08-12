package kubejob

import (
	"reflect"
	"testing"
)

func TestApplyCommandUsesManifestStdinAndSeparateArgv(t *testing.T) {
	t.Parallel()

	manifest := []byte("{\"kind\":\"Job\"}\n")
	command, err := ApplyCommand("/opt/bin/kubectl", "experiments", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if command.Program != "/opt/bin/kubectl" {
		t.Fatalf("program = %q", command.Program)
	}
	wantArgs := []string{"--namespace", "experiments", "apply", "--filename", "-"}
	if !reflect.DeepEqual(command.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", command.Args, wantArgs)
	}
	if string(command.Stdin) != string(manifest) {
		t.Fatalf("stdin = %q", command.Stdin)
	}
	manifest[0] = 'X'
	if command.Stdin[0] == 'X' {
		t.Fatal("command retains caller-owned stdin buffer")
	}
}

func TestForegroundDeleteCommandIsBlockingAndIdempotent(t *testing.T) {
	t.Parallel()

	command, err := ForegroundDeleteCommand("kubectl", "experiments", "aicp-launch-1-1234567890")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"--namespace", "experiments",
		"delete", "job", "aicp-launch-1-1234567890",
		"--cascade=foreground",
		"--wait=true",
		"--ignore-not-found=true",
	}
	if command.Program != "kubectl" || !reflect.DeepEqual(command.Args, wantArgs) || len(command.Stdin) != 0 {
		t.Fatalf("command = %#v, want args %#v", command, wantArgs)
	}
}

func TestCommandsRejectUnresolvedTargets(t *testing.T) {
	t.Parallel()

	if _, err := ApplyCommand("kubectl", "", []byte("{}")); err == nil {
		t.Fatal("ApplyCommand accepted an empty namespace")
	}
	if _, err := ForegroundDeleteCommand("kubectl", "default", "../../all-jobs"); err == nil {
		t.Fatal("ForegroundDeleteCommand accepted an unsafe Job name")
	}
}

package main

import (
	"testing"

	"github.com/chilltongx/ai-infra-control-plane/internal/worker"
)

func TestRunInternalPortableSleep(t *testing.T) {
	handled, err := runInternal([]string{worker.InternalSleepCommand, "0s"})
	if err != nil || !handled {
		t.Fatalf("runInternal() = handled %v, error %v", handled, err)
	}
}

func TestRunInternalRejectsInvalidInvocation(t *testing.T) {
	if handled, err := runInternal([]string{"--version"}); handled || err != nil {
		t.Fatalf("unrelated command = handled %v, error %v", handled, err)
	}
	if handled, err := runInternal([]string{worker.InternalSleepCommand, "forever"}); !handled || err == nil {
		t.Fatalf("invalid command = handled %v, error %v", handled, err)
	}
}

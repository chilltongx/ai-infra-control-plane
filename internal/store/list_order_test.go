package store

import (
	"context"
	"testing"
	"time"

	"github.com/chilltongx/ai-infra-control-plane/internal/domain"
)

func TestListsReturnNewestResourcesFirst(t *testing.T) {
	repository, clock, _ := newTestStore(t)
	ctx := context.Background()

	firstExperiment, err := repository.CreateExperiment(ctx, CreateExperimentParams{Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	secondExperiment, err := repository.CreateExperiment(ctx, CreateExperimentParams{Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	experiments, err := repository.ListExperiments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(experiments) != 2 || experiments[0].ID != secondExperiment.Experiment.ID || experiments[1].ID != firstExperiment.Experiment.ID {
		t.Fatalf("experiment order = %#v", experiments)
	}

	firstRun, err := repository.CreateRun(ctx, CreateRunParams{
		ExperimentID: firstExperiment.Experiment.ID,
		Recipe:       domain.Recipe{Adapter: "demo.sleep", Command: []string{"1ms"}},
		MaxAttempts:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	secondRun, err := repository.CreateRun(ctx, CreateRunParams{
		ExperimentID: firstExperiment.Experiment.ID,
		Recipe:       domain.Recipe{Adapter: "demo.sleep", Command: []string{"1ms"}},
		MaxAttempts:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := repository.ListRuns(ctx, firstExperiment.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != secondRun.Run.ID || runs[1].ID != firstRun.Run.ID {
		t.Fatalf("run order = %#v", runs)
	}

	if _, err := repository.RegisterWorker(ctx, RegisterWorkerParams{ID: "worker-old", Name: "old", Adapter: "demo.sleep"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if _, err := repository.RegisterWorker(ctx, RegisterWorkerParams{ID: "worker-new", Name: "new", Adapter: "demo.sleep"}); err != nil {
		t.Fatal(err)
	}
	workers, err := repository.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 2 || workers[0].ID != "worker-new" || workers[1].ID != "worker-old" {
		t.Fatalf("worker order = %#v", workers)
	}
}

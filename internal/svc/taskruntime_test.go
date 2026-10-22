package svc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/task"
)

func TestTaskRuntimeReconcilesBeforeAcceptingAndPersistsSubmission(t *testing.T) {
	dir := t.TempDir()
	store, err := task.Open(task.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	started := now.Add(-time.Minute)
	record := task.Record{
		SchemaVersion: task.SchemaVersion, TaskID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e63f",
		Type: "container-update", Status: task.StatusRunning, CreatedAt: now.Add(-2 * time.Minute),
		UpdatedAt: now.Add(-time.Minute), StartedAt: &started,
	}
	if err := store.Create(record); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	runtime, err := NewTaskRuntime(context.Background(), TaskRuntimeOptions{
		Directory: dir, MaxConcurrent: 2, QueueCapacity: 100, TaskTimeout: time.Minute,
		RecoveryTimeout: time.Second, Retention: 24 * time.Hour, MaxRecords: 10,
	}, nil)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer runtime.Close()

	interrupted, err := runtime.Query.GetProgress(record.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != task.StatusInterrupted || !interrupted.IsDone {
		t.Fatalf("startup record was not reconciled before runner startup: %#v", interrupted)
	}

	id, err := runtime.Runner.Submit(task.TaskSpec{
		Record: task.Record{Type: "test-operation", Message: "accepted"},
		Run: func(ctx context.Context, execution *task.Execution) error {
			if err := execution.Progress("working", 50, "working"); err != nil {
				return err
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}
	if err := runtime.Runner.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, err := runtime.Query.GetProgress(id)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != task.StatusSucceeded || !completed.IsDone || completed.Stage != "working" {
		t.Fatalf("submitted task was not durably completed: %#v", completed)
	}
}

func TestTaskRuntimeCloseIsSafeForConcurrentCallers(t *testing.T) {
	runtime, err := NewTaskRuntime(context.Background(), TaskRuntimeOptions{
		Directory: t.TempDir(), MaxConcurrent: 1, QueueCapacity: 1,
		TaskTimeout: time.Minute, RecoveryTimeout: time.Second,
		Retention: 24 * time.Hour, MaxRecords: 10,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runtime.Close(); err != nil {
				t.Errorf("concurrent close: %v", err)
			}
		}()
	}
	wg.Wait()
}

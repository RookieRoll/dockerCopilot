package svc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
)

type fixedPendingValidator struct{ proof task.PendingInstallProof }

func (v fixedPendingValidator) VerifyPendingInstall(context.Context, task.Record) (task.PendingInstallProof, error) {
	return v.proof, nil
}

type failedPendingValidator struct{}

func (failedPendingValidator) VerifyPendingInstall(context.Context, task.Record) (task.PendingInstallProof, error) {
	return task.PendingInstallProof{}, errors.New("running program does not match the pending handoff")
}

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

func TestConfirmPendingInstallsWritesReadyBeforeDurableSuccess(t *testing.T) {
	workDir := t.TempDir()
	store, err := task.Open(task.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	taskID := "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e63f"
	marker := &task.PendingInstallHandoff{
		HandoffID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e640", TargetVersion: "2.3.4",
		GOARCH: "amd64", ArtifactSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PreparedAt: time.Now().UTC()}
	record := task.Record{
		SchemaVersion: task.SchemaVersion, TaskID: taskID, Type: task.TaskTypeProgramUpdate,
		Status: task.StatusRunning, CreatedAt: time.Now().UTC(), PendingInstall: marker,
	}
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	runtime := &TaskRuntime{
		Store: store, handoffWorkDir: workDir, pendingInstallTaskIDs: []string{taskID},
		pendingValidator: fixedPendingValidator{proof: task.PendingInstallProof{
			Verified: true, TaskID: taskID, HandoffID: marker.HandoffID, TargetVersion: marker.TargetVersion,
			GOARCH: marker.GOARCH, ArtifactSHA256: marker.ArtifactSHA256,
		}},
	}
	if err := runtime.ConfirmPendingInstalls(context.Background()); err != nil {
		t.Fatalf("confirm pending install: %v", err)
	}
	confirmed, err := store.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != task.StatusSucceeded || confirmed.Stage != "install-confirmed" {
		t.Fatalf("task was not confirmed after readiness: %#v", confirmed)
	}
	result, err := updater.ReadHandoffResult(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "ready" || result.HandoffID != marker.HandoffID {
		t.Fatalf("unexpected ready sidecar: %#v", result)
	}
	if runtime.HasPendingInstall() {
		t.Fatal("pending install IDs were not cleared after durable confirmation")
	}
}

func TestStartupRejectsUnverifiedCandidateAndSignalsLauncherRollback(t *testing.T) {
	workDir, storeDir := t.TempDir(), t.TempDir()
	taskID := "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e63f"
	marker := task.PendingInstallHandoff{
		HandoffID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e640", TargetVersion: "2.3.4",
		GOARCH: "amd64", ArtifactSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PreparedAt: time.Now().UTC(),
	}
	store, err := task.Open(task.Options{Dir: storeDir})
	if err != nil {
		t.Fatal(err)
	}
	record := task.Record{SchemaVersion: task.SchemaVersion, TaskID: taskID, Type: task.TaskTypeProgramUpdate, Status: task.StatusRunning, CreatedAt: time.Now().UTC(), PendingInstall: &marker}
	if err := store.Create(record); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	metadata := updater.HandoffMetadata{TaskID: taskID, HandoffID: marker.HandoffID, TargetVersion: marker.TargetVersion, GOARCH: marker.GOARCH, BinarySHA256: marker.ArtifactSHA256}
	if err := updater.WriteHandoffMetadata(workDir, metadata); err != nil {
		t.Fatal(err)
	}
	_, err = NewTaskRuntime(context.Background(), TaskRuntimeOptions{
		Directory: storeDir, MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: time.Minute,
		RecoveryTimeout: time.Second, Retention: 24 * time.Hour, MaxRecords: 10,
		PendingValidator: failedPendingValidator{}, HandoffWorkDir: workDir,
	}, nil)
	if err == nil {
		t.Fatal("unverified candidate startup unexpectedly succeeded")
	}
	result, err := updater.ReadHandoffResult(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "failed-start" || result.ErrorCode != "running_binary_unverified" || result.HandoffID != marker.HandoffID {
		t.Fatalf("launcher was not told to roll back this handoff: %#v", result)
	}
	store, err = task.Open(task.Options{Dir: storeDir})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	interrupted, err := store.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != task.StatusInterrupted || !interrupted.RequiresAttention {
		t.Fatalf("failed candidate startup did not retain manual-review state: %#v", interrupted)
	}
}

func TestStartupAllowsOldBinaryAfterMatchingLauncherFailure(t *testing.T) {
	workDir, storeDir := t.TempDir(), t.TempDir()
	taskID := "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e63f"
	marker := task.PendingInstallHandoff{
		HandoffID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e640", TargetVersion: "2.3.4",
		GOARCH: "amd64", ArtifactSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PreparedAt: time.Now().UTC(),
	}
	store, err := task.Open(task.Options{Dir: storeDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(task.Record{SchemaVersion: task.SchemaVersion, TaskID: taskID, Type: task.TaskTypeProgramUpdate, Status: task.StatusRunning, CreatedAt: time.Now().UTC(), PendingInstall: &marker}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	metadata := updater.HandoffMetadata{TaskID: taskID, HandoffID: marker.HandoffID, TargetVersion: marker.TargetVersion, GOARCH: marker.GOARCH, BinarySHA256: marker.ArtifactSHA256}
	if err := updater.WriteHandoffMetadata(workDir, metadata); err != nil {
		t.Fatal(err)
	}
	if err := updater.WriteHandoffResult(workDir, updater.HandoffResult{HandoffMetadata: metadata, Result: "failed-start", ErrorCode: "candidate_exit"}); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewTaskRuntime(context.Background(), TaskRuntimeOptions{
		Directory: storeDir, MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: time.Minute,
		RecoveryTimeout: time.Second, Retention: 24 * time.Hour, MaxRecords: 10,
		PendingValidator: failedPendingValidator{}, HandoffWorkDir: workDir,
	}, nil)
	if err != nil {
		t.Fatalf("old binary should serve after the matching failed-start result: %v", err)
	}
	defer runtime.Close()
	if runtime.HasPendingInstall() {
		t.Fatal("failed candidate should not be retained as a pending install")
	}
	interrupted, err := runtime.Store.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != task.StatusInterrupted || !interrupted.RequiresAttention {
		t.Fatalf("old binary fallback lost manual-review facts: %#v", interrupted)
	}
}

package svc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
)

type handoffVerifierFunc func(context.Context, updater.HandoffMetadata) error

func (f handoffVerifierFunc) VerifyProgramInstall(ctx context.Context, metadata updater.HandoffMetadata) error {
	return f(ctx, metadata)
}

func TestTaskRuntimeIgnoresLegacyTaskDirectoryAndStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "not-a-task.json")
	contents := []byte("private legacy task data")
	if err := os.WriteFile(corrupt, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewTaskRuntime(context.Background(), TaskRuntimeOptions{Directory: dir, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if records, err := runtime.Store.List(); err != nil || len(records) != 0 {
		t.Fatalf("new runtime did not start empty: records=%#v err=%v", records, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(corrupt)
	if err != nil || string(after) != string(contents) {
		t.Fatalf("legacy task directory was touched: data=%q err=%v", after, err)
	}
	runtime, err = NewTaskRuntime(context.Background(), TaskRuntimeOptions{Directory: dir, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if records, err := runtime.Store.List(); err != nil || len(records) != 0 {
		t.Fatalf("new process runtime loaded old task history: records=%#v err=%v", records, err)
	}
}

func TestTaskRuntimeCloseIsSafeForConcurrentCallers(t *testing.T) {
	runtime, err := NewTaskRuntime(context.Background(), TaskRuntimeOptions{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runtime.Close(); err != nil {
				t.Errorf("close runtime: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, err := runtime.Store.List(); err == nil {
		t.Fatal("closed runtime store accepted a read")
	}
}

func TestConfirmPendingProgramHandoffWritesReadyThenRecreatesInMemorySuccess(t *testing.T) {
	workDir := t.TempDir()
	metadata := validProgramHandoff()
	if err := updater.WriteHandoffMetadata(workDir, metadata); err != nil {
		t.Fatal(err)
	}
	var verifies atomic.Int32
	runtime, err := NewTaskRuntimeForConfigWithValidator(context.Background(), config.Config{}, handoffVerifierFunc(func(_ context.Context, got updater.HandoffMetadata) error {
		verifies.Add(1)
		if !sameHandoff(got, metadata) {
			t.Errorf("validator received wrong identity: %#v", got)
		}
		return nil
	}), workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if !runtime.HasPendingInstall() {
		t.Fatal("startup did not detect the handoff sidecar")
	}
	if err := runtime.ConfirmPendingInstalls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if verifies.Load() != 1 {
		t.Fatalf("validator calls=%d, want 1", verifies.Load())
	}
	result, err := updater.ReadHandoffResult(workDir)
	if err != nil || result.Result != "ready" || !sameHandoff(result.HandoffMetadata, metadata) {
		t.Fatalf("ready result mismatch: %#v err=%v", result, err)
	}
	record, err := runtime.Store.Get(metadata.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != task.StatusSucceeded || record.RequestID != metadata.TaskID || record.ImageRef != metadata.TargetVersion || !record.IsDone {
		t.Fatalf("recreated task is not terminal success: %#v", record)
	}
	if runtime.HasPendingInstall() || runtime.HasUnresolvedProgramInstall() {
		t.Fatal("confirmed handoff remains unresolved")
	}
}

func TestFailedStartHandoffRecreatesFailureWithoutVerifyingOldBinary(t *testing.T) {
	workDir := t.TempDir()
	metadata := validProgramHandoff()
	if err := updater.WriteHandoffMetadata(workDir, metadata); err != nil {
		t.Fatal(err)
	}
	if err := updater.WriteHandoffResult(workDir, updater.HandoffResult{HandoffMetadata: metadata, Result: "failed-start", ErrorCode: "candidate_exit"}); err != nil {
		t.Fatal(err)
	}
	var verifies atomic.Int32
	runtime, err := NewTaskRuntimeForConfigWithValidator(context.Background(), config.Config{}, handoffVerifierFunc(func(context.Context, updater.HandoffMetadata) error {
		verifies.Add(1)
		return errors.New("old binary must not be checked against the candidate digest")
	}), workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.ConfirmPendingInstalls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if verifies.Load() != 0 {
		t.Fatalf("failed-start was incorrectly verified as candidate: calls=%d", verifies.Load())
	}
	record, err := runtime.Store.Get(metadata.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != task.StatusFailed || record.Error == "" || record.Recovery.Status != "restored" {
		t.Fatalf("launcher rollback was not reconstructed as failed: %#v", record)
	}
	if runtime.HasPendingInstall() || runtime.HasUnresolvedProgramInstall() {
		t.Fatal("verified launcher rollback remains unresolved")
	}
}

func TestProgramHandoffVerificationFailureRetainsPendingMarker(t *testing.T) {
	workDir := t.TempDir()
	metadata := validProgramHandoff()
	if err := updater.WriteHandoffMetadata(workDir, metadata); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewTaskRuntimeForConfigWithValidator(context.Background(), config.Config{}, handoffVerifierFunc(func(context.Context, updater.HandoffMetadata) error {
		return errors.New("running binary digest mismatch")
	}), workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.ConfirmPendingInstalls(context.Background()); err == nil {
		t.Fatal("expected binary verification failure")
	}
	if !runtime.HasPendingInstall() || !runtime.HasUnresolvedProgramInstall() {
		t.Fatal("failed verification discarded the handoff marker")
	}
	if _, err := updater.ReadHandoffResult(workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification failure wrote a ready result: %v", err)
	}
}

func TestMalformedProgramHandoffBlocksMutationConfirmation(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".dockerCopilot-handoff"), []byte("schema=broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewTaskRuntimeForConfigWithValidator(context.Background(), config.Config{}, nil, workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if !runtime.HasPendingInstall() || !runtime.HasUnresolvedProgramInstall() {
		t.Fatal("malformed handoff sidecar did not fail closed")
	}
	if err := runtime.ConfirmPendingInstalls(context.Background()); err == nil {
		t.Fatal("malformed handoff unexpectedly confirmed")
	}
}

func validProgramHandoff() updater.HandoffMetadata {
	return updater.HandoffMetadata{
		TaskID: "123e4567-e89b-12d3-a456-426614174000", HandoffID: "123e4567-e89b-12d3-a456-426614174001",
		TargetVersion: "2.3.4", GOARCH: "amd64", BinarySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
}

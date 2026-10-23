package version

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"debug/elf"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
)

func TestRollbackFailureWritesMatchingFailedStartToPreventRetry(t *testing.T) {
	workDir := t.TempDir()
	candidate := filepath.Join(workDir, "dockerCopilot-new")
	if err := os.WriteFile(candidate, []byte("candidate binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := &programTaskState{
		workDir:     workDir,
		stagingRoot: filepath.Join(t.TempDir(), "stage"),
		placed: &updater.InstallHandoff{
			TaskID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e63f", TargetVersion: "2.3.4", GOARCH: "amd64",
			BinarySHA256: strings.Repeat("a", 64), Path: candidate,
		},
		metadata: &updater.HandoffMetadata{
			TaskID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e63f", HandoffID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e640",
			TargetVersion: "2.3.4", GOARCH: "amd64", BinarySHA256: strings.Repeat("a", 64),
		},
	}
	if err := updater.WriteHandoffMetadata(workDir, *state.metadata); err != nil {
		t.Fatal(err)
	}
	if err := state.rollback(); err == nil {
		t.Fatal("expected candidate cleanup to fail without its original file identity")
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("changed candidate should be preserved for manual review: %v", err)
	}
	result, err := updater.ReadHandoffResult(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "failed-start" || result.ErrorCode != "rollback_failed" || result.TaskID != state.metadata.TaskID || result.HandoffID != state.metadata.HandoffID {
		t.Fatalf("rollback failure did not disable a future candidate retry: %#v", result)
	}
}

func TestProgramUpdateTaskDrainsAndRollsBackWhenHandoffSignalFails(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("integration fixture emits an amd64 ELF archive")
	}
	oldVersion := config.Version
	config.Version = "2.2.0"
	t.Cleanup(func() { config.Version = oldVersion })
	archive := programUpdateArchive(t)
	archiveDigest := sha256.Sum256(archive)
	checksum := fmt.Sprintf("%x  dockerCopilot-amd64.tar.gz\n", archiveDigest)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest/version"):
			_, _ = w.Write([]byte("2.3.4\n"))
		case strings.HasSuffix(r.URL.Path, ".tar.gz.sha256"):
			_, _ = w.Write([]byte(checksum))
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	workDir := t.TempDir()
	tasksDir := t.TempDir()
	runtimeTasks, err := svc.NewTaskRuntime(context.Background(), svc.TaskRuntimeOptions{
		Directory: tasksDir, MaxConcurrent: 1, QueueCapacity: 2, TaskTimeout: time.Minute,
		RecoveryTimeout: time.Second, Retention: 24 * time.Hour, MaxRecords: 10,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeTasks.Close()
	shutdown := make(chan struct{}, 1)
	shutdown <- struct{}{} // force the final handoff notification to fail
	cfg := config.Config{}
	cfg.Tasks.Directory = tasksDir
	cfg.Updates.Repository = "org/project"
	cfg.Updates.GitHubProxy = server.URL
	cfg.Updates.WorkDir = workDir
	cfg.Tasks.Directory = tasksDir
	serviceContext := &svc.ServiceContext{
		Config: cfg, TaskRuntime: runtimeTasks, TaskRunner: runtimeTasks.Runner,
		TaskStore: runtimeTasks.Store, TaskQuery: runtimeTasks.Query,
		Mutations: operation.NewMutationGate(), ProgramShutdown: shutdown,
	}
	logic := &UpdateProgramLogic{
		ctx: context.Background(), serviceContext: serviceContext,
		updateSource: cfg.UpdateSource(), validateProgramHandoff: func(context.Context) error { return nil },
	}
	response, err := logic.UpdateProgram()
	if err != nil {
		t.Fatalf("submit program update: %v", err)
	}
	taskID := response.Data.(map[string]any)["taskID"].(string)
	if err := runtimeTasks.Runner.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, err := runtimeTasks.Store.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != task.StatusFailed || completed.Recovery.Status != "restored" {
		t.Fatalf("failed handoff should be rolled back and finalized: %#v", completed)
	}
	if serviceContext.MutationGate().IsDraining() {
		t.Fatal("failed handoff did not reopen mutation admission")
	}
	if _, err := os.Stat(filepath.Join(workDir, "dockerCopilot-new")); !os.IsNotExist(err) {
		t.Fatalf("failed handoff left a runnable candidate: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".dockerCopilot-handoff")); err != nil {
		t.Fatalf("handoff audit metadata should remain: %v", err)
	}
}

func TestProgramUpdateDrainTimeoutRestoresMutationAdmission(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("integration fixture emits an amd64 ELF archive")
	}
	oldVersion := config.Version
	config.Version = "2.2.0"
	t.Cleanup(func() { config.Version = oldVersion })
	server := newProgramUpdateTestServer(t, programUpdateArchive(t), nil)
	defer server.Close()
	ctx, runtimeTasks, logic, shutdown := newProgramUpdateHarness(t, server.URL, time.Second)
	blocked, err := ctx.MutationGate().Acquire(context.Background(), "held-mutation")
	if err != nil {
		t.Fatal(err)
	}
	response, err := logic.UpdateProgram()
	if err != nil {
		blocked.Release()
		t.Fatalf("submit update: %v", err)
	}
	taskID := response.Data.(map[string]any)["taskID"].(string)
	deadline := time.Now().Add(3 * time.Second)
	for !ctx.MutationGate().IsDraining() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !ctx.MutationGate().IsDraining() {
		blocked.Release()
		t.Fatal("program task did not begin draining")
	}
	if err := runtimeTasks.Runner.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	blocked.Release()
	record, err := runtimeTasks.Store.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != task.StatusFailed || record.Recovery.Status != "restored" {
		t.Fatalf("drain timeout should fail before handoff: %#v", record)
	}
	if ctx.MutationGate().IsDraining() {
		t.Fatal("drain timeout left mutation admission closed")
	}
	if len(shutdown) != 0 {
		t.Fatal("drain timeout signalled program shutdown")
	}
}

func TestProgramUpdateSuccessfulHandoffWaitsForNewProcessProof(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("integration fixture emits an amd64 ELF archive")
	}
	oldVersion := config.Version
	config.Version = "2.2.0"
	t.Cleanup(func() { config.Version = oldVersion })
	server := newProgramUpdateTestServer(t, programUpdateArchive(t), nil)
	defer server.Close()
	ctx, runtimeTasks, logic, shutdown := newProgramUpdateHarness(t, server.URL, time.Minute)
	requestID := "123e4567-e89b-12d3-a456-426614174000"
	logic.SetRequestID(requestID)
	response, err := logic.UpdateProgram()
	if err != nil {
		t.Fatalf("submit update: %v", err)
	}
	taskID := response.Data.(map[string]any)["taskID"].(string)
	shutdownRequested := false
	select {
	case <-shutdown:
		shutdownRequested = true
	case <-time.After(5 * time.Second):
		t.Fatal("program task did not request controlled shutdown")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		record, getErr := runtimeTasks.Store.Get(taskID)
		if getErr == nil && record.Status == task.StatusRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, err := runtimeTasks.Store.Get(taskID)
	if err != nil || record.Status != task.StatusRunning || record.FinishedAt != nil {
		t.Fatalf("old process must keep handoff running until shutdown: record=%#v err=%v", record, err)
	}
	if record.RequestID != requestID {
		t.Fatalf("task request ID did not preserve the client UUID: got %q", record.RequestID)
	}
	if !ctx.MutationGate().IsDraining() {
		t.Fatal("successful handoff reopened mutation admission")
	}
	if !runtimeTasks.HasPendingInstall() {
		t.Fatal("task runtime did not retain the handoff for the restarted process")
	}
	if !shutdownRequested {
		t.Fatal("successful handoff did not signal controlled shutdown")
	}
	if err := runtimeTasks.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ctx.Config.Updates.WorkDir, "dockerCopilot-new")); err != nil {
		t.Fatalf("handoff cleanup removed the candidate after shutdown: %v", err)
	}
}

func TestProgramUpdateRejectsConcurrentSubmission(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("integration fixture emits an amd64 ELF archive")
	}
	oldVersion := config.Version
	config.Version = "2.2.0"
	t.Cleanup(func() { config.Version = oldVersion })
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var once sync.Once
	server := newProgramUpdateTestServer(t, programUpdateArchive(t), func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
	})
	defer server.Close()
	defer unblock()
	_, runtimeTasks, logic, _ := newProgramUpdateHarness(t, server.URL, time.Minute)
	if _, err := logic.UpdateProgram(); err != nil {
		t.Fatalf("first submission failed: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first task did not reach version lookup")
	}
	if _, err := logic.UpdateProgram(); err == nil {
		t.Fatal("concurrent program update was accepted")
	}
	unblock()
	if err := runtimeTasks.Runner.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProgramUpdatePrepareFailureDoesNotSignalShutdown(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("integration fixture emits an amd64 ELF archive")
	}
	oldVersion := config.Version
	config.Version = "2.2.0"
	t.Cleanup(func() { config.Version = oldVersion })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest/version"):
			_, _ = w.Write([]byte("2.3.4\n"))
		case strings.HasSuffix(r.URL.Path, ".tar.gz.sha256"):
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  dockerCopilot-amd64.tar.gz\n"))
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			_, _ = w.Write(programUpdateArchive(t))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, runtimeTasks, logic, shutdown := newProgramUpdateHarness(t, server.URL, time.Minute)
	response, err := logic.UpdateProgram()
	if err != nil {
		t.Fatalf("submit update: %v", err)
	}
	taskID := response.Data.(map[string]any)["taskID"].(string)
	if err := runtimeTasks.Runner.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := runtimeTasks.Store.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != task.StatusFailed {
		t.Fatalf("invalid package was not rejected before handoff: %#v", record)
	}
	if len(shutdown) != 0 {
		t.Fatal("prepare validation failure signalled shutdown")
	}
}

func newProgramUpdateHarness(t *testing.T, sourceURL string, timeout time.Duration) (*svc.ServiceContext, *svc.TaskRuntime, *UpdateProgramLogic, chan struct{}) {
	t.Helper()
	workDir := t.TempDir()
	tasksDir := t.TempDir()
	cfg := config.Config{}
	cfg.Updates.Repository = "org/project"
	cfg.Updates.GitHubProxy = sourceURL
	cfg.Updates.WorkDir = workDir
	cfg.Tasks.Directory = tasksDir
	runtimeTasks, err := svc.NewTaskRuntime(context.Background(), svc.TaskRuntimeOptions{
		Directory: tasksDir, MaxConcurrent: 1, QueueCapacity: 2, TaskTimeout: timeout,
		RecoveryTimeout: time.Second, Retention: 24 * time.Hour, MaxRecords: 10,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeTasks.Close() })
	shutdown := make(chan struct{}, 1)
	serviceContext := &svc.ServiceContext{
		Config: cfg, TaskRuntime: runtimeTasks, TaskRunner: runtimeTasks.Runner,
		TaskStore: runtimeTasks.Store, TaskQuery: runtimeTasks.Query,
		Mutations: operation.NewMutationGate(), ProgramShutdown: shutdown,
	}
	logic := &UpdateProgramLogic{
		ctx: context.Background(), serviceContext: serviceContext,
		updateSource: cfg.UpdateSource(), validateProgramHandoff: func(context.Context) error { return nil },
	}
	return serviceContext, runtimeTasks, logic, shutdown
}

func newProgramUpdateTestServer(t *testing.T, archive []byte, beforeVersion func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	digest := sha256.Sum256(archive)
	checksum := fmt.Sprintf("%x  dockerCopilot-amd64.tar.gz\n", digest)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest/version"):
			if beforeVersion != nil {
				beforeVersion(w, r)
			}
			_, _ = w.Write([]byte("2.3.4\n"))
		case strings.HasSuffix(r.URL.Path, ".tar.gz.sha256"):
			_, _ = w.Write([]byte(checksum))
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
}

func programUpdateArchive(t *testing.T) []byte {
	t.Helper()
	binary := make([]byte, 64)
	copy(binary[:4], []byte{0x7f, 'E', 'L', 'F'})
	binary[4], binary[5], binary[6] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	binary[16], binary[17] = byte(elf.ET_EXEC), byte(elf.ET_EXEC>>8)
	binary[18], binary[19] = byte(elf.EM_X86_64), byte(elf.EM_X86_64>>8)
	binary[20], binary[52] = byte(elf.EV_CURRENT), 64
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "dockerCopilot-new", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestRollbackFailureDoesNotOverwriteOtherHandoffResult(t *testing.T) {
	workDir := t.TempDir()
	candidate := filepath.Join(workDir, "dockerCopilot-new")
	if err := os.WriteFile(candidate, []byte("candidate binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	taskID := "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e63f"
	metadata := updater.HandoffMetadata{TaskID: taskID, HandoffID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e640", TargetVersion: "2.3.4", GOARCH: "amd64", BinarySHA256: strings.Repeat("a", 64)}
	if err := updater.WriteHandoffMetadata(workDir, metadata); err != nil {
		t.Fatal(err)
	}
	other := updater.HandoffMetadata{TaskID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e641", HandoffID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e642", TargetVersion: metadata.TargetVersion, GOARCH: metadata.GOARCH, BinarySHA256: metadata.BinarySHA256}
	if err := updater.WriteHandoffResult(workDir, updater.HandoffResult{HandoffMetadata: other, Result: "ready"}); err != nil {
		t.Fatal(err)
	}
	state := &programTaskState{
		workDir: workDir, stagingRoot: filepath.Join(t.TempDir(), "stage"), metadata: &metadata,
		placed: &updater.InstallHandoff{TaskID: taskID, TargetVersion: metadata.TargetVersion, GOARCH: metadata.GOARCH, BinarySHA256: metadata.BinarySHA256, Path: candidate},
	}
	if err := state.rollback(); err == nil {
		t.Fatal("expected cleanup failure")
	}
	result, err := updater.ReadHandoffResult(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID != other.TaskID || result.Result != "ready" {
		t.Fatalf("different handoff result was overwritten: %#v", result)
	}
}

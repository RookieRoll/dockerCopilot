package svc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
)

const (
	programTaskRetention  = 10 * time.Minute
	programTaskMaxRecords = 1000
)

// ProgramInstallValidator is deliberately specific to the self-update
// handoff. The general task runtime does not interpret installation records.
type ProgramInstallValidator interface {
	VerifyProgramInstall(context.Context, updater.HandoffMetadata) error
}

// TaskRuntime owns process-local task progress and a bounded runner. Task
// records are intentionally not loaded from or saved to a task directory.
type TaskRuntime struct {
	Store  *task.Store
	Runner *task.Runner
	Query  *task.QueryService

	mu         sync.Mutex
	handoff    *updater.HandoffMetadata
	handoffErr error
	workDir    string
	validator  ProgramInstallValidator
	closeOnce  sync.Once
	closeErr   error
}

// TaskRuntimeOptions contains execution limits only. Directory and history
// fields are retained for source compatibility and are ignored.
type TaskRuntimeOptions struct {
	Directory       string
	MaxConcurrent   int
	QueueCapacity   int
	TaskTimeout     time.Duration
	RecoveryTimeout time.Duration
	Retention       time.Duration
	MaxRecords      int
	StartupTimeout  time.Duration
}

// NewTaskRuntime creates an in-memory store, a bounded runner, and a small
// progress query. Extra legacy arguments are ignored; no task files are read.
func NewTaskRuntime(_ context.Context, opts TaskRuntimeOptions, _ ...any) (*TaskRuntime, error) {
	store, err := task.Open(task.Options{})
	if err != nil {
		return nil, fmt.Errorf("open in-memory task store: %w", err)
	}
	query, err := task.NewQueryService(store, task.QueryOptions{Retention: programTaskRetention, MaxRecords: programTaskMaxRecords})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("create task query service: %w", err)
	}
	runner, err := task.NewRunner(store, task.RunnerOptions{
		MaxConcurrent: opts.MaxConcurrent, TaskTimeout: opts.TaskTimeout,
		RecoveryTimeout: opts.RecoveryTimeout,
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("create task runner: %w", err)
	}
	return &TaskRuntime{Store: store, Runner: runner, Query: query}, nil
}

func NewTaskRuntimeForConfig(ctx context.Context, c config.Config, workDir string) (*TaskRuntime, error) {
	return NewTaskRuntimeForConfigWithValidator(ctx, c, nil, workDir)
}

func NewTaskRuntimeForConfigWithValidator(ctx context.Context, c config.Config, validator ProgramInstallValidator, workDir string) (*TaskRuntime, error) {
	c.ApplyDefaults()
	taskTimeout, err := time.ParseDuration(c.Tasks.Timeout)
	if err != nil {
		return nil, fmt.Errorf("resolve task timeout: %w", err)
	}
	recoveryTimeout, err := time.ParseDuration(c.Tasks.RecoveryTimeout)
	if err != nil {
		return nil, fmt.Errorf("resolve task recovery timeout: %w", err)
	}
	runtime, err := NewTaskRuntime(ctx, TaskRuntimeOptions{
		MaxConcurrent: *c.Tasks.MaxConcurrent, TaskTimeout: taskTimeout, RecoveryTimeout: recoveryTimeout,
	})
	if err != nil {
		return nil, err
	}
	runtime.workDir = workDir
	runtime.validator = validator
	runtime.loadHandoffState()
	return runtime, nil
}

func (r *TaskRuntime) loadHandoffState() {
	if r == nil || r.workDir == "" {
		return
	}
	metadata, err := updater.ReadHandoffMetadata(r.workDir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.handoffErr = fmt.Errorf("read program handoff metadata: %w", err)
		return
	}
	r.handoff = &metadata
}

// MarkProgramHandoff records the in-process view only after the validated
// sidecar has been written successfully.
func (r *TaskRuntime) MarkProgramHandoff(metadata updater.HandoffMetadata) {
	if r == nil {
		return
	}
	r.mu.Lock()
	copy := metadata
	r.handoff = &copy
	r.handoffErr = nil
	r.mu.Unlock()
}

func (r *TaskRuntime) ClearProgramHandoff(taskID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.handoff != nil && r.handoff.TaskID == taskID {
		r.handoff = nil
		r.handoffErr = nil
	}
	r.mu.Unlock()
}

// HasPendingInstall is retained for startup wiring. A sidecar, including a
// malformed one, blocks mutations until readiness verification can resolve it.
func (r *TaskRuntime) HasPendingInstall() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handoff != nil || r.handoffErr != nil
}

// ConfirmPendingInstalls runs only after the new process has passed HTTP
// readiness. It verifies the running executable before recording ready and
// recreates the task result in this process's memory store.
func (r *TaskRuntime) ConfirmPendingInstalls(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.handoffErr != nil {
		err := r.handoffErr
		r.mu.Unlock()
		return err
	}
	if r.handoff == nil {
		r.mu.Unlock()
		return nil
	}
	metadata := *r.handoff
	r.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	currentMetadata, err := updater.ReadHandoffMetadata(r.workDir)
	if err != nil {
		return fmt.Errorf("re-read program handoff metadata: %w", err)
	}
	if !sameHandoff(currentMetadata, metadata) {
		return errors.New("program handoff metadata changed before confirmation")
	}
	result, err := updater.ReadHandoffResult(r.workDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read program handoff result: %w", err)
	}
	if err == nil && !sameHandoff(result.HandoffMetadata, metadata) {
		return errors.New("program handoff result identity does not match metadata")
	}
	if err == nil && result.Result == "failed-start" {
		if err := r.recreateProgramTask(metadata, task.StatusFailed, "program update rolled back", result.ErrorCode); err != nil {
			return err
		}
		r.ClearProgramHandoff(metadata.TaskID)
		return nil
	}
	if r.validator == nil {
		return errors.New("program handoff cannot be confirmed without an install validator")
	}
	if err := r.validator.VerifyProgramInstall(ctx, metadata); err != nil {
		return fmt.Errorf("verify running program for handoff %s: %w", metadata.TaskID, err)
	}
	if err != nil || result.Result != "ready" {
		if err := updater.WriteHandoffResult(r.workDir, updater.HandoffResult{HandoffMetadata: metadata, Result: "ready"}); err != nil {
			// Keep the handoff marker and mutation drain. Never roll back a binary
			// that has already started and passed verification because the result
			// sidecar could not be synced.
			return fmt.Errorf("record ready program handoff: %w", err)
		}
	}
	if err := r.recreateProgramTask(metadata, task.StatusSucceeded, "program update ready", ""); err != nil {
		return err
	}
	r.ClearProgramHandoff(metadata.TaskID)
	return nil
}

func (r *TaskRuntime) recreateProgramTask(metadata updater.HandoffMetadata, status task.Status, message, errorCode string) error {
	if _, err := r.Store.Get(metadata.TaskID); err == nil {
		return nil
	} else if !errors.Is(err, task.ErrTaskNotFound) {
		return fmt.Errorf("inspect in-memory program task: %w", err)
	}
	now := time.Now().UTC()
	started, finished := now, now
	record := task.Record{
		SchemaVersion: task.SchemaVersion, TaskID: metadata.TaskID, Type: task.TaskTypeProgramUpdate,
		Status: status, Stage: string(status), Percentage: 100, Message: message,
		Name: "在线更新程序", ImageRef: metadata.TargetVersion,
		CreatedAt: now, StartedAt: &started, UpdatedAt: now, FinishedAt: &finished,
		IsDone: true, RequestID: metadata.TaskID,
	}
	if errorCode != "" {
		record.Error = "program handoff failed: " + errorCode
		record.Recovery = task.Recovery{Status: "restored", Detail: "launcher restored the previous program"}
	}
	if err := r.Store.Create(record); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("recreate program task progress: %w", err)
	}
	return nil
}

func sameHandoff(a, b updater.HandoffMetadata) bool {
	return a.TaskID == b.TaskID && a.HandoffID == b.HandoffID && a.TargetVersion == b.TargetVersion && a.GOARCH == b.GOARCH && strings.EqualFold(a.BinarySHA256, b.BinarySHA256)
}

func (r *TaskRuntime) HasUnresolvedProgramInstall() bool {
	if r == nil {
		return false
	}
	if r.HasPendingInstall() {
		return true
	}
	records, err := r.Store.List()
	if err != nil {
		return true
	}
	for _, record := range records {
		if record.Type == task.TaskTypeProgramUpdate && (!record.Status.Terminal() || record.RequiresAttention) {
			return true
		}
	}
	return false
}

// Close drains accepted work and closes the in-memory task store. It is safe
// to call more than once.
func (r *TaskRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.Runner != nil {
			r.Runner.Close()
		}
		if r.Store != nil {
			r.closeErr = r.Store.Close()
		}
	})
	return r.closeErr
}

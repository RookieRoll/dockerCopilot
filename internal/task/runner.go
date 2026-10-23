package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/zeromicro/go-zero/core/logx"
)

var (
	ErrQueueFull       = errors.New("background operation concurrency limit reached")
	ErrRunnerClosed    = errors.New("task runner is closed")
	ErrExecutionClosed = errors.New("task execution is already finalized")
)

// RecordStore is the runner's narrow in-process progress dependency.
type RecordStore interface {
	Create(Record) error
	Save(Record) error
	Get(string) (Record, error)
}

type TaskFunc func(context.Context, *Execution) error
type RecoveryFunc func(context.Context, error) (Recovery, error)

type TaskSpec struct {
	Record     Record
	Run        TaskFunc
	Recover    RecoveryFunc
	OnAccepted func() error
	OnSettled  func()
}

type RunnerOptions struct {
	MaxConcurrent   int
	QueueCapacity   int
	TaskTimeout     time.Duration
	RecoveryTimeout time.Duration
	Now             func() time.Time
	Output          io.Writer
}

type Runner struct {
	store           RecordStore
	maxConcurrent   int
	taskTimeout     time.Duration
	recoveryTimeout time.Duration
	now             func() time.Time
	output          io.Writer
	outputMu        sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc

	mu      sync.Mutex
	active  int
	closed  bool
	idle    bool
	idleCh  chan struct{}
	workers sync.WaitGroup
}

type submittedTask struct {
	spec     TaskSpec
	accepted Record
}

func NewRunner(store RecordStore, opts RunnerOptions) (*Runner, error) {
	if store == nil {
		return nil, errors.New("task record store is required")
	}
	if opts.MaxConcurrent < 0 || opts.TaskTimeout < 0 || opts.RecoveryTimeout < 0 {
		return nil, errors.New("runner limits and timeouts cannot be negative")
	}
	if opts.MaxConcurrent == 0 {
		opts.MaxConcurrent = 2
	}
	if opts.MaxConcurrent > 64 {
		return nil, errors.New("runner limits exceed supported bounds")
	}
	if opts.TaskTimeout == 0 {
		opts.TaskTimeout = 30 * time.Minute
	}
	if opts.RecoveryTimeout == 0 {
		opts.RecoveryTimeout = 2 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Output == nil {
		opts.Output = os.Stdout
	}
	runnerCtx, cancel := context.WithCancel(context.Background())
	// Runner diagnostics may contain upstream errors. Always sanitize at the
	// final output boundary, including caller-provided writers.
	opts.Output = observability.NewRedactingTextWriter(opts.Output)
	r := &Runner{
		store: store, maxConcurrent: opts.MaxConcurrent,
		taskTimeout: opts.TaskTimeout, recoveryTimeout: opts.RecoveryTimeout,
		now: opts.Now, output: opts.Output,
		ctx: runnerCtx, cancel: cancel,
		idle: true,
	}
	r.idleCh = make(chan struct{})
	close(r.idleCh)
	return r, nil
}

// Submit reserves an execution slot immediately. QueueCapacity is ignored for
// compatibility; callers receive ErrQueueFull when all slots are occupied.
func (r *Runner) Submit(spec TaskSpec) (string, error) {
	if spec.Run == nil {
		return "", errors.New("task callback is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", ErrRunnerClosed
	}
	if r.active >= r.maxConcurrent {
		return "", ErrQueueFull
	}
	if r.idle {
		r.idle = false
		r.idleCh = make(chan struct{})
	}
	rec := cloneRecord(spec.Record)
	if rec.TaskID == "" {
		rec.TaskID = uuid.NewString()
	}
	if !validTaskID(rec.TaskID) {
		r.markIdleLocked()
		return "", errors.New("task ID must be a canonical UUID")
	}
	if rec.SchemaVersion == 0 {
		rec.SchemaVersion = SchemaVersion
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = r.now().UTC()
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = rec.CreatedAt
	}
	rec.Status = StatusQueued
	rec.IsDone = false
	rec.FinishedAt = nil
	rec.StartedAt = nil
	rec.Error = ""
	rec.SuccessCount, rec.FailedCount, rec.SkippedCount = 0, 0, 0
	for i := range rec.Items {
		rec.Items[i].Status = "queued"
		rec.Items[i].Error = ""
	}
	if err := rec.validate(); err != nil {
		r.markIdleLocked()
		return "", err
	}
	r.active++
	if err := r.store.Create(rec); err != nil {
		r.active--
		r.markIdleLocked()
		return "", fmt.Errorf("create in-memory task: %w", err)
	}
	if spec.OnAccepted != nil {
		if err := invokeAccepted(spec.OnAccepted); err != nil {
			r.active--
			cause := fmt.Errorf("task acceptance callback: %w", err)
			r.finalizeWithoutExecution(rec, cause, false)
			r.markIdleLocked()
			return "", cause
		}
	}
	spec.Record = cloneRecord(rec)
	r.workers.Add(1)
	go func() {
		defer r.workers.Done()
		r.execute(submittedTask{spec: spec, accepted: rec})
		r.mu.Lock()
		r.active--
		r.markIdleLocked()
		r.mu.Unlock()
	}()
	observability.LogTaskEvent(context.Background(), "task.accepted", rec.TaskID, rec.RequestID,
		taskEventFields(rec, string(StatusQueued), nil, r.now())...)
	return rec.TaskID, nil
}

func (r *Runner) execute(q submittedTask) {
	if q.spec.OnSettled != nil {
		defer func() {
			defer func() {
				if value := recover(); value != nil {
					r.writeOutput("task %s settlement callback panicked: %v\n", q.accepted.TaskID, value)
				}
			}()
			q.spec.OnSettled()
		}()
	}
	rec := cloneRecord(q.accepted)
	started := r.now().UTC()
	rec.Status = StatusRunning
	rec.StartedAt = &started
	rec.UpdatedAt = started
	rec.Stage = "starting"
	if err := r.store.Save(rec); err != nil {
		r.finalizeWithoutExecution(rec, fmt.Errorf("update running task: %w", err), true)
		return
	}
	observability.LogTaskEvent(context.Background(), "task.started", rec.TaskID, rec.RequestID,
		taskEventFields(rec, string(StatusRunning), nil, r.now())...)
	ctx, cancel := context.WithTimeout(r.ctx, r.taskTimeout)
	e := &Execution{store: r.store, record: rec, ctx: ctx, cancel: cancel, now: r.now, report: r.writeOutput}
	done := make(chan taskOutcome, 1)
	go func() {
		out := invokeTask(q.spec.Run, ctx, e)
		done <- out
	}()
	select {
	case out := <-done:
		e.markRunReturned()
		if cause := e.failure(); cause != nil {
			out.err = cause
		} else if e.isCommitted() {
			if out.err != nil || out.panicked {
				e.Warning("主操作已完成，但后续清理未确认；不回滚已完成结果")
			}
			r.finish(e, StatusSucceeded, nil, Recovery{Status: "not-needed"}, false)
			cancel()
			return
		} else if deadlineErr := taskContextError(ctx); deadlineErr != nil {
			if e.isCommitted() {
				e.Warning("主操作已完成，但任务期限已到；清理结果尚未确认")
				out.err = nil
			} else {
				out.err = deadlineErr
			}
		}
		r.finishExecution(e, out, q.spec.Recover, (taskContextError(ctx) != nil && !e.isCommitted()) || out.panicked)
		cancel()
	case <-ctx.Done():
		// Keep the record visibly running until the callback really returns, but
		// expose why progress stopped. No recovery or lease release races Run.
		e.markWaitingForRun(ctx.Err())
		out := <-done
		e.markRunReturned()
		if criticalErr := e.failure(); criticalErr != nil {
			out.err = errors.Join(out.err, criticalErr)
		}
		if e.isCommitted() {
			if out.err != nil || out.panicked {
				e.Warning("主操作已完成，但后续清理未确认；不回滚已完成结果")
			}
			r.finish(e, StatusSucceeded, nil, Recovery{Status: "not-needed"}, false)
			cancel()
			return
		}
		out.err = errors.Join(ctx.Err(), out.err)
		r.finishExecution(e, out, q.spec.Recover, true)
		cancel()
	}
}

func (r *Runner) finishExecution(e *Execution, out taskOutcome, recoverFn RecoveryFunc, needsRecovery bool) {
	cause := out.err
	if cause == nil && out.panicked {
		cause = errors.New("task panicked")
	}
	status := StatusSucceeded
	if cause != nil {
		status = StatusFailed
	}
	if (needsRecovery || cause != nil) && recoverFn == nil {
		if cause != nil && !needsRecovery && !out.panicked {
			r.finish(e, StatusFailed, cause, Recovery{Status: "not-needed"}, false)
			return
		}
		detail := "task ended without a recovery callback"
		if cause != nil {
			detail = cause.Error()
		}
		r.finish(e, StatusFailed, cause, Recovery{Status: "unknown", Detail: detail}, true)
		return
	}
	if (needsRecovery || cause != nil) && recoverFn != nil {
		call := r.startRecovery(recoverFn, cause, e)
		// The recovery context is bounded, but the worker and operation lease are
		// held until the callback truly returns. Its result is then finalized once.
		result := <-call.done
		call.cancel()
		recovery, recoveryErr := result.recovery, result.err
		if recoveryErr == nil && recovery.Status == "" {
			recovery = Recovery{Status: "unknown", Detail: "recovery callback returned no status"}
		}
		if recoveryErr != nil {
			cause = errors.Join(cause, fmt.Errorf("recovery callback failed: %w", recoveryErr))
			status = StatusFailed
			recovery = Recovery{Status: "unknown", Detail: recoveryErr.Error()}
		}
		attention := recoveryErr != nil || recovery.Status == "unknown" || recovery.Status == "failed"
		r.logRecovery(e, recovery, attention)
		r.finish(e, status, cause, recovery, attention)
		return
	}
	if cause != nil {
		if out.panicked {
			r.finish(e, status, cause, Recovery{Status: "unknown", Detail: "task panicked; recovery was not available"}, true)
		} else {
			r.finish(e, status, cause, Recovery{}, false)
		}
		return
	}
	r.finish(e, status, nil, Recovery{Status: "not-needed"}, false)
}

func (r *Runner) logRecovery(e *Execution, recovery Recovery, attention bool) {
	e.mu.Lock()
	rec := cloneRecord(e.record)
	e.mu.Unlock()
	observability.LogTaskEvent(context.Background(), "task.recovery", rec.TaskID, rec.RequestID,
		append(taskEventFields(rec, recovery.Status, nil, r.now()),
			logx.Field("recoveryStatus", recovery.Status),
			logx.Field("requiresAttention", attention))...)
}

func (r *Runner) logRecoveryAttempt(e *Execution, cause error) {
	e.mu.Lock()
	rec := cloneRecord(e.record)
	e.mu.Unlock()
	observability.LogTaskEvent(context.Background(), "task.recovery_attempt", rec.TaskID, rec.RequestID,
		append(taskEventFields(rec, "attempting", cause, r.now()), logx.Field("recoveryStatus", "attempting"))...)
}

type recoveryResult struct {
	recovery Recovery
	err      error
}
type recoveryCall struct {
	ctx    context.Context
	done   chan recoveryResult
	cancel context.CancelFunc
}

func (r *Runner) startRecovery(fn RecoveryFunc, cause error, execution *Execution) recoveryCall {
	ctx, cancel := context.WithTimeout(context.Background(), r.recoveryTimeout)
	call := r.startRecoveryWithContext(ctx, fn, cause, execution)
	childCancel := call.cancel
	call.cancel = func() { childCancel(); cancel() }
	return call
}

func (r *Runner) startRecoveryWithContext(parent context.Context, fn RecoveryFunc, cause error, execution *Execution) recoveryCall {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan recoveryResult, 1)
	if execution != nil {
		r.logRecoveryAttempt(execution, cause)
	}
	go func() {
		if execution != nil {
			execution.authorizeRecovery(ctx)
		}
		result := recoveryResult{}
		func() {
			defer func() {
				if value := recover(); value != nil {
					result.err = fmt.Errorf("recovery panic: %v", value)
				}
			}()
			result.recovery, result.err = fn(ctx, cause)
		}()
		if result.err == nil && ctx.Err() != nil {
			result.err = ctx.Err()
		}
		if execution != nil {
			execution.revokeRecovery(ctx)
		}
		done <- result
	}()
	return recoveryCall{ctx: ctx, done: done, cancel: cancel}
}

func (r *Runner) finish(e *Execution, status Status, cause error, recovery Recovery, attention bool) {
	e.mu.Lock()
	if e.finalized {
		e.mu.Unlock()
		return
	}
	e.finalized = true
	rec := cloneRecord(e.record)
	rec.Status = status
	rec.IsDone = status.Terminal()
	rec.FinishedAt = timePointer(r.now().UTC())
	rec.UpdatedAt = *rec.FinishedAt
	if recovery.Status == "" {
		if rec.Recovery.Status == "" {
			recovery = Recovery{Status: "not-needed"}
		} else {
			recovery = rec.Recovery
		}
	}
	rec.Recovery = recovery
	rec.RequiresAttention = attention || rec.RequiresAttention
	if cause != nil {
		rec.Error = cause.Error()
	}
	if e.canReleaseSettledNameProtectionLocked(rec, status) {
		rec.ProtectedNames = nil
	}
	settleBatchItems(&rec, status)
	e.record = cloneRecord(rec)
	err := e.store.Save(rec)
	e.mu.Unlock()
	e.cancel()
	if err != nil {
		r.writeOutput("task %s final in-memory result could not be stored: %v\n", rec.TaskID, err)
	} else {
		observability.LogTaskEvent(context.Background(), "task.finished", rec.TaskID, rec.RequestID,
			append(taskEventFields(rec, string(rec.Status), cause, r.now()),
				logx.Field("recoveryStatus", rec.Recovery.Status),
				logx.Field("requiresAttention", rec.RequiresAttention),
				logx.Field("warningCount", len(rec.Warnings)))...)
	}
}

func (r *Runner) finalizeWithoutExecution(rec Record, cause error, accepted bool) {
	rec.Status = StatusFailed
	rec.IsDone = true
	rec.Error = cause.Error()
	rec.Recovery = Recovery{Status: "not-needed"}
	finished := r.now().UTC()
	rec.FinishedAt = &finished
	rec.UpdatedAt = finished
	if err := r.store.Save(rec); err != nil {
		r.writeOutput("task %s could not store start failure or final result: %v\n", rec.TaskID, errors.Join(cause, err))
	} else if accepted {
		observability.LogTaskEvent(context.Background(), "task.finished", rec.TaskID, rec.RequestID,
			taskEventFields(rec, string(rec.Status), cause, r.now())...)
	}
}

func (r *Runner) writeOutput(format string, args ...any) {
	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	_, _ = fmt.Fprintf(r.output, format, args...)
}

func taskEventFields(record Record, result string, cause error, now time.Time) []logx.LogField {
	containerID := record.NewContainerID
	if containerID == "" {
		containerID = record.OldContainerID
	}
	if containerID == "" && len(record.ContainerIDs) > 0 {
		containerID = record.ContainerIDs[0]
	}
	imageRef := record.ImageRef
	if imageRef == "" {
		imageRef = record.Options.Image
	}
	name := record.Name
	if len(record.Items) > 0 {
		if record.Items[0].Name != "" {
			name = record.Items[0].Name
		}
		if imageRef == "" {
			imageRef = record.Items[0].ImageRef
		}
	}
	imageRef = observability.RedactText(imageRef)
	imageID := ""
	if len(record.ImageIDs) > 0 {
		imageID = record.ImageIDs[len(record.ImageIDs)-1]
	} else if len(record.Items) > 0 && len(record.Items[0].ImageIDs) > 0 {
		imageID = record.Items[0].ImageIDs[len(record.Items[0].ImageIDs)-1]
	}
	fields := []logx.LogField{
		logx.Field("taskType", record.Type),
		logx.Field("operation", record.Type),
		logx.Field("stage", record.Stage),
		logx.Field("containerId", containerID),
		logx.Field("name", name),
		logx.Field("imageRef", imageRef),
		logx.Field("imageId", imageID),
		logx.Field("result", result),
		logx.Field("status", result),
	}
	if record.StartedAt != nil {
		fields = append(fields, logx.Field("durationMs", now.Sub(*record.StartedAt).Milliseconds()))
	}
	if cause != nil {
		fields = append(fields,
			logx.Field("errorCode", observability.SafeErrorCode(cause)),
			logx.Field("error", observability.RedactText(cause.Error())),
		)
	}
	return fields
}

func (r *Runner) markIdleLocked() {
	if r.active == 0 && !r.idle {
		r.idle = true
		close(r.idleCh)
	}
}

// WaitIdle waits until every accepted task and recovery callback has returned.
func (r *Runner) WaitIdle(ctx context.Context) error {
	r.mu.Lock()
	ch := r.idleCh
	r.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops acceptance and waits for accepted work and recovery callbacks.
func (r *Runner) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	r.workers.Wait()
}

func invokeTask(fn TaskFunc, ctx context.Context, e *Execution) (out taskOutcome) {
	defer func() {
		if value := recover(); value != nil {
			out.err = fmt.Errorf("task panic: %v", value)
			out.panicked = true
		}
	}()
	out.err = fn(ctx, e)
	return
}

func invokeAccepted(fn func() error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("acceptance callback panic: %v", value)
		}
	}()
	return fn()
}

type taskOutcome struct {
	err      error
	panicked bool
}

func timePointer(t time.Time) *time.Time { return &t }

// Execution exposes process-local progress and checkpoints to a task callback.
// Critical must succeed before the callback performs the destructive step it
// describes. A failed checkpoint cancels the task context immediately.
type Execution struct {
	mu          sync.Mutex
	store       RecordStore
	record      Record
	ctx         context.Context
	cancel      context.CancelFunc
	now         func() time.Time
	report      func(string, ...any)
	finalized   bool
	criticalErr error
	recoveryCtx context.Context
	committed   bool
	runReturned bool
}

func (e *Execution) markRunReturned() {
	e.mu.Lock()
	e.runReturned = true
	e.mu.Unlock()
}

func (e *Execution) markWaitingForRun(cause error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finalized {
		return
	}
	next := cloneRecord(e.record)
	next.Stage = "waiting-for-operation-exit"
	next.Message = "操作期限已到，等待当前操作退出后再进行恢复"
	if cause != nil {
		next.Message = "操作已取消或超时，等待当前操作退出后再进行恢复"
	}
	next.UpdatedAt = e.now().UTC()
	e.record = next
	if err := e.store.Save(next); err != nil && e.report != nil {
		e.report("task %s waiting status could not be updated in memory: %v\n", next.TaskID, err)
	}
}

func (e *Execution) canReleaseSettledNameProtectionLocked(record Record, status Status) bool {
	record.Status = status
	return e.runReturned && nameProtectionCanBeReleased(record)
}

func nameProtectionCanBeReleased(record Record) bool {
	settledStatus := record.Status == StatusSucceeded || record.Status == StatusFailed
	settledRecovery := record.Recovery.Status == "not-needed" || record.Recovery.Status == "restored"
	return record.Type == "container-update" && settledStatus && settledRecovery &&
		!record.RequiresAttention
}

func (e *Execution) TaskID() string { e.mu.Lock(); defer e.mu.Unlock(); return e.record.TaskID }

func (e *Execution) Progress(stage string, percentage int, message string) error {
	e.mu.Lock()
	if e.finalized {
		e.mu.Unlock()
		return ErrExecutionClosed
	}
	if err := taskContextError(e.ctx); err != nil {
		e.mu.Unlock()
		return err
	}
	if e.criticalErr != nil {
		e.mu.Unlock()
		return e.criticalErr
	}
	next := cloneRecord(e.record)
	previousStage := e.record.Stage
	next.Stage = stage
	next.Percentage = percentage
	next.Message = message
	next.UpdatedAt = e.now().UTC()
	if err := e.store.Save(next); err != nil {
		e.mu.Unlock()
		return err
	}
	e.record = next
	e.mu.Unlock()
	if previousStage != stage {
		observability.LogTaskEvent(context.Background(), "task.stage", next.TaskID, next.RequestID,
			append(taskEventFields(next, string(next.Status), nil, e.now()),
				logx.Field("percentage", percentage))...)
	}
	return nil
}

// Warning records a non-fatal result such as cleanup that failed after the
// primary operation committed. A checkpoint error does not turn a committed
// operation into a failed task.
func (e *Execution) Warning(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	e.mu.Lock()
	if e.finalized {
		report, id := e.report, e.record.TaskID
		e.mu.Unlock()
		if report != nil {
			report("task %s warning arrived after finalization and was not recorded: %s\n", id, message)
		}
		return
	}
	next := cloneRecord(e.record)
	next.Warnings = append(next.Warnings, message)
	next.UpdatedAt = e.now().UTC()
	e.record = next
	err := e.store.Save(next)
	report, id := e.report, next.TaskID
	e.mu.Unlock()
	if err != nil && report != nil {
		report("task %s warning could not be stored yet: %v\n", id, err)
	}
}

func (e *Execution) Critical(stage string, mutate func(*Record)) error {
	return e.critical(stage, mutate, false)
}

// CriticalCommit atomically stores a primary-operation commit checkpoint in
// memory and records the commit under the same execution lock. It is for single-task
// operations whose later steps are cleanup only; batch items must not mark
// their parent task committed.
func (e *Execution) CriticalCommit(stage string, mutate func(*Record)) error {
	return e.critical(stage, mutate, true)
}

// CriticalItemCommit stores one batch item's commit fact together with its
// checkpoint. The item is considered committed only after Save succeeds;
// this does not commit the parent batch task.
func (e *Execution) CriticalItemCommit(index int, stage string, mutate func(*Record)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finalized {
		return ErrExecutionClosed
	}
	if e.criticalErr != nil {
		return e.criticalErr
	}
	if err := taskContextError(e.ctx); err != nil {
		e.cancel()
		return err
	}
	if index < 0 || index >= len(e.record.Items) {
		return errors.New("batch item index is invalid")
	}
	next := cloneRecord(e.record)
	next.Stage = stage
	if mutate != nil {
		mutate(&next)
	}
	next.Items[index].Committed = true
	if next.TaskID != e.record.TaskID || next.Type != e.record.Type || next.Status != StatusRunning || !next.CreatedAt.Equal(e.record.CreatedAt) || next.RequestID != e.record.RequestID || !optionsEqual(next.Options, e.record.Options) {
		return errors.New("critical checkpoint cannot change task identity or status")
	}
	next.UpdatedAt = e.now().UTC()
	if err := e.store.Save(next); err != nil {
		e.criticalErr = fmt.Errorf("critical task checkpoint %q failed: %w", stage, err)
		// Keep observed identity facts, but do not mark the mutation committed.
		next.Items[index].Committed = e.record.Items[index].Committed
		e.record = next
		if e.report != nil {
			e.report("task %s critical checkpoint failure: %v\n", next.TaskID, e.criticalErr)
		}
		e.cancel()
		return e.criticalErr
	}
	e.record = next
	if err := taskContextError(e.ctx); err != nil {
		e.cancel()
		return err
	}
	return nil
}

func (e *Execution) critical(stage string, mutate func(*Record), commit bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finalized {
		return ErrExecutionClosed
	}
	if e.criticalErr != nil {
		return e.criticalErr
	}
	if err := taskContextError(e.ctx); err != nil {
		e.cancel()
		return err
	}
	next := cloneRecord(e.record)
	next.Stage = stage
	if mutate != nil {
		mutate(&next)
	}
	if next.TaskID != e.record.TaskID || next.Type != e.record.Type || next.Status != StatusRunning || !next.CreatedAt.Equal(e.record.CreatedAt) || next.RequestID != e.record.RequestID || !optionsEqual(next.Options, e.record.Options) {
		return errors.New("critical checkpoint cannot change task identity or status")
	}
	next.UpdatedAt = e.now().UTC()
	if err := e.store.Save(next); err != nil {
		e.criticalErr = fmt.Errorf("critical task checkpoint %q failed: %w", stage, err)
		// Preserve the observed in-memory facts for final reporting and recovery,
		// but do not authorize the mutation without a successful checkpoint.
		e.record = next
		if e.report != nil {
			e.report("task %s critical checkpoint failure: %v\n", next.TaskID, e.criticalErr)
		}
		e.cancel()
		return e.criticalErr
	}
	e.record = next
	if commit {
		e.committed = true
	}
	// A deadline may expire while updating progress. Do not let the
	// callback begin the described operation after its budget has elapsed.
	if err := taskContextError(e.ctx); err != nil {
		e.cancel()
		return err
	}
	return nil
}

func (e *Execution) isCommitted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.committed && e.criticalErr == nil
}

func (e *Execution) Current() Record { return e.current() }
func (e *Execution) current() Record { e.mu.Lock(); defer e.mu.Unlock(); return cloneRecord(e.record) }
func (e *Execution) failure() error  { e.mu.Lock(); defer e.mu.Unlock(); return e.criticalErr }
func (e *Execution) taskID() string  { e.mu.Lock(); defer e.mu.Unlock(); return e.record.TaskID }

func taskContextError(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func settleBatchItems(rec *Record, status Status) {
	if len(rec.Items) == 0 {
		return
	}
	for i := range rec.Items {
		if rec.Items[i].Committed {
			rec.Items[i].Status = "succeeded"
			rec.Items[i].RequiresAttention = false
			rec.Items[i].ProtectedNames = nil
			continue
		}
		switch rec.Items[i].Status {
		case "queued":
			rec.Items[i].Status = "skipped"
			if rec.Items[i].Error == "" {
				rec.Items[i].Error = "task ended before item started"
			}
		case "running":
			rec.Items[i].Status = "failed"
			if rec.Items[i].Error == "" {
				rec.Items[i].Error = "item was running when task ended; outcome is uncertain"
			}
		}
		if rec.Items[i].Status == "failed" && len(rec.Items[i].ProtectedNames) > 0 && rec.Items[i].Recovery.Status == "" {
			rec.Items[i].Recovery = Recovery{Status: "unknown", Detail: "item recovery did not complete before the task ended"}
			rec.Items[i].RequiresAttention = true
		}
	}
	recountBatchItems(rec)
}

// batchRecoveryTimedOut saves an unresolved item recovery result before
// cancelling the parent execution. It intentionally bypasses the expired task
// context, but remains private to the runner and only updates the current
// running record's item recovery facts.
func (e *Execution) batchRecoveryTimedOut(index int, timeoutErr error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finalized {
		return ErrExecutionClosed
	}
	if index < 0 || index >= len(e.record.Items) {
		return errors.New("batch recovery item index is invalid")
	}
	next := cloneRecord(e.record)
	item := &next.Items[index]
	item.Recovery = Recovery{Status: "unknown", Detail: "item recovery exceeded its deadline; outcome is uncertain"}
	item.RequiresAttention = true
	if len(item.ProtectedNames) == 0 && item.Name != "" {
		item.ProtectedNames = []string{item.Name}
	}
	if timeoutErr != nil && item.Error == "" {
		item.Error = "item recovery timed out: " + timeoutErr.Error()
	}
	next.UpdatedAt = e.now().UTC()
	recountBatchItems(&next)
	if err := e.store.Save(next); err != nil {
		e.criticalErr = fmt.Errorf("store batch item recovery timeout: %w", err)
		// Keep the in-memory fact for final reporting and cancellation even when
		// this checkpoint itself could not be stored.
		e.record = next
		if e.report != nil {
			e.report("task %s recovery timeout checkpoint failure: %v\n", next.TaskID, e.criticalErr)
		}
		e.cancel()
		return e.criticalErr
	}
	e.record = next
	e.cancel()
	return nil
}

func recountBatchItems(rec *Record) {
	rec.SuccessCount, rec.FailedCount, rec.SkippedCount = 0, 0, 0
	for _, item := range rec.Items {
		switch item.Status {
		case "succeeded":
			rec.SuccessCount++
		case "failed":
			rec.FailedCount++
		case "skipped":
			rec.SkippedCount++
		}
	}
}

type BatchOperation struct {
	Item               Item
	SkipReason         string
	InitialError       string
	PreserveFinalStage bool
	Run                func(context.Context, *Execution) error
	Recover            RecoveryFunc
}

// SubmitBatch runs every item serially inside one worker, keeping one shared
// progress record and recovery scope for the batch.
func (r *Runner) SubmitBatch(record Record, operations []BatchOperation, recoverFn RecoveryFunc) (string, error) {
	return r.SubmitBatchWithHooks(record, operations, recoverFn, nil, nil)
}

// SubmitBatchWithHooks is SubmitBatch plus the same narrow acceptance and
// settlement hooks used by single tasks, so callers can bind batch-wide
// leases for the full worker and recovery lifecycle.
func (r *Runner) SubmitBatchWithHooks(record Record, operations []BatchOperation, recoverFn RecoveryFunc, onAccepted func() error, onSettled func()) (string, error) {
	if len(operations) == 0 {
		return "", errors.New("batch task needs at least one item")
	}
	operations = append([]BatchOperation(nil), operations...)
	items := make([]Item, len(operations))
	for i, operation := range operations {
		if operation.Run == nil && operation.SkipReason == "" && operation.InitialError == "" {
			return "", fmt.Errorf("batch item %d has no callback", i)
		}
		items[i] = operation.Item
		if items[i].ID == "" {
			items[i].ID = fmt.Sprint(i)
		}
		items[i].Status = "queued"
	}
	record.Items = items
	run := func(ctx context.Context, e *Execution) error {
		var failures []error
		for i, operation := range operations {
			if err := ctx.Err(); err != nil {
				break
			}
			if operation.InitialError != "" {
				initialError := operation.InitialError
				if err := e.Critical("batch-item-preflight-failed", func(r *Record) {
					r.Items[i].Status = "failed"
					r.Items[i].Stage = "preflight"
					r.Items[i].Error = initialError
					recountBatchItems(r)
				}); err != nil {
					return errors.Join(errors.Join(failures...), err)
				}
				failures = append(failures, errors.New(initialError))
				continue
			}
			if operation.SkipReason != "" {
				if err := e.Critical("batch-item-skipped", func(r *Record) {
					r.Items[i].Status = "skipped"
					r.Items[i].Stage = "skipped"
					r.Items[i].Error = operation.SkipReason
					recountBatchItems(r)
				}); err != nil {
					return err
				}
				continue
			}
			if err := e.Critical("batch-item-start", func(r *Record) { r.Items[i].Status = "running"; r.Items[i].Stage = "starting" }); err != nil {
				return err
			}
			out := invokeTask(func(ctx context.Context, e *Execution) error { return operation.Run(ctx, e) }, ctx, e)
			status := "succeeded"
			detail := ""
			if out.err != nil {
				status = "failed"
				detail = out.err.Error()
				failures = append(failures, out.err)
			}
			if out.panicked {
				status = "failed"
			}
			if err := e.Critical("batch-item-finished", func(r *Record) {
				r.Items[i].Status = status
				if !operation.PreserveFinalStage || r.Items[i].Stage == "" {
					r.Items[i].Stage = "finished"
				}
				r.Items[i].Error = detail
				recountBatchItems(r)
			}); err != nil {
				return errors.Join(errors.Join(failures...), err)
			}
			if e.failure() != nil {
				return errors.Join(errors.Join(failures...), e.failure())
			}
			if status == "failed" && operation.Recover != nil {
				cause := out.err
				if cause == nil {
					cause = errors.New("batch item failed")
				}
				call := r.startRecovery(operation.Recover, cause, e)
				var result recoveryResult
				select {
				case result = <-call.done:
					call.cancel()
				case <-call.ctx.Done():
					// Make the uncertain outcome queryable immediately and cancel the
					// parent task. The batch callback still holds its worker/lease until
					// this non-cooperative recovery function actually returns.
					timeoutErr := call.ctx.Err()
					persistErr := e.batchRecoveryTimedOut(i, timeoutErr)
					call.cancel()
					<-call.done
					return errors.Join(errors.Join(failures...), fmt.Errorf("batch item recovery timed out: %w", timeoutErr), persistErr)
				}
				recovery := result.recovery
				if result.err == nil && recovery.Status == "" {
					recovery = Recovery{Status: "unknown", Detail: "recovery callback returned no status"}
				}
				if result.err != nil {
					recovery = Recovery{Status: "failed", Detail: result.err.Error()}
				}
				attention := result.err != nil || recovery.Status == "unknown" || recovery.Status == "failed"
				if err := e.Critical("batch-item-recovery-result", func(record *Record) {
					item := &record.Items[i]
					item.Recovery = recovery
					item.RequiresAttention = attention
					if attention {
						if len(item.ProtectedNames) == 0 && item.Name != "" {
							item.ProtectedNames = []string{item.Name}
						}
					} else {
						item.ProtectedNames = nil
					}
					recountBatchItems(record)
				}); err != nil {
					return errors.Join(errors.Join(failures...), err)
				}
				if attention {
					return errors.Join(errors.Join(failures...), errors.New("batch item recovery is unresolved; later items were not started"))
				}
			}
		}
		if ctx.Err() != nil {
			return errors.Join(errors.Join(failures...), ctx.Err())
		}
		return errors.Join(failures...)
	}
	return r.Submit(TaskSpec{Record: record, Run: run, Recover: recoverFn, OnAccepted: onAccepted, OnSettled: onSettled})
}

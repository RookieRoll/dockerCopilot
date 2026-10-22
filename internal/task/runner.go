package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
)

var (
	ErrQueueFull       = errors.New("task queue is full")
	ErrRunnerClosed    = errors.New("task runner is closed")
	ErrExecutionClosed = errors.New("task execution is already finalized")
)

// RecordStore is the runner's narrow persistence dependency. A concrete Store
// also implements ReconciliationStore so timeout recovery facts can be saved
// after the failed terminal result has already been persisted.
type RecordStore interface {
	Create(Record) error
	Save(Record) error
	Get(string) (Record, error)
}

type ReconciliationStore interface {
	Reconcile(string, Reconciliation) error
}

type TaskFunc func(context.Context, *Execution) error
type RecoveryFunc func(context.Context, error) (Recovery, error)

type TaskSpec struct {
	Record  Record
	Run     TaskFunc
	Recover RecoveryFunc
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
	queueCapacity   int
	taskTimeout     time.Duration
	recoveryTimeout time.Duration
	now             func() time.Time
	output          io.Writer
	outputMu        sync.Mutex

	mu      sync.Mutex
	queue   chan queuedTask
	queued  int
	active  int
	closed  bool
	idle    bool
	idleCh  chan struct{}
	workers sync.WaitGroup
}

type queuedTask struct {
	spec     TaskSpec
	accepted Record
}

func NewRunner(store RecordStore, opts RunnerOptions) (*Runner, error) {
	if store == nil {
		return nil, errors.New("task record store is required")
	}
	if opts.MaxConcurrent < 0 || opts.QueueCapacity < 0 || opts.TaskTimeout < 0 || opts.RecoveryTimeout < 0 {
		return nil, errors.New("runner limits and timeouts cannot be negative")
	}
	if opts.MaxConcurrent == 0 {
		opts.MaxConcurrent = 2
	}
	if opts.QueueCapacity == 0 {
		opts.QueueCapacity = 100
	}
	if opts.MaxConcurrent > 64 || opts.QueueCapacity > 10000 {
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
	// Runner diagnostics may contain upstream errors. Always sanitize at the
	// final output boundary, including caller-provided writers.
	opts.Output = observability.NewRedactingTextWriter(opts.Output)
	r := &Runner{
		store: store, maxConcurrent: opts.MaxConcurrent, queueCapacity: opts.QueueCapacity,
		taskTimeout: opts.TaskTimeout, recoveryTimeout: opts.RecoveryTimeout,
		now: opts.Now, output: opts.Output,
		queue: make(chan queuedTask, opts.MaxConcurrent+opts.QueueCapacity), idle: true,
	}
	r.idleCh = make(chan struct{})
	close(r.idleCh)
	for i := 0; i < r.maxConcurrent; i++ {
		r.workers.Add(1)
		go r.worker()
	}
	return r, nil
}

// Submit reserves a bounded admission slot and persists queued before making
// the task visible to workers. The caller's request context is intentionally
// absent: accepted work is detached from HTTP request lifetime.
func (r *Runner) Submit(spec TaskSpec) (string, error) {
	if spec.Run == nil {
		return "", errors.New("task callback is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", ErrRunnerClosed
	}
	if r.active+r.queued >= r.maxConcurrent+r.queueCapacity {
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
	r.queued++ // reserve capacity before potentially slow initial persistence
	if err := r.store.Create(rec); err != nil {
		r.queued--
		r.markIdleLocked()
		return "", fmt.Errorf("persist initial queued task: %w", err)
	}
	spec.Record = cloneRecord(rec)
	select {
	case r.queue <- queuedTask{spec: spec, accepted: rec}:
		return rec.TaskID, nil
	default:
		// Queue capacity is sized to the maximum admitted outstanding work.
		r.queued--
		r.markIdleLocked()
		return "", errors.New("internal task queue capacity invariant violated")
	}
}

func (r *Runner) worker() {
	defer r.workers.Done()
	for queued := range r.queue {
		r.mu.Lock()
		r.queued--
		r.active++
		r.mu.Unlock()
		r.execute(queued)
		r.mu.Lock()
		r.active--
		r.markIdleLocked()
		r.mu.Unlock()
	}
}

func (r *Runner) execute(q queuedTask) {
	rec := cloneRecord(q.accepted)
	started := r.now().UTC()
	rec.Status = StatusRunning
	rec.StartedAt = &started
	rec.UpdatedAt = started
	rec.Stage = "starting"
	if err := r.store.Save(rec); err != nil {
		r.finalizeWithoutExecution(rec, fmt.Errorf("persist running task: %w", err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.taskTimeout)
	e := &Execution{store: r.store, record: rec, ctx: ctx, cancel: cancel, now: r.now, report: r.writeOutput}
	done := make(chan taskOutcome, 1)
	go func() {
		out := invokeTask(q.spec.Run, ctx, e)
		done <- out
	}()
	select {
	case out := <-done:
		if cause := e.failure(); cause != nil {
			out.err = cause
		} else if ctx.Err() != nil {
			out.err = ctx.Err()
		}
		r.finishExecution(e, out, q.spec.Recover, ctx.Err() != nil || out.panicked)
		cancel()
	case <-ctx.Done():
		cause := e.failure()
		if cause == nil {
			cause = ctx.Err()
		}
		rec := e.current()
		rec.Recovery = Recovery{Status: "unknown", Detail: "task stopped at its deadline or critical persistence failed; recovery is pending"}
		r.finish(e, StatusFailed, cause, rec.Recovery, true)
		// Keep this worker slot until Run actually exits. Recovery must never
		// race a callback that may still be mutating Docker state.
		out := <-done
		if criticalErr := e.failure(); criticalErr != nil {
			cause = criticalErr
			out.err = criticalErr
		}
		r.runRecoveryAndReconcile(e, q.spec.Recover, cause, out)
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
		call := r.startRecovery(recoverFn, cause)
		select {
		case result := <-call.done:
			call.cancel()
			recovery, recoveryErr := result.recovery, result.err
			if recoveryErr == nil && recovery.Status == "" {
				recovery = Recovery{Status: "unknown", Detail: "recovery callback returned no status"}
			}
			if recoveryErr != nil {
				cause = errors.Join(cause, fmt.Errorf("recovery callback failed: %w", recoveryErr))
				status = StatusFailed
				recovery = Recovery{Status: "failed", Detail: recoveryErr.Error()}
			}
			r.finish(e, status, cause, recovery, recoveryErr != nil || recovery.Status == "unknown" || recovery.Status == "failed")
			return
		case <-call.ctx.Done():
			timeoutErr := call.ctx.Err()
			cause = errors.Join(cause, fmt.Errorf("recovery callback timed out: %w", timeoutErr))
			r.finish(e, StatusFailed, cause, Recovery{Status: "unknown", Detail: "recovery callback exceeded its deadline; outcome is uncertain"}, true)
			// Keep this worker occupied until recovery exits; never start another
			// mutating callback concurrently with an uncooperative recovery.
			result := <-call.done
			call.cancel()
			r.reconcileLateRecovery(e, result)
			return
		}
	}
	if cause != nil {
		if out.panicked {
			r.finish(e, status, cause, Recovery{Status: "unknown", Detail: "task panicked; recovery was not available"}, true)
		} else {
			r.finish(e, status, cause, Recovery{}, false)
		}
		return
	}
	r.finish(e, status, nil, Recovery{}, false)
}

func (r *Runner) runRecoveryAndReconcile(e *Execution, recoverFn RecoveryFunc, cause error, out taskOutcome) {
	if recoverFn == nil {
		if out.panicked {
			r.writeOutput("task %s panicked after timeout: %v\n", e.taskID(), out.err)
		}
		return
	}
	call := r.startRecovery(recoverFn, cause)
	select {
	case result := <-call.done:
		call.cancel()
		r.reconcileLateRecovery(e, result)
	case <-call.ctx.Done():
		// The failed terminal result has already been persisted. Keep the
		// worker reserved until this callback really exits before reconciling.
		result := <-call.done
		call.cancel()
		r.reconcileLateRecovery(e, result)
	}
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

func (r *Runner) startRecovery(fn RecoveryFunc, cause error) recoveryCall {
	ctx, cancel := context.WithTimeout(context.Background(), r.recoveryTimeout)
	done := make(chan recoveryResult, 1)
	go func() {
		result := recoveryResult{}
		defer func() {
			if value := recover(); value != nil {
				result.err = fmt.Errorf("recovery panic: %v", value)
			}
			done <- result
		}()
		result.recovery, result.err = fn(ctx, cause)
		if result.err == nil && ctx.Err() != nil {
			result.err = ctx.Err()
		}
	}()
	return recoveryCall{ctx: ctx, done: done, cancel: cancel}
}

func (r *Runner) reconcileLateRecovery(e *Execution, result recoveryResult) {
	recovery, recoveryErr := result.recovery, result.err
	if recoveryErr == nil && recovery.Status == "" {
		recovery = Recovery{Status: "unknown", Detail: "recovery callback returned no status"}
	}
	attention := recoveryErr != nil || recovery.Status == "unknown" || recovery.Status == "failed"
	if recoveryErr != nil {
		recovery = Recovery{Status: "failed", Detail: recoveryErr.Error()}
	}
	if store, ok := r.store.(ReconciliationStore); ok {
		finding := e.reconciliation(recovery, attention)
		if err := store.Reconcile(e.taskID(), finding); err != nil {
			r.writeOutput("task %s recovery finding not durably saved: %v\n", e.taskID(), err)
		}
	} else {
		r.writeOutput("task %s record store does not support persisting recovery findings\n", e.taskID())
	}
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
	settleBatchItems(&rec, status)
	e.record = cloneRecord(rec)
	err := e.store.Save(rec)
	e.mu.Unlock()
	e.cancel()
	if err != nil {
		r.writeOutput("task %s final result is not durably saved: %v\n", rec.TaskID, err)
	}
}

func (r *Runner) finalizeWithoutExecution(rec Record, cause error) {
	rec.Status = StatusFailed
	rec.IsDone = true
	rec.Error = cause.Error()
	rec.Recovery = Recovery{Status: "not-needed"}
	finished := r.now().UTC()
	rec.FinishedAt = &finished
	rec.UpdatedAt = finished
	if err := r.store.Save(rec); err != nil {
		r.writeOutput("task %s could not persist start failure or final result: %v\n", rec.TaskID, errors.Join(cause, err))
	}
}

func (r *Runner) writeOutput(format string, args ...any) {
	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	_, _ = fmt.Fprintf(r.output, format, args...)
}

func (r *Runner) markIdleLocked() {
	if r.queued == 0 && r.active == 0 && !r.idle {
		r.idle = true
		close(r.idleCh)
	}
}

// WaitIdle waits until accepted work has left the queue and all callbacks have
// returned. A timed-out callback that ignores context continues to occupy a
// worker, so this method will not report idle until that callback exits.
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

// Close stops acceptance, drains accepted work, and waits for callbacks. Task
// callbacks must observe their context for shutdown to complete promptly.
func (r *Runner) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
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

type taskOutcome struct {
	err      error
	panicked bool
}

func timePointer(t time.Time) *time.Time { return &t }

// Execution exposes progress and durable checkpoints to a task callback.
// Critical must succeed before the callback performs the destructive step it
// describes. A failed critical write cancels the task context immediately.
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
}

func (e *Execution) TaskID() string { e.mu.Lock(); defer e.mu.Unlock(); return e.record.TaskID }

func (e *Execution) Progress(stage string, percentage int, message string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finalized {
		return ErrExecutionClosed
	}
	if err := e.ctx.Err(); err != nil {
		return err
	}
	if e.criticalErr != nil {
		return e.criticalErr
	}
	next := cloneRecord(e.record)
	next.Stage = stage
	next.Percentage = percentage
	next.Message = message
	next.UpdatedAt = e.now().UTC()
	if err := e.store.Save(next); err != nil {
		return err
	}
	e.record = next
	return nil
}

func (e *Execution) Critical(stage string, mutate func(*Record)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finalized {
		return ErrExecutionClosed
	}
	if e.criticalErr != nil {
		return e.criticalErr
	}
	if err := e.ctx.Err(); err != nil {
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
		// Preserve the observed in-memory facts for terminal persistence and
		// recovery even though this checkpoint was not durable.
		e.record = next
		if e.report != nil {
			e.report("task %s critical persistence failure: %v\n", next.TaskID, e.criticalErr)
		}
		e.cancel()
		return e.criticalErr
	}
	e.record = next
	// A deadline may expire while persistence is in progress. Do not let the
	// callback begin the described operation after its budget has elapsed.
	if err := e.ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (e *Execution) Current() Record { return e.current() }
func (e *Execution) current() Record { e.mu.Lock(); defer e.mu.Unlock(); return cloneRecord(e.record) }
func (e *Execution) failure() error  { e.mu.Lock(); defer e.mu.Unlock(); return e.criticalErr }
func (e *Execution) taskID() string  { e.mu.Lock(); defer e.mu.Unlock(); return e.record.TaskID }

func (e *Execution) reconciliation(recovery Recovery, attention bool) Reconciliation {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Reconciliation{Recovery: recovery, RequiresAttention: attention, ProtectedNames: cloneStrings(e.record.ProtectedNames)}
}

func settleBatchItems(rec *Record, status Status) {
	if len(rec.Items) == 0 {
		return
	}
	for i := range rec.Items {
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
	}
	recountBatchItems(rec)
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
	Item       Item
	SkipReason string
	Run        func(context.Context, *Execution) error
}

// SubmitBatch runs every item serially inside one worker. It never submits
// child tasks to the same queue and therefore cannot deadlock by holding a
// worker while awaiting another worker from that pool.
func (r *Runner) SubmitBatch(record Record, operations []BatchOperation, recoverFn RecoveryFunc) (string, error) {
	if len(operations) == 0 {
		return "", errors.New("batch task needs at least one item")
	}
	operations = append([]BatchOperation(nil), operations...)
	items := make([]Item, len(operations))
	for i, operation := range operations {
		if operation.Run == nil && operation.SkipReason == "" {
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
				r.Items[i].Stage = "finished"
				r.Items[i].Error = detail
				recountBatchItems(r)
			}); err != nil {
				return errors.Join(errors.Join(failures...), err)
			}
			if e.failure() != nil {
				return errors.Join(errors.Join(failures...), e.failure())
			}
		}
		if ctx.Err() != nil {
			return errors.Join(errors.Join(failures...), ctx.Err())
		}
		return errors.Join(failures...)
	}
	return r.Submit(TaskSpec{Record: record, Run: run, Recover: recoverFn})
}

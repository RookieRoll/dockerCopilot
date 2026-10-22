package task

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type runnerMemoryStore struct {
	mu             sync.Mutex
	records        map[string]Record
	createErr      error
	saveErr        func(Record, int) error
	saveCalls      int
	reconcileCalls int
	reconcileErr   error
}

func newRunnerMemoryStore() *runnerMemoryStore {
	return &runnerMemoryStore{records: make(map[string]Record)}
}
func (s *runnerMemoryStore) Create(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	if _, ok := s.records[r.TaskID]; ok {
		return errors.New("already exists")
	}
	s.records[r.TaskID] = cloneRecord(r)
	return nil
}
func (s *runnerMemoryStore) Save(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCalls++
	if s.saveErr != nil {
		if err := s.saveErr(r, s.saveCalls); err != nil {
			return err
		}
	}
	old, ok := s.records[r.TaskID]
	if !ok {
		return errors.New("not found")
	}
	if old.Status.Terminal() {
		return errors.New("terminal record is immutable")
	}
	s.records[r.TaskID] = cloneRecord(r)
	return nil
}
func (s *runnerMemoryStore) Get(id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return Record{}, errors.New("not found")
	}
	return cloneRecord(r), nil
}
func (s *runnerMemoryStore) Reconcile(id string, f Reconciliation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconcileCalls++
	if s.reconcileErr != nil {
		return s.reconcileErr
	}
	r, ok := s.records[id]
	if !ok {
		return errors.New("not found")
	}
	if !r.Status.Terminal() {
		return errors.New("not terminal")
	}
	r.Recovery = f.Recovery
	r.RequiresAttention = f.RequiresAttention
	r.ProtectedNames = cloneStrings(f.ProtectedNames)
	s.records[id] = r
	return nil
}

func TestRunnerRejectsInitialPersistenceFailureWithoutRunning(t *testing.T) {
	store := newRunnerMemoryStore()
	store.createErr = errors.New("disk unavailable")
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	var calls atomic.Int32
	_, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { calls.Add(1); return nil }})
	if err == nil || calls.Load() != 0 {
		t.Fatalf("submit err=%v calls=%d", err, calls.Load())
	}
	if err = runner.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerDoesNotRunWhenRunningCheckpointCannotPersist(t *testing.T) {
	store := newRunnerMemoryStore()
	store.saveErr = func(_ Record, call int) error {
		if call == 1 {
			return errors.New("running checkpoint failure")
		}
		return nil
	}
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	var calls atomic.Int32
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { calls.Add(1); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || got.Status != StatusFailed {
		t.Fatalf("callback calls=%d record=%#v", calls.Load(), got)
	}
}

func TestRunnerWithFileStorePersistsFinalRecord(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || !got.IsDone {
		t.Fatalf("file store result=%#v", got)
	}
}

func TestRunnerAdmissionBoundedWhenTimedOutCallbackIgnoresContext(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: 25 * time.Millisecond})
	started := make(chan struct{})
	release := make(chan struct{})
	var late *Execution
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(_ context.Context, e *Execution) error { late = e; close(started); <-release; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	waitFor(t, func() bool { r, _ := store.Get(id); return r.Status == StatusFailed })
	if err := late.Progress("too-late", 99, "late"); !errors.Is(err, ErrExecutionClosed) {
		t.Fatalf("late progress returned %v", err)
	}
	var secondCalls atomic.Int32
	secondID, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { secondCalls.Add(1); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return nil }})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third task admission returned %v", err)
	}
	if secondCalls.Load() != 0 {
		t.Fatal("queued work ran while the timed-out callback still occupied the worker")
	}
	close(release)
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := store.Get(secondID)
	if err != nil || second.Status != StatusSucceeded {
		t.Fatalf("second result=%#v err=%v", second, err)
	}
	first, err := store.Get(id)
	if err != nil || first.Status != StatusFailed {
		t.Fatalf("late callback changed terminal status: %#v err=%v", first, err)
	}
}

func TestRunnerTimeoutUsesIndependentRecoveryContext(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: 20 * time.Millisecond, RecoveryTimeout: time.Second})
	var recovered atomic.Bool
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(ctx context.Context, _ *Execution) error { <-ctx.Done(); return ctx.Err() }, Recover: func(ctx context.Context, cause error) (Recovery, error) {
		if !errors.Is(cause, context.DeadlineExceeded) {
			return Recovery{}, fmt.Errorf("wrong recovery cause: %w", cause)
		}
		if ctx.Err() != nil {
			return Recovery{}, errors.New("recovery context inherited task cancellation")
		}
		if _, ok := ctx.Deadline(); !ok {
			return Recovery{}, errors.New("recovery context has no deadline")
		}
		recovered.Store(true)
		return Recovery{Status: "restored"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Load() || r.Status != StatusFailed || r.Recovery.Status != "restored" || r.RequiresAttention {
		t.Fatalf("recovery result: recovered=%v record=%#v", recovered.Load(), r)
	}
	if store.reconcileCalls != 1 {
		t.Fatalf("reconciliation calls=%d", store.reconcileCalls)
	}
}

func TestRunnerRunsRecoveryForOrdinaryFailure(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	want := errors.New("ordinary operation failure")
	var called atomic.Bool
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return want }, Recover: func(ctx context.Context, cause error) (Recovery, error) {
		called.Store(true)
		if ctx.Err() != nil || !errors.Is(cause, want) {
			return Recovery{}, errors.New("wrong recovery context or cause")
		}
		return Recovery{Status: "not-needed"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !called.Load() || r.Status != StatusFailed || r.Recovery.Status != "not-needed" {
		t.Fatalf("called=%v record=%#v", called.Load(), r)
	}
}

func TestRunnerImmediateRecoveryDoesNotRaceOwnerCancellation(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 4, QueueCapacity: 100})
	ids := make([]string, 64)
	for i := range ids {
		id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return errors.New("operation failed") }, Recover: func(ctx context.Context, _ error) (Recovery, error) {
			if ctx.Err() != nil {
				return Recovery{}, fmt.Errorf("recovery context canceled before callback: %w", ctx.Err())
			}
			return Recovery{Status: "restored"}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	if err := runner.WaitIdle(testContext(t, 3*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		r, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != StatusFailed || r.Recovery.Status != "restored" || r.RequiresAttention || strings.Contains(r.Error, "context canceled") {
			t.Fatalf("immediate recovery incorrectly timed out: %#v", r)
		}
	}
}

func TestRunnerOrdinaryFailureWithoutRecoveryNeedsNoAttention(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return errors.New("image pull failed") }})
	if err != nil {
		t.Fatal(err)
	}
	protected := runnerRecord()
	protected.ProtectedNames = []string{"already-protected"}
	protected.RequiresAttention = true
	protectedID, err := runner.Submit(TaskSpec{Record: protected, Run: func(context.Context, *Execution) error { return errors.New("backup failed") }})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusFailed || r.Recovery.Status != "not-needed" || r.RequiresAttention {
		t.Fatalf("ordinary failure was marked uncertain: %#v", r)
	}
	r, err = store.Get(protectedID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusFailed || r.Recovery.Status != "not-needed" || !r.RequiresAttention || len(r.ProtectedNames) != 1 || r.ProtectedNames[0] != "already-protected" {
		t.Fatalf("existing protection was lost: %#v", r)
	}
}

func TestRunnerRecoveryTimeoutFinalizesAndRetainsWorkerUntilExit(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: time.Second, RecoveryTimeout: 20 * time.Millisecond})
	recoveryStarted, releaseRecovery := make(chan struct{}), make(chan struct{})
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return errors.New("failed") }, Recover: func(context.Context, error) (Recovery, error) {
		close(recoveryStarted)
		<-releaseRecovery
		return Recovery{Status: "restored"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-recoveryStarted
	waitFor(t, func() bool {
		r, _ := store.Get(id)
		return r.Status == StatusFailed && r.Recovery.Status == "unknown" && r.RequiresAttention
	})
	var secondCalls atomic.Int32
	secondID, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { secondCalls.Add(1); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return nil }})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("admission while recovery remains active: %v", err)
	}
	if secondCalls.Load() != 0 {
		t.Fatal("queued callback ran while recovery callback was active")
	}
	close(releaseRecovery)
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	first, _ := store.Get(id)
	second, _ := store.Get(secondID)
	if first.Status != StatusFailed || first.Recovery.Status != "failed" || second.Status != StatusSucceeded {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
}

func TestRunnerCriticalCheckpointFailureCancelsAndRunsRecovery(t *testing.T) {
	store := newRunnerMemoryStore()
	checkpointErr := errors.New("checkpoint disk fault")
	store.saveErr = func(r Record, _ int) error {
		if r.Stage == "stop-old" && r.Status == StatusRunning {
			return checkpointErr
		}
		return nil
	}
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: time.Second, RecoveryTimeout: time.Second})
	var recoverySawContext atomic.Bool
	id, err := runner.Submit(TaskSpec{
		Record: runnerRecord(),
		Run: func(ctx context.Context, e *Execution) error {
			firstErr := e.Critical("stop-old", nil)
			if !errors.Is(firstErr, checkpointErr) {
				return fmt.Errorf("checkpoint error=%v", firstErr)
			}
			if ctx.Err() == nil {
				return errors.New("context wasn't canceled after critical persistence failure")
			}
			if nextErr := e.Critical("remove-old", nil); !errors.Is(nextErr, checkpointErr) {
				return fmt.Errorf("second checkpoint error=%v", nextErr)
			}
			return firstErr
		},
		Recover: func(ctx context.Context, cause error) (Recovery, error) {
			if ctx.Err() == nil && errors.Is(cause, checkpointErr) {
				recoverySawContext.Store(true)
				return Recovery{Status: "restored"}, nil
			}
			return Recovery{}, errors.New("independent recovery context/cause missing")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !recoverySawContext.Load() || r.Status != StatusFailed || r.Recovery.Status != "restored" {
		t.Fatalf("recoveryCtx=%v record=%#v", recoverySawContext.Load(), r)
	}
}

func TestRunnerCriticalFailurePreservesObservedFactsAndReports(t *testing.T) {
	store := newRunnerMemoryStore()
	checkpointErr := errors.New("checkpoint disk fault")
	store.saveErr = func(r Record, _ int) error {
		if r.Stage == "new-container-created" && r.Status == StatusRunning {
			return checkpointErr
		}
		return nil
	}
	var output bytes.Buffer
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, Output: &output})
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(_ context.Context, e *Execution) error {
		return e.Critical("new-container-created", func(r *Record) { r.NewContainerID = "container-observed"; r.ProtectedNames = []string{"protect-me"} })
	}, Recover: func(_ context.Context, cause error) (Recovery, error) {
		if !errors.Is(cause, checkpointErr) {
			return Recovery{}, errors.New("checkpoint failure missing from recovery cause")
		}
		return Recovery{Status: "unknown", Detail: "manual review"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.NewContainerID != "container-observed" || len(r.ProtectedNames) != 1 || r.ProtectedNames[0] != "protect-me" {
		t.Fatalf("checkpoint facts lost: %#v", r)
	}
	if !strings.Contains(output.String(), id) || !strings.Contains(output.String(), "critical persistence failure") || !strings.Contains(output.String(), "checkpoint disk fault") {
		t.Fatalf("missing immediate persistence warning: %q", output.String())
	}
}

func TestRunnerCriticalCheckpointDeadlinePreventsNextStep(t *testing.T) {
	store := newRunnerMemoryStore()
	store.saveErr = func(_ Record, call int) error {
		if call == 2 {
			time.Sleep(45 * time.Millisecond)
		}
		return nil
	}
	var afterCheckpoint atomic.Bool
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: 20 * time.Millisecond})
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(ctx context.Context, e *Execution) error {
		err := e.Critical("slow-save", nil)
		if err == nil {
			afterCheckpoint.Store(true)
		}
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if afterCheckpoint.Load() || r.Status != StatusFailed || !r.RequiresAttention || r.Recovery.Status != "unknown" {
		t.Fatalf("step after expired checkpoint=%v record=%#v", afterCheckpoint.Load(), r)
	}
}

func TestRunnerPanicAndLateProgressCannotChangeTerminalResult(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: time.Second})
	var late *Execution
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(_ context.Context, e *Execution) error {
		late = e
		if err := e.Progress("working", 50, "half"); err != nil {
			return err
		}
		panic("synthetic failure")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := late.Progress("late", 100, "done"); !errors.Is(err, ErrExecutionClosed) {
		t.Fatalf("late progress error=%v", err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusFailed || !r.IsDone || !strings.Contains(r.Error, "synthetic failure") {
		t.Fatalf("panic result=%#v", r)
	}
	if err := store.Save(Record{TaskID: id, SchemaVersion: SchemaVersion, Type: r.Type, Status: StatusRunning, CreatedAt: r.CreatedAt, RequestID: r.RequestID, Options: r.Options}); err == nil {
		t.Fatal("terminal task was overwritten")
	}
}

func TestRunnerBatchUsesOneWorkerAndPersistsPartialCounts(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: time.Second})
	var active, maxActive atomic.Int32
	operations := []BatchOperation{
		{Item: Item{ID: "one", Name: "one"}, Run: func(context.Context, *Execution) error { return runBatchCounter(&active, &maxActive, nil) }},
		{Item: Item{ID: "two", Name: "two"}, Run: func(context.Context, *Execution) error {
			return runBatchCounter(&active, &maxActive, errors.New("item failure"))
		}},
		{Item: Item{ID: "three", Name: "three"}, Run: func(context.Context, *Execution) error { return runBatchCounter(&active, &maxActive, nil) }},
		{Item: Item{ID: "four", Name: "four"}, SkipReason: "conflict with active operation"},
	}
	id, err := runner.SubmitBatch(runnerRecord(), operations, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusFailed || r.SuccessCount != 2 || r.FailedCount != 1 || r.SkippedCount != 1 || len(r.Items) != 4 {
		t.Fatalf("batch result=%#v", r)
	}
	if maxActive.Load() != 1 {
		t.Fatalf("batch item concurrency=%d", maxActive.Load())
	}
}

func TestRunnerBatchMarksRunningUncertainAndQueuedSkippedOnTimeout(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: 25 * time.Millisecond})
	started, release := make(chan struct{}), make(chan struct{})
	operations := []BatchOperation{
		{Item: Item{ID: "running"}, Run: func(context.Context, *Execution) error { close(started); <-release; return nil }},
		{Item: Item{ID: "queued"}, Run: func(context.Context, *Execution) error { return nil }},
	}
	id, err := runner.SubmitBatch(runnerRecord(), operations, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	waitFor(t, func() bool { r, _ := store.Get(id); return r.Status == StatusFailed })
	close(release)
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Items[0].Status != "failed" || !strings.Contains(r.Items[0].Error, "outcome is uncertain") || r.Items[1].Status != "skipped" || r.SuccessCount != 0 || r.FailedCount != 1 || r.SkippedCount != 1 {
		t.Fatalf("batch timeout states/counts: %#v", r)
	}
}

func TestSubmitBatchCopiesCallerOperations(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	started, release := make(chan struct{}), make(chan struct{})
	var originalCalls, replacementCalls atomic.Int32
	operations := []BatchOperation{{Item: Item{ID: "original"}, Run: func(context.Context, *Execution) error { close(started); <-release; originalCalls.Add(1); return nil }}}
	id, err := runner.SubmitBatch(runnerRecord(), operations, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	operations[0] = BatchOperation{Item: Item{ID: "mutated"}, Run: func(context.Context, *Execution) error { replacementCalls.Add(1); return nil }}
	close(release)
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if originalCalls.Load() != 1 || replacementCalls.Load() != 0 || r.Items[0].ID != "original" {
		t.Fatalf("caller slice mutation leaked: calls=(%d,%d) item=%#v", originalCalls.Load(), replacementCalls.Load(), r.Items[0])
	}
}

func TestRunnerReportsFinalPersistenceFailure(t *testing.T) {
	store := newRunnerMemoryStore()
	store.saveErr = func(r Record, _ int) error {
		if r.Status.Terminal() {
			return errors.New("final disk fault Authorization: Bearer runner-fake-jwt")
		}
		return nil
	}
	var output bytes.Buffer
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, Output: &output})
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	r, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusRunning {
		t.Fatalf("failed final write should not pretend durable success: %#v", r)
	}
	if !strings.Contains(output.String(), "not durably saved") || !strings.Contains(output.String(), id) {
		t.Fatalf("missing stdout durability warning: %q", output.String())
	}
	if strings.Contains(output.String(), "runner-fake-jwt") || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("runner output did not redact persistence error: %q", output.String())
	}
}

func newTestRunner(t *testing.T, store RecordStore, opts RunnerOptions) *Runner {
	t.Helper()
	runner, err := NewRunner(store, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	return runner
}
func runnerRecord() Record {
	return Record{SchemaVersion: SchemaVersion, TaskID: uuid.NewString(), Type: "test", Status: StatusQueued}
}
func testContext(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before deadline")
}
func runBatchCounter(active, maxActive *atomic.Int32, err error) error {
	n := active.Add(1)
	for {
		old := maxActive.Load()
		if n <= old || maxActive.CompareAndSwap(old, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	active.Add(-1)
	return err
}

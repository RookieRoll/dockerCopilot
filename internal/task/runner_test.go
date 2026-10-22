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
	"github.com/onlyLTY/dockerCopilot/internal/operation"
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
func (s *runnerMemoryStore) saveRecoveryCheckpoint(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.records[r.TaskID]
	if !ok || old.Status != r.Status || old.Error != r.Error || !old.CreatedAt.Equal(r.CreatedAt) {
		return errors.New("invalid recovery checkpoint")
	}
	old.Stage = r.Stage
	old.NewContainerID = r.NewContainerID
	old.ContainerIDs = cloneStrings(r.ContainerIDs)
	old.BackupContainerName = r.BackupContainerName
	old.UpdatedAt = r.UpdatedAt
	s.records[r.TaskID] = old
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
	if f.Error != "" {
		r.Error = f.Error
	}
	if f.ClearPendingInstall {
		r.PendingInstall = nil
	}
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

func TestRunnerDefersProgramUpdateFinalizationUntilPendingInstallConfirmation(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	marker := PendingInstallHandoff{
		HandoffID: "10000000-0000-4000-8000-000000000061", TargetVersion: "v2.3.4",
		GOARCH: "amd64", ArtifactSHA256: strings.Repeat("a", 64), PreparedAt: time.Now().UTC(),
	}
	var handoffCalls atomic.Int32
	id, err := runner.Submit(TaskSpec{
		Record: func() Record { r := runnerRecord(); r.Type = TaskTypeProgramUpdate; return r }(),
		Run: func(_ context.Context, execution *Execution) error {
			return execution.MarkPendingInstall(marker)
		},
		OnHandoff: func(record Record) error {
			handoffCalls.Add(1)
			if record.Status != StatusRunning || record.PendingInstall == nil || record.PendingInstall.HandoffID != marker.HandoffID {
				return fmt.Errorf("handoff callback received unpersisted record: %#v", record)
			}
			persisted, getErr := store.Get(record.TaskID)
			if getErr != nil || persisted.PendingInstall == nil || persisted.PendingInstall.HandoffID != marker.HandoffID {
				return fmt.Errorf("handoff marker was not persisted before callback: %#v err=%v", persisted, getErr)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	record, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if handoffCalls.Load() != 1 || record.Status != StatusRunning || record.IsDone || record.FinishedAt != nil || record.PendingInstall == nil {
		t.Fatalf("successful handoff was finalized early: calls=%d record=%#v", handoffCalls.Load(), record)
	}
}

func TestRunnerFailedHandoffNeverLeavesPendingOrReportsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name       string
		onHandoff  func(Record) error
		runError   error
		wantCalled bool
	}{
		{name: "missing callback"},
		{name: "callback error", onHandoff: func(Record) error { return errors.New("shutdown signal unavailable") }, wantCalled: true},
		{name: "callback panic", onHandoff: func(Record) error { panic("shutdown signal panic") }, wantCalled: true},
		{name: "run error after marker", onHandoff: func(Record) error { return nil }, runError: errors.New("post-handoff preparation failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRunnerMemoryStore()
			runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
			marker := PendingInstallHandoff{HandoffID: "10000000-0000-4000-8000-000000000062", TargetVersion: "2.3.4", GOARCH: "arm64", ArtifactSHA256: strings.Repeat("b", 64), PreparedAt: time.Now().UTC()}
			var callbackCalls atomic.Int32
			callback := tc.onHandoff
			if callback != nil {
				callback = func(record Record) error {
					callbackCalls.Add(1)
					return tc.onHandoff(record)
				}
			}
			record := runnerRecord()
			record.Type = TaskTypeProgramUpdate
			var rollbackCalls atomic.Int32
			id, err := runner.Submit(TaskSpec{Record: record, Run: func(_ context.Context, execution *Execution) error {
				if err := execution.MarkPendingInstall(marker); err != nil {
					return err
				}
				return tc.runError
			}, OnHandoff: callback, RollbackHandoff: func(context.Context, Record) error { rollbackCalls.Add(1); return nil }})
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
			if got.Status != StatusFailed || !got.IsDone || got.PendingInstall != nil || tc.wantCalled != (callbackCalls.Load() == 1) || rollbackCalls.Load() != 1 {
				t.Fatalf("failed handoff state: callback=%d rollback=%d record=%#v", callbackCalls.Load(), rollbackCalls.Load(), got)
			}
		})
	}
}

func TestRunnerDoesNotSignalHandoffWhenMarkerPersistenceFails(t *testing.T) {
	store := newRunnerMemoryStore()
	store.saveErr = func(record Record, _ int) error {
		if record.Stage == "install-handoff" {
			return errors.New("pending marker storage unavailable")
		}
		return nil
	}
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	var handoffCalls atomic.Int32
	record := runnerRecord()
	record.Type = TaskTypeProgramUpdate
	id, err := runner.Submit(TaskSpec{
		Record: record,
		Run: func(_ context.Context, execution *Execution) error {
			return execution.MarkPendingInstall(PendingInstallHandoff{
				HandoffID: "10000000-0000-4000-8000-000000000063", TargetVersion: "2.3.4", GOARCH: "amd64",
				ArtifactSHA256: strings.Repeat("d", 64), PreparedAt: time.Now().UTC(),
			})
		},
		OnHandoff:       func(Record) error { handoffCalls.Add(1); return nil },
		RollbackHandoff: func(context.Context, Record) error { return nil },
	})
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
	if handoffCalls.Load() != 0 || got.Status != StatusFailed || !got.RequiresAttention || got.PendingInstall != nil || !strings.Contains(got.Error, "pending marker storage unavailable") {
		t.Fatalf("marker persistence failure did not stop handoff: calls=%d record=%#v", handoffCalls.Load(), got)
	}
}

func TestRunnerMarksHandoffRollbackFailureForAttention(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	record := runnerRecord()
	record.Type = TaskTypeProgramUpdate
	id, err := runner.Submit(TaskSpec{
		Record: record,
		Run: func(_ context.Context, execution *Execution) error {
			return execution.MarkPendingInstall(PendingInstallHandoff{HandoffID: "10000000-0000-4000-8000-000000000064", TargetVersion: "2.3.4", GOARCH: "amd64", ArtifactSHA256: strings.Repeat("e", 64), PreparedAt: time.Now().UTC()})
		},
		OnHandoff:       func(Record) error { return errors.New("shutdown signal unavailable") },
		RollbackHandoff: func(context.Context, Record) error { return errors.New("candidate cleanup failed") },
	})
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
	if got.Status != StatusFailed || !got.RequiresAttention || got.Recovery.Status != "unknown" || got.PendingInstall == nil || !strings.Contains(got.Error, "candidate cleanup failed") {
		t.Fatalf("rollback failure was not surfaced as uncertain: %#v", got)
	}
}

func TestRunnerRunsRecoveryAfterHandoffNotificationFailure(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	record := runnerRecord()
	record.Type = TaskTypeProgramUpdate
	var recoverCalls atomic.Int32
	id, err := runner.Submit(TaskSpec{
		Record: record,
		Run: func(_ context.Context, execution *Execution) error {
			return execution.MarkPendingInstall(PendingInstallHandoff{HandoffID: "10000000-0000-4000-8000-000000000065", TargetVersion: "2.3.4", GOARCH: "amd64", ArtifactSHA256: strings.Repeat("f", 64), PreparedAt: time.Now().UTC()})
		},
		OnHandoff:       func(Record) error { return errors.New("shutdown signal failed") },
		RollbackHandoff: func(context.Context, Record) error { return errors.New("rollback needs recovery") },
		Recover: func(context.Context, error) (Recovery, error) {
			recoverCalls.Add(1)
			return Recovery{Status: "restored", Detail: "candidate removed and gate resumed"}, nil
		},
	})
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
	if recoverCalls.Load() != 1 || got.Status != StatusFailed || got.Recovery.Status != "restored" || !got.RequiresAttention || got.PendingInstall == nil {
		t.Fatalf("handoff recovery result: calls=%d record=%#v", recoverCalls.Load(), got)
	}
}

func TestRunnerDoesNotRollbackHandoffUntilUncooperativeRunReturns(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: 30 * time.Millisecond, RecoveryTimeout: time.Second})
	runCtxReady := make(chan struct{})
	deadlineReached := make(chan struct{})
	releaseRun := make(chan struct{})
	var releaseRunOnce sync.Once
	releaseRunCallback := func() { releaseRunOnce.Do(func() { close(releaseRun) }) }
	t.Cleanup(releaseRunCallback)
	var candidatePlaced atomic.Bool
	var runReturned atomic.Bool
	var rollbackCalls atomic.Int32
	record := runnerRecord()
	record.Type = TaskTypeProgramUpdate
	id, err := runner.Submit(TaskSpec{
		Record: record,
		Run: func(ctx context.Context, execution *Execution) error {
			if err := execution.MarkPendingInstall(PendingInstallHandoff{HandoffID: "10000000-0000-4000-8000-000000000066", TargetVersion: "2.3.4", GOARCH: "amd64", ArtifactSHA256: strings.Repeat("1", 64), PreparedAt: time.Now().UTC()}); err != nil {
				return err
			}
			close(runCtxReady)
			go func() { <-ctx.Done(); close(deadlineReached) }()
			<-releaseRun // deliberately ignores deadline while a candidate is being placed
			candidatePlaced.Store(true)
			runReturned.Store(true)
			return nil
		},
		OnHandoff: func(Record) error { return errors.New("deadline should prevent handoff notification") },
		RollbackHandoff: func(ctx context.Context, _ Record) error {
			rollbackCalls.Add(1)
			if !runReturned.Load() || !candidatePlaced.Load() {
				return errors.New("rollback ran before Run finished placing candidate")
			}
			candidatePlaced.Store(false)
			return nil
		},
		Recover: func(context.Context, error) (Recovery, error) {
			if candidatePlaced.Load() {
				return Recovery{Status: "unknown", Detail: "candidate still exists"}, errors.New("candidate cleanup did not finish")
			}
			return Recovery{Status: "restored"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-runCtxReady
	<-deadlineReached
	if rollbackCalls.Load() != 0 {
		t.Fatal("handoff cleanup raced the still-running task callback")
	}
	var terminal Record
	waitFor(t, func() bool {
		var getErr error
		terminal, getErr = store.Get(id)
		return getErr == nil && terminal.Status == StatusFailed
	})
	if terminal.Status != StatusFailed || !terminal.RequiresAttention || terminal.PendingInstall == nil {
		t.Fatalf("deadline did not promptly persist failed/unknown handoff facts: %#v", terminal)
	}
	releaseRunCallback()
	if err := runner.WaitIdle(testContext(t, 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if rollbackCalls.Load() != 1 || candidatePlaced.Load() || got.Status != StatusFailed || got.PendingInstall != nil || got.Recovery.Status != "restored" || got.RequiresAttention {
		t.Fatalf("timeout handoff cleanup did not run after Run exited: rollback=%d candidate=%v record=%#v", rollbackCalls.Load(), candidatePlaced.Load(), got)
	}
}

func TestRunnerFinalizesBeforeUncooperativeHandoffRollbackReturns(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: time.Second, RecoveryTimeout: 30 * time.Millisecond})
	rollbackEntered := make(chan struct{})
	rollbackDeadline := make(chan struct{})
	releaseRollback := make(chan struct{})
	var releaseRollbackOnce sync.Once
	releaseRollbackCallback := func() { releaseRollbackOnce.Do(func() { close(releaseRollback) }) }
	t.Cleanup(releaseRollbackCallback)
	settled := make(chan struct{})
	record := runnerRecord()
	record.Type = TaskTypeProgramUpdate
	id, err := runner.Submit(TaskSpec{
		Record: record,
		Run: func(_ context.Context, execution *Execution) error {
			return execution.MarkPendingInstall(PendingInstallHandoff{HandoffID: "10000000-0000-4000-8000-000000000068", TargetVersion: "2.3.4", GOARCH: "amd64", ArtifactSHA256: strings.Repeat("2", 64), PreparedAt: time.Now().UTC()})
		},
		OnHandoff: func(Record) error { return errors.New("shutdown signal unavailable") },
		RollbackHandoff: func(ctx context.Context, _ Record) error {
			close(rollbackEntered)
			<-ctx.Done()
			close(rollbackDeadline)
			<-releaseRollback // deliberately ignores the shared recovery deadline
			return nil
		},
		Recover: func(context.Context, error) (Recovery, error) {
			return Recovery{Status: "restored"}, nil
		},
		OnSettled: func() { close(settled) },
	})
	if err != nil {
		t.Fatal(err)
	}
	<-rollbackEntered
	deadline := time.Now().Add(time.Second)
	var terminal Record
	for time.Now().Before(deadline) {
		terminal, err = store.Get(id)
		if err == nil && terminal.Status == StatusFailed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if terminal.Status != StatusFailed || terminal.Recovery.Status != "unknown" || terminal.PendingInstall == nil || !terminal.RequiresAttention {
		t.Fatalf("slow rollback left task nonterminal or discarded handoff facts: %#v", terminal)
	}
	select {
	case <-rollbackDeadline:
	case <-time.After(time.Second):
		t.Fatal("rollback callback did not observe its bounded recovery deadline")
	}
	select {
	case <-settled:
		t.Fatal("settlement ran while rollback callback was still executing")
	default:
	}
	releaseRollbackCallback()
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("settlement did not run after rollback callback returned")
	}
	terminal, err = store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != StatusFailed || terminal.PendingInstall == nil || !terminal.RequiresAttention {
		t.Fatalf("expired rollback incorrectly cleared handoff evidence: %#v", terminal)
	}
}

func TestOnSettledWaitsUntilRunAndRecoveryCallbacksReallyReturn(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: 20 * time.Millisecond, RecoveryTimeout: time.Second})
	runEntered := make(chan struct{})
	releaseRun := make(chan struct{})
	recoveryEntered := make(chan struct{})
	releaseRecovery := make(chan struct{})
	var releaseRunOnce, releaseRecoveryOnce sync.Once
	releaseAll := func() {
		releaseRunOnce.Do(func() { close(releaseRun) })
		releaseRecoveryOnce.Do(func() { close(releaseRecovery) })
	}
	t.Cleanup(releaseAll)
	settled := make(chan struct{})
	_, err := runner.Submit(TaskSpec{
		Record: runnerRecord(),
		Run: func(context.Context, *Execution) error {
			close(runEntered)
			<-releaseRun // deliberately ignores the task deadline
			return errors.New("late operation failure")
		},
		Recover: func(context.Context, error) (Recovery, error) {
			close(recoveryEntered)
			<-releaseRecovery
			return Recovery{Status: "restored"}, nil
		},
		OnSettled: func() { close(settled) },
	})
	if err != nil {
		t.Fatal(err)
	}
	<-runEntered
	select {
	case <-settled:
		t.Fatal("settled callback ran while task callback was still executing")
	case <-time.After(60 * time.Millisecond):
	}
	releaseRunOnce.Do(func() { close(releaseRun) })
	<-recoveryEntered
	select {
	case <-settled:
		t.Fatal("settled callback ran while recovery callback was still executing")
	case <-time.After(20 * time.Millisecond):
	}
	releaseRecoveryOnce.Do(func() { close(releaseRecovery) })
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("settled callback did not run after recovery returned")
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestOnSettledDoesNotRunWhenSubmitIsRejected(t *testing.T) {
	store := newRunnerMemoryStore()
	store.createErr = errors.New("disk unavailable")
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	var calls atomic.Int32
	_, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return nil }, OnSettled: func() { calls.Add(1) }})
	if err == nil {
		t.Fatal("expected initial persistence failure")
	}
	if calls.Load() != 0 {
		t.Fatalf("settlement callback called for rejected task: %d", calls.Load())
	}
}

func TestOnAcceptedConfirmsDurableAdmissionBeforeWorkerCanRun(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	accepted := make(chan struct{})
	var ran atomic.Bool
	id, err := runner.Submit(TaskSpec{
		Record: runnerRecord(),
		OnAccepted: func() error {
			close(accepted)
			return nil
		},
		Run: func(context.Context, *Execution) error {
			select {
			case <-accepted:
			default:
				return errors.New("worker ran before durable admission callback")
			}
			ran.Store(true)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	if !ran.Load() {
		t.Fatalf("accepted task %s did not run", id)
	}
}

func TestOnAcceptedFailureRejectsTaskAndFinalizesDurableRecord(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	var ran atomic.Int32
	_, err := runner.Submit(TaskSpec{
		Record:     runnerRecord(),
		OnAccepted: func() error { return errors.New("lease confirmation failed") },
		Run:        func(context.Context, *Execution) error { ran.Add(1); return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "lease confirmation failed") {
		t.Fatalf("Submit error=%v; want acceptance failure", err)
	}
	if ran.Load() != 0 {
		t.Fatal("task ran after acceptance callback failed")
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.records) != 1 {
		t.Fatalf("durable queued record was lost after acceptance error: %d records", len(store.records))
	}
	for _, record := range store.records {
		if record.Status != StatusFailed {
			t.Fatalf("acceptance failure record status=%q, want failed", record.Status)
		}
	}
}

func TestFastSettlementCannotMakePostSubmitConfirmationFail(t *testing.T) {
	coordinator := operation.NewCoordinator()
	taskID := uuid.NewString()
	reservation, err := coordinator.Reserve(operation.Request{
		TaskID: taskID, Kind: operation.ContainerUpdate,
		Target: operation.Target{ID: "container-id", Name: "web"}, EquivalenceKey: "fingerprint",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, newRunnerMemoryStore(), RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	record := runnerRecord()
	record.TaskID = taskID
	spec := TaskSpec{
		Record:     record,
		OnAccepted: func() error { return reservation.Lease.Confirm() },
		OnSettled:  reservation.Lease.Release,
		Run:        func(context.Context, *Execution) error { return nil },
	}
	_, err = runner.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	// A fast worker has now settled and released before this confirmation.
	// Confirm must remain idempotent so adapters cannot report a false failure.
	if err := reservation.Lease.Confirm(); err != nil {
		t.Fatalf("post-submit confirmation raced settlement: %v", err)
	}
	if _, err := coordinator.Reserve(operation.Request{TaskID: "next", Kind: "container-stop", Target: operation.Target{ID: "container-id", Name: "web"}}); err != nil {
		t.Fatalf("settled task retained container lease: %v", err)
	}
}

func TestOnAcceptedPanicRejectsTaskAndDoesNotKillRunner(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	var calls atomic.Int32
	_, err := runner.Submit(TaskSpec{
		Record:     runnerRecord(),
		OnAccepted: func() error { panic("confirm hook panic") },
		Run:        func(context.Context, *Execution) error { calls.Add(1); return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "acceptance callback panic") {
		t.Fatalf("Submit error=%v; want recovered acceptance panic", err)
	}
	if calls.Load() != 0 {
		t.Fatal("task ran after acceptance callback panic")
	}
	var ran atomic.Int32
	if _, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { ran.Add(1); return nil }}); err != nil {
		t.Fatalf("runner rejected later work after acceptance panic: %v", err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Fatalf("runner worker did not recover after acceptance panic: calls=%d", ran.Load())
	}
}

func TestOnSettledPanicDoesNotStopRunnerWorker(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	var ran atomic.Int32
	_, err := runner.Submit(TaskSpec{
		Record:    runnerRecord(),
		Run:       func(context.Context, *Execution) error { return nil },
		OnSettled: func() { panic("settlement hook bug") },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { ran.Add(1); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Fatalf("worker failed to process later task after hook panic; calls=%d", ran.Load())
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
	var settled atomic.Int32
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { calls.Add(1); return nil }, OnSettled: func() { settled.Add(1) }})
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
	if calls.Load() != 0 || got.Status != StatusFailed || settled.Load() != 1 {
		t.Fatalf("callback calls=%d settled=%d record=%#v", calls.Load(), settled.Load(), got)
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

func TestRecoveryCheckpointIsAuthorizedAndPersistsIdentityFacts(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	want := errors.New("update failed after new container creation")
	var execution *Execution
	id, err := runner.Submit(TaskSpec{
		Record: runnerRecord(),
		Run: func(_ context.Context, e *Execution) error {
			execution = e
			return want
		},
		Recover: func(ctx context.Context, cause error) (Recovery, error) {
			if !errors.Is(cause, want) {
				return Recovery{}, errors.New("recovery cause was not preserved")
			}
			// The callback cannot get its Execution through the signature, so the
			// test uses the reference assigned by the Run callback below.
			if execution == nil {
				return Recovery{}, errors.New("execution reference unavailable")
			}
			err := execution.RecoveryCheckpoint(ctx, "recovering-old", func(record *Record) {
				record.NewContainerID = "new-confirmed"
				record.ContainerIDs = append(record.ContainerIDs, "new-confirmed")
			})
			if err != nil {
				return Recovery{}, err
			}
			return Recovery{Status: "restored"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	record, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusFailed || record.Stage != "recovering-old" || record.NewContainerID != "new-confirmed" ||
		record.Recovery.Status != "restored" || record.Error != want.Error() {
		t.Fatalf("recovery checkpoint/final result mismatch: %#v", record)
	}
}

func TestRecoveryCheckpointCanAddFactsAfterTimeoutWithoutRewritingTerminalResult(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: 20 * time.Millisecond, RecoveryTimeout: time.Second})
	var execution *Execution
	record := runnerRecord()
	id := record.TaskID
	_, err := runner.Submit(TaskSpec{
		Record: record,
		Run: func(ctx context.Context, e *Execution) error {
			execution = e
			<-ctx.Done()
			return ctx.Err()
		},
		Recover: func(ctx context.Context, cause error) (Recovery, error) {
			before, err := store.Get(id)
			if err != nil {
				return Recovery{}, err
			}
			if before.Status != StatusFailed || before.FinishedAt == nil || !errors.Is(cause, context.DeadlineExceeded) {
				return Recovery{}, errors.New("timeout terminal result was not persisted before recovery")
			}
			if err := execution.RecoveryCheckpoint(ctx, "recovering-old", func(record *Record) {
				record.NewContainerID = "new-after-timeout"
				record.ContainerIDs = append(record.ContainerIDs, "new-after-timeout")
			}); err != nil {
				return Recovery{}, err
			}
			after, err := store.Get(id)
			if err != nil {
				return Recovery{}, err
			}
			if after.Status != before.Status || after.Error != before.Error || !after.FinishedAt.Equal(*before.FinishedAt) {
				return Recovery{}, errors.New("recovery checkpoint rewrote terminal task result")
			}
			return Recovery{Status: "restored"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	finalRecord, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if finalRecord.Status != StatusFailed || finalRecord.Stage != "recovering-old" || finalRecord.NewContainerID != "new-after-timeout" || finalRecord.Recovery.Status != "restored" {
		t.Fatalf("timeout recovery result: %#v", finalRecord)
	}
}

func TestRecoveryCheckpointRejectsCallsOutsideAuthorizedCallback(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	var execution *Execution
	id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(_ context.Context, e *Execution) error {
		execution = e
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := execution.RecoveryCheckpoint(context.Background(), "unauthorized", nil); err == nil {
		t.Fatal("recovery checkpoint succeeded outside the active recovery callback")
	}
	record, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusSucceeded || record.Stage == "unauthorized" {
		t.Fatalf("unauthorized recovery altered task: %#v", record)
	}
}

func TestRecoveryCheckpointAuthorizationEndsBeforeCallbackSettlement(t *testing.T) {
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	var execution *Execution
	var recoveryCtx context.Context
	id, err := runner.Submit(TaskSpec{
		Record: runnerRecord(),
		Run: func(_ context.Context, e *Execution) error {
			execution = e
			return errors.New("trigger recovery")
		},
		Recover: func(ctx context.Context, _ error) (Recovery, error) {
			recoveryCtx = ctx
			return Recovery{Status: "not-needed"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := execution.RecoveryCheckpoint(recoveryCtx, "late", nil); err == nil {
		t.Fatal("checkpoint remained authorized after the recovery callback returned")
	}
	record, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Stage == "late" || record.Recovery.Status != "not-needed" {
		t.Fatalf("late recovery checkpoint altered final record: %#v", record)
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

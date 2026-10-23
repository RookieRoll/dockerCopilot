package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func runnerRecord() Record {
	return Record{TaskID: uuid.NewString(), Type: "test", Status: StatusQueued}
}

func newRunnerMemoryStore() *Store {
	store, err := Open(Options{})
	if err != nil {
		panic(err)
	}
	return store
}

func newTestRunner(t *testing.T, store RecordStore, opts RunnerOptions) *Runner {
	t.Helper()
	if opts.Output == nil {
		opts.Output = &strings.Builder{}
	}
	runner, err := NewRunner(store, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	return runner
}

func testContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

func TestStoreIsMemoryOnlyAndRestartForgetsRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "must-not-be-created")
	store, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open touched compatibility Dir: %v", err)
	}
	record := runnerRecord()
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := restarted.Get(record.TaskID); !errors.Is(err, ErrTaskNotFound) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record survived restart or missing sentinel changed: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reopened store touched compatibility Dir: %v", err)
	}
}

func TestStoreCopiesRecordsAndSnapshotsAndEnforcesSnapshotBudgets(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	record := runnerRecord()
	record.Items = []Item{{ID: "0", SnapshotRef: uuid.NewString()}}
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(record.TaskID, []byte("rollback")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveItemSnapshot(record.TaskID, record.Items[0].SnapshotRef, []byte("item")); err != nil {
		t.Fatal(err)
	}
	read, err := store.ReadSnapshot(record.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	read[0] = 'X'
	second, err := store.ReadSnapshot(record.TaskID)
	if err != nil || string(second) != "rollback" {
		t.Fatalf("snapshot read was not copied: %q, %v", second, err)
	}
	item, err := store.ReadItemSnapshot(record.TaskID, record.Items[0].SnapshotRef)
	if err != nil {
		t.Fatal(err)
	}
	item[0] = 'X'
	itemAgain, err := store.ReadItemSnapshot(record.TaskID, record.Items[0].SnapshotRef)
	if err != nil || string(itemAgain) != "item" {
		t.Fatalf("item snapshot read was not copied: %q, %v", itemAgain, err)
	}
	if err := store.SaveSnapshot(record.TaskID, make([]byte, maxSnapshotBytes+1)); err == nil {
		t.Fatal("accepted oversize individual snapshot")
	}
	for n := 0; n < 3; n++ {
		other := runnerRecord()
		if err := store.Create(other); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSnapshot(other.TaskID, make([]byte, maxSnapshotBytes)); err != nil {
			t.Fatalf("snapshot %d: %v", n, err)
		}
	}
	full := runnerRecord()
	if err := store.Create(full); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(full.TaskID, make([]byte, maxSnapshotBytes)); err == nil {
		t.Fatal("accepted snapshot above total byte limit")
	}
}

func TestStoreExpiresSettledRecordsButKeepsActiveAndReleasesSnapshots(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store, err := Open(Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settled := runnerRecord()
	settled.Status = StatusRunning
	settled.ImageIDs = []string{"sha256:old"}
	if err := store.Create(settled); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(settled.TaskID, []byte("private")); err != nil {
		t.Fatal(err)
	}
	settled, err = store.Get(settled.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	settled.Status = StatusSucceeded
	settled.Recovery = Recovery{Status: "not-needed"}
	finished := now
	settled.FinishedAt = &finished
	if err := store.Save(settled); err != nil {
		t.Fatal(err)
	}
	active := runnerRecord()
	active.Status = StatusRunning
	active.ImageIDs = []string{"sha256:keep"}
	if err := store.Create(active); err != nil {
		t.Fatal(err)
	}
	now = now.Add(defaultRecordTTL + time.Second)
	if _, err := store.List(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(settled.TaskID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("expired terminal record remains: %v", err)
	}
	if _, err := store.ReadSnapshot(settled.TaskID); err == nil {
		t.Fatal("expired record snapshot was retained")
	}
	protected, err := store.ProtectedImageIDs()
	if err != nil || len(protected) != 1 || protected[0] != "sha256:keep" {
		t.Fatalf("active protection = %v, %v", protected, err)
	}
}

func TestStoreCapacityEvictsSettledAndRejectsWhenOnlyActiveRemain(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	base := time.Now().UTC()
	firstID := ""
	for n := 0; n < maxRecords; n++ {
		record := runnerRecord()
		record.Status = StatusSucceeded
		record.Recovery = Recovery{Status: "not-needed"}
		record.CreatedAt = base.Add(time.Duration(n) * time.Nanosecond)
		record.UpdatedAt = record.CreatedAt
		record.FinishedAt = &record.UpdatedAt
		if n == 0 {
			firstID = record.TaskID
		}
		if err := store.Create(record); err != nil {
			t.Fatalf("create terminal %d: %v", n, err)
		}
	}
	if err := store.Create(runnerRecord()); err != nil {
		t.Fatalf("eligible terminal capacity should make room: %v", err)
	}
	if _, err := store.Get(firstID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("oldest terminal not evicted: %v", err)
	}
	active := newRunnerMemoryStore()
	defer active.Close()
	for n := 0; n < maxRecords; n++ {
		record := runnerRecord()
		record.Status = StatusRunning
		if err := active.Create(record); err != nil {
			t.Fatalf("create active %d: %v", n, err)
		}
	}
	if err := active.Create(runnerRecord()); err == nil {
		t.Fatal("accepted after all record capacity was occupied by active work")
	}
}

func TestStoreEnforcesPerRecordAndAggregateByteLimits(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	oversize := runnerRecord()
	oversize.Warnings = []string{strings.Repeat("x", maxRecordBytes)}
	if err := store.Create(oversize); err == nil {
		t.Fatal("accepted a task record above the per-record limit")
	}
	payload := strings.Repeat("x", 900<<10)
	for n := 0; n < 37; n++ {
		record := runnerRecord()
		record.Status = StatusRunning
		record.Warnings = []string{payload}
		err := store.Create(record)
		if n < 36 && err != nil {
			t.Fatalf("create record %d below total byte limit: %v", n, err)
		}
		if n == 36 && err == nil {
			t.Fatal("accepted records above aggregate byte limit")
		}
	}
}

func TestRunnerRejectsWhenAllConcurrentSlotsAreBusy(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1000})
	entered, release := make(chan struct{}), make(chan struct{})
	if _, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { close(entered); <-release; return nil }}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return nil }}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full admission = %v", err)
	}
	close(release)
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerTracksSuccessFailureAndPanicInMemory(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 2})
	cases := []struct {
		name string
		fn   TaskFunc
		want Status
	}{
		{"success", func(context.Context, *Execution) error { return nil }, StatusSucceeded},
		{"failure", func(context.Context, *Execution) error { return errors.New("failed") }, StatusFailed},
		{"panic", func(context.Context, *Execution) error { panic("failed") }, StatusFailed},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			id, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: test.fn})
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
				t.Fatal(err)
			}
			record, err := store.Get(id)
			if err != nil || record.Status != test.want {
				t.Fatalf("record status = %s, %v; want %s", record.Status, err, test.want)
			}
		})
	}
}

func TestRunnerDoesNotRecoverAfterCommittedWorkAndCleanupError(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1})
	var recoveries atomic.Int32
	id, err := runner.Submit(TaskSpec{
		Record: runnerRecord(),
		Run: func(_ context.Context, execution *Execution) error {
			if err := execution.CriticalCommit("updated", nil); err != nil {
				return err
			}
			return errors.New("cleanup failed")
		},
		Recover: func(context.Context, error) (Recovery, error) {
			recoveries.Add(1)
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
	if err != nil || record.Status != StatusSucceeded || recoveries.Load() != 0 || len(record.Warnings) == 0 {
		t.Fatalf("committed task result=%#v recovery calls=%d error=%v", record, recoveries.Load(), err)
	}
}

func TestQueryCleanupUsesStoreRetentionWithoutHistoryDeletion(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	query, err := NewQueryService(store, QueryOptions{Retention: time.Nanosecond, MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, second := runnerRecord(), runnerRecord()
	first.Status, second.Status = StatusSucceeded, StatusSucceeded
	first.Recovery, second.Recovery = Recovery{Status: "not-needed"}, Recovery{Status: "not-needed"}
	if err := store.Create(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(second); err != nil {
		t.Fatal(err)
	}
	report, err := query.Cleanup()
	if err != nil || len(report.Deleted) != 0 || report.Retained != 2 {
		t.Fatalf("cleanup report = %#v, %v", report, err)
	}
	if _, err := store.Get(first.TaskID); err != nil {
		t.Fatalf("compatibility cleanup deleted in-memory progress: %v", err)
	}
}

func TestRunnerTimeoutWaitsForRunAndRecoveryBeforeSettling(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	settled := make(chan struct{})
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, TaskTimeout: 25 * time.Millisecond, RecoveryTimeout: time.Second})
	runEntered, allowRunReturn := make(chan struct{}), make(chan struct{})
	recoveryEntered, allowRecoveryReturn := make(chan struct{}), make(chan struct{})
	id, err := runner.Submit(TaskSpec{
		Record: runnerRecord(),
		Run: func(context.Context, *Execution) error {
			close(runEntered)
			<-allowRunReturn
			return errors.New("run ended after timeout")
		},
		Recover: func(context.Context, error) (Recovery, error) {
			close(recoveryEntered)
			<-allowRecoveryReturn
			return Recovery{Status: "restored"}, nil
		},
		OnSettled: func() { close(settled) },
	})
	if err != nil {
		t.Fatal(err)
	}
	<-runEntered
	deadline := time.Now().Add(time.Second)
	for {
		record, getErr := store.Get(id)
		if getErr == nil && record.Status == StatusRunning && record.Stage == "waiting-for-operation-exit" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout progress not visible: %#v, %v", record, getErr)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-recoveryEntered:
		t.Fatal("recovery started before Run returned")
	default:
	}
	if _, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return nil }}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("worker freed before Run returned: %v", err)
	}
	close(allowRunReturn)
	<-recoveryEntered
	if record, err := store.Get(id); err != nil || record.Status != StatusRunning {
		t.Fatalf("record settled before recovery returned: %#v, %v", record, err)
	}
	select {
	case <-settled:
		t.Fatal("settled before recovery returned")
	default:
	}
	if _, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return nil }}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("worker freed before recovery returned: %v", err)
	}
	close(allowRecoveryReturn)
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	<-settled
	record, err := store.Get(id)
	if err != nil || record.Status != StatusFailed || record.Recovery.Status != "restored" {
		t.Fatalf("final record = %#v, %v", record, err)
	}
}

func TestRunnerCloseCancelsActiveRunAndWaitsForRealReturn(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	runner, err := NewRunner(store, RunnerOptions{MaxConcurrent: 1, TaskTimeout: time.Hour, RecoveryTimeout: time.Second, Output: &strings.Builder{}})
	if err != nil {
		t.Fatal(err)
	}
	started, returned := make(chan struct{}), make(chan struct{})
	if _, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(ctx context.Context, _ *Execution) error {
		close(started)
		<-ctx.Done()
		close(returned)
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	<-started
	done := make(chan struct{})
	go func() { runner.Close(); close(done) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel active Run")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for Run settlement")
	}
}

func TestRunnerBatchUsesSingleExecutionAndKeepsCommittedItem(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1})
	var calls atomic.Int32
	operations := []BatchOperation{
		{Item: Item{ID: "a", Name: "alpha"}, Run: func(_ context.Context, e *Execution) error {
			calls.Add(1)
			return e.CriticalItemCommit(0, "committed", func(r *Record) { r.Items[0].NewContainerID = "new-a" })
		}},
		{Item: Item{ID: "b", Name: "beta"}, Run: func(context.Context, *Execution) error { calls.Add(1); return errors.New("second failed") }, Recover: func(context.Context, error) (Recovery, error) { return Recovery{Status: "restored"}, nil }},
	}
	id, err := runner.SubmitBatch(runnerRecord(), operations, nil)
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
	if calls.Load() != 2 || record.Status != StatusFailed || record.Items[0].Status != "succeeded" || record.Items[1].Recovery.Status != "restored" {
		t.Fatalf("batch calls=%d record=%#v", calls.Load(), record)
	}
}

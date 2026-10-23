package task

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatchRecoveryTimeoutWaitsForCallbackAndStopsLaterItems(t *testing.T) {
	store := newRunnerMemoryStore()
	defer store.Close()
	runner := newTestRunner(t, store, RunnerOptions{
		MaxConcurrent: 1, TaskTimeout: time.Second, RecoveryTimeout: 35 * time.Millisecond,
	})
	recoveryEntered := make(chan struct{})
	releaseRecovery := make(chan struct{})
	settled := make(chan struct{})
	var secondItemRuns atomic.Int32

	operations := []BatchOperation{
		{
			Item: Item{ID: "first", Name: "first"},
			Run:  func(context.Context, *Execution) error { return errors.New("first item failed") },
			Recover: func(context.Context, error) (Recovery, error) {
				close(recoveryEntered)
				<-releaseRecovery // Deliberately ignore timeout cancellation.
				return Recovery{Status: "restored"}, nil
			},
		},
		{
			Item: Item{ID: "second", Name: "second"},
			Run: func(context.Context, *Execution) error {
				secondItemRuns.Add(1)
				return nil
			},
		},
	}
	parentRecovery := func(context.Context, error) (Recovery, error) {
		return Recovery{Status: "unknown", Detail: "item recovery timed out"}, nil
	}
	id, err := runner.SubmitBatchWithHooks(runnerRecord(), operations, parentRecovery, nil, func() {
		close(settled)
	})
	if err != nil {
		t.Fatal(err)
	}
	<-recoveryEntered

	deadline := time.Now().Add(time.Second)
	for {
		record, getErr := store.Get(id)
		if getErr != nil {
			t.Fatalf("get task while recovery is blocked: %v", getErr)
		}
		if record.Status == StatusRunning && len(record.Items) == 2 && record.Items[0].Recovery.Status == "unknown" {
			if record.Items[1].Status != "queued" {
				t.Fatalf("later item advanced before recovery returned: %#v", record.Items[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("item recovery timeout was not recorded while callback remained blocked: %#v", record)
		}
		time.Sleep(time.Millisecond)
	}

	select {
	case <-settled:
		t.Fatal("task settled before its non-cooperative recovery callback returned")
	default:
	}
	if _, err := runner.Submit(TaskSpec{Record: runnerRecord(), Run: func(context.Context, *Execution) error { return nil }}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("worker slot was released while recovery callback was blocked: %v", err)
	}
	if got := secondItemRuns.Load(); got != 0 {
		t.Fatalf("second item ran before first item recovery returned: runs=%d", got)
	}

	close(releaseRecovery)
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	<-settled
	if got := secondItemRuns.Load(); got != 0 {
		t.Fatalf("second item ran after first item recovery timed out: runs=%d", got)
	}
	record, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusFailed || record.Recovery.Status != "unknown" || record.Items[0].Recovery.Status != "unknown" || record.Items[1].Status != "skipped" {
		t.Fatalf("unexpected final timed-out batch state: %#v", record)
	}
}

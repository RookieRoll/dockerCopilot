package operation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMutationGateDrainWaitsForOtherOwnersAndRejectsNewWork(t *testing.T) {
	gate := NewMutationGate()
	program, err := gate.Acquire(context.Background(), "program-task")
	if err != nil {
		t.Fatal(err)
	}
	other, err := gate.Acquire(context.Background(), "container-task")
	if err != nil {
		t.Fatal(err)
	}
	drain, err := gate.BeginDrain("program-task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Acquire(context.Background(), "new-request"); !errors.Is(err, ErrMutationDrain) {
		t.Fatalf("new mutation should be rejected during drain, got %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- drain.WaitOthers(context.Background()) }()
	select {
	case err := <-waitDone:
		t.Fatalf("drain completed while another owner was active: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	other.Release()
	if err := <-waitDone; err != nil {
		t.Fatal(err)
	}
	program.Release()
	if !gate.IsDraining() {
		t.Fatal("gate opened before handoff was resumed")
	}
	drain.Resume()
	if gate.IsDraining() {
		t.Fatal("Resume did not reopen the gate")
	}
	lease, err := gate.Acquire(context.Background(), "next-request")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestMutationGateCanceledDrainCanBeResumed(t *testing.T) {
	gate := NewMutationGate()
	other, err := gate.Acquire(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	drain, err := gate.BeginDrain("program")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := drain.WaitOthers(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected bounded wait deadline, got %v", err)
	}
	drain.Resume()
	if gate.IsDraining() {
		t.Fatal("failed handoff did not reopen mutations")
	}
	other.Release()
}

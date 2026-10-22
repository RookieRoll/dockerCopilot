package operation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReserveBatchContextCreatesSingleParentLeaseAcrossTargets(t *testing.T) {
	c := NewCoordinator()
	reservation, err := c.ReserveBatchContext(context.Background(), Request{TaskID: "batch-parent", Kind: "container-batch-restart"}, []Target{{ID: "id-1", Name: "one"}, {ID: "id-2", Name: "two"}, {ID: "id-1", Name: "one"}})
	if err != nil || reservation.Lease == nil {
		t.Fatalf("batch reservation=%+v err=%v", reservation, err)
	}
	if err := reservation.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []Target{{ID: "id-1", Name: "one"}, {ID: "id-2", Name: "two"}} {
		if !c.Owns("batch-parent", target) {
			t.Fatalf("batch parent does not own %#v", target)
		}
	}
	if _, err := c.Reserve(Request{TaskID: "other", Kind: "container-stop", Target: Target{ID: "id-2"}}); err == nil {
		t.Fatal("expected conflict while batch lease is active")
	} else {
		var conflictErr *ConflictError
		if !errors.As(err, &conflictErr) || conflictErr.TaskID != "batch-parent" {
			t.Fatalf("conflict should point to parent task, got %v", err)
		}
	}
	reservation.Lease.Release()
	if c.Owns("batch-parent", Target{ID: "id-1"}) || c.Owns("batch-parent", Target{ID: "id-2"}) {
		t.Fatal("release did not release every batch target")
	}
}

func TestReserveBatchContextDoesNotPartiallyAcquireOnConflict(t *testing.T) {
	c := NewCoordinator()
	owner, err := c.Reserve(Request{TaskID: "owner", Kind: "container-stop", Target: Target{ID: "occupied", Name: "busy"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	_, err = c.ReserveBatchContext(context.Background(), Request{TaskID: "batch", Kind: "container-batch-start"}, []Target{{ID: "free", Name: "free-name"}, {ID: "occupied", Name: "busy"}})
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) || conflictErr.TaskID != "owner" {
		t.Fatalf("batch conflict=%v", err)
	}
	if c.Owns("batch", Target{ID: "free", Name: "free-name"}) {
		t.Fatal("failed batch reservation partially acquired an unconflicted target")
	}
}

func TestReserveAvailableBatchContextReturnsPerTargetConflictAndReservesOthers(t *testing.T) {
	c := NewCoordinator()
	owner, err := c.Reserve(Request{TaskID: "busy-task", Kind: "container-stop", Target: Target{ID: "busy", Name: "busy-name"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	reservation, err := c.ReserveAvailableBatchContext(context.Background(), Request{TaskID: "batch", Kind: "container-batch-start"}, []Target{{ID: "free", Name: "free-name"}, {ID: "busy", Name: "busy-name"}})
	if err != nil || reservation.Lease == nil || len(reservation.Conflicts) != 1 {
		t.Fatalf("partial reservation=%+v err=%v", reservation, err)
	}
	if reservation.Conflicts[0].TaskID != "busy-task" || reservation.Conflicts[0].Target.ID != "busy" {
		t.Fatalf("conflict details=%+v", reservation.Conflicts)
	}
	if !c.Owns("batch", Target{ID: "free", Name: "free-name"}) || c.Owns("batch", Target{ID: "busy", Name: "busy-name"}) {
		t.Fatal("partial lease did not protect exactly its available targets")
	}
}

func TestReserveDeduplicatesEquivalentQueuedUpdateAndConflictsOtherwise(t *testing.T) {
	c := NewCoordinator()
	first, err := c.Reserve(Request{TaskID: "task-a", Kind: ContainerUpdate, Target: Target{ID: "abcdef", Name: "/web"}, EquivalenceKey: "nginx|delete=true"})
	if err != nil || first.Lease == nil {
		t.Fatalf("first reservation=%+v err=%v", first, err)
	}
	if err := first.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	duplicate, err := c.Reserve(Request{TaskID: "task-b", Kind: ContainerUpdate, Target: Target{ID: "ABCDEF", Name: "web"}, EquivalenceKey: "nginx|delete=true"})
	if err != nil || duplicate.Lease != nil || duplicate.ExistingTaskID != "task-a" {
		t.Fatalf("duplicate reservation=%+v err=%v", duplicate, err)
	}
	_, err = c.Reserve(Request{TaskID: "task-c", Kind: ContainerUpdate, Target: Target{ID: "abcdef", Name: "web"}, EquivalenceKey: "nginx|delete=false"})
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) || conflictErr.TaskID != "task-a" || conflictErr.HTTPStatus() != 409 {
		t.Fatalf("expected task conflict, got %v", err)
	}
	_, err = c.Reserve(Request{TaskID: "task-d", Kind: "container-stop", Target: Target{ID: "abcdef"}})
	if !errors.As(err, &conflictErr) || conflictErr.TaskID != "task-a" {
		t.Fatalf("expected non-equivalent operation conflict, got %v", err)
	}
}

func TestContainerNamesRemainCaseSensitive(t *testing.T) {
	c := NewCoordinator()
	lower, err := c.Reserve(Request{TaskID: "lower", Kind: "container-stop", Target: Target{Name: "web"}})
	if err != nil {
		t.Fatal(err)
	}
	upper, err := c.Reserve(Request{TaskID: "upper", Kind: "container-stop", Target: Target{Name: "WEB"}})
	if err != nil {
		t.Fatalf("Docker names are case-sensitive; distinct name was rejected: %v", err)
	}
	lower.Lease.Release()
	upper.Lease.Release()
}

func TestAliasesAreOwnerScopedAndReleaseClearsOnlyOwnedIdentities(t *testing.T) {
	c := NewCoordinator()
	lease, err := c.Reserve(Request{TaskID: "update", Kind: ContainerUpdate, Target: Target{ID: "old-id", Name: "web"}, EquivalenceKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Lease.AddAlias(Target{ID: "new-id", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if !c.Owns("update", Target{ID: "new-id", Name: "web"}) {
		t.Fatal("new ID alias was not attached to owner")
	}
	if err := c.AddAlias("other-task", Target{ID: "external-id"}); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("non-owner alias result: %v", err)
	}
	other, err := c.Reserve(Request{TaskID: "other", Kind: "container-stop", Target: Target{ID: "external-id", Name: "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Lease.AddAlias(Target{ID: "external-id"}); err == nil {
		t.Fatal("alias collision should be rejected")
	}
	lease.Lease.Release()
	lease.Lease.Release()
	if _, err := c.Reserve(Request{TaskID: "after", Kind: "container-stop", Target: Target{ID: "new-id", Name: "web"}}); err != nil {
		t.Fatalf("released alias still protected: %v", err)
	}
	if !c.Owns("other", Target{ID: "external-id", Name: "other"}) {
		t.Fatal("releasing one lease removed another owner's identities")
	}
	other.Lease.Release()
}

func TestAliasesCannotSplitAnExistingTargetAcrossOwners(t *testing.T) {
	c := NewCoordinator()
	first, err := c.Reserve(Request{TaskID: "first", Kind: "container-stop", Target: Target{ID: "id-1", Name: "name-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	second, err := c.Reserve(Request{TaskID: "second", Kind: "container-start", Target: Target{ID: "id-2", Name: "name-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	if err := first.Lease.AddAlias(Target{ID: "id-2", Name: "name-1"}); err == nil {
		t.Fatal("alias overlapping another owner's ID should fail")
	}
	if !c.Owns("first", Target{ID: "id-1", Name: "name-1"}) || !c.Owns("second", Target{ID: "id-2", Name: "name-2"}) {
		t.Fatal("failed alias insertion changed current ownership")
	}
	first.Lease.Release()
	second.Lease.Release()
}

func TestProvisionalUpdateWaitsForSubmitDecision(t *testing.T) {
	c := NewCoordinator()
	first, err := c.Reserve(Request{TaskID: "first", Kind: ContainerUpdate, Target: Target{ID: "id", Name: "web"}, EquivalenceKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan Reservation, 1)
	errResult := make(chan error, 1)
	go func() {
		reservation, reserveErr := c.Reserve(Request{TaskID: "second", Kind: ContainerUpdate, Target: Target{ID: "id", Name: "web"}, EquivalenceKey: "same"})
		result <- reservation
		errResult <- reserveErr
	}()
	select {
	case <-result:
		t.Fatal("duplicate returned before task acceptance was known")
	default:
	}
	if err := first.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if err := <-errResult; err != nil || got.ExistingTaskID != "first" || got.Lease != nil {
			t.Fatalf("duplicate result=%+v err=%v", got, err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate did not observe confirmed task")
	}
	first.Lease.Release()
}

func TestProvisionalSubmitFailureLetsWaiterReserveInstead(t *testing.T) {
	c := NewCoordinator()
	first, err := c.Reserve(Request{TaskID: "failed", Kind: ContainerUpdate, Target: Target{ID: "id", Name: "web"}, EquivalenceKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan Reservation, 1)
	errResult := make(chan error, 1)
	go func() {
		reservation, reserveErr := c.Reserve(Request{TaskID: "retry", Kind: ContainerUpdate, Target: Target{ID: "id", Name: "web"}, EquivalenceKey: "same"})
		result <- reservation
		errResult <- reserveErr
	}()
	first.Lease.Release()
	select {
	case got := <-result:
		if err := <-errResult; err != nil || got.Lease == nil || got.ExistingTaskID != "" {
			t.Fatalf("retry reservation=%+v err=%v", got, err)
		}
		got.Lease.Release()
	case <-time.After(time.Second):
		t.Fatal("waiter did not reserve after failed submit was released")
	}
}

func TestReserveContextCancelsWhileWaitingForProvisionalDecision(t *testing.T) {
	c := NewCoordinator()
	first, err := c.Reserve(Request{TaskID: "first", Kind: ContainerUpdate, Target: Target{ID: "id", Name: "web"}, EquivalenceKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := c.ReserveContext(ctx, Request{TaskID: "second", Kind: ContainerUpdate, Target: Target{ID: "id", Name: "web"}, EquivalenceKey: "same"})
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReserveContext error=%v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("provisional waiter ignored context cancellation")
	}
	first.Lease.Release()
}

package operation

import (
	"testing"
)

func TestMutationRevisionDetectsCompletedLeaseBetweenSnapshots(t *testing.T) {
	coordinator := NewCoordinator()
	before := coordinator.CaptureVersion()
	reservation, err := coordinator.Reserve(Request{TaskID: "rename-task", Kind: "container-rename", Target: Target{ID: "id-1", Name: "before"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Lease.AddAlias(Target{Name: "after"}); err != nil {
		t.Fatal(err)
	}
	reservation.Lease.Release()

	committed := false
	stable, err := coordinator.WithStableVersion(before, func() error {
		committed = true
		return nil
	})
	if err != nil || stable || committed {
		t.Fatalf("stale snapshot committed: stable=%v committed=%v err=%v", stable, committed, err)
	}

	current := coordinator.CaptureVersion()
	stable, err = coordinator.WithStableVersion(current, func() error {
		committed = true
		return nil
	})
	if err != nil || !stable || !committed {
		t.Fatalf("stable snapshot did not commit: stable=%v committed=%v err=%v", stable, committed, err)
	}
}

func TestStableCommitWaitsUntilActiveMutationsSettle(t *testing.T) {
	coordinator := NewCoordinator()
	version := coordinator.CaptureVersion()
	reservation, err := coordinator.Reserve(Request{TaskID: "active-task", Kind: "container-rename", Target: Target{Name: "web"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	called := false
	stable, err := coordinator.WithStableVersion(coordinator.CaptureVersion(), func() error {
		called = true
		return nil
	})
	if err != nil || stable || called {
		t.Fatalf("active mutation must prevent cleanup commit: stable=%v called=%v err=%v", stable, called, err)
	}
	reservation.Lease.Release()
	stable, err = coordinator.WithStableVersion(version, func() error {
		called = true
		return nil
	})
	if err != nil || stable || called {
		t.Fatalf("lease lifecycle change should stale earlier snapshot: stable=%v called=%v err=%v", stable, called, err)
	}
}

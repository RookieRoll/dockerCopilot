package task

import (
	"context"
	"testing"
)

// TestStartupReconcilePreservesUncertainCommitAttention simulates a restart after
// an uncertain directory-sync rollback left a commit candidate visible.
func TestStartupReconcilePreservesUncertainCommitAttention(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent := startupRecord("00000000-0000-4000-8000-000000000092", StatusRunning)
	parent.Type = "container-batch-update"
	parent.RequiresAttention = true
	parent.Recovery = Recovery{Status: "unknown", Detail: "commit write outcome is uncertain"}
	parent.ProtectedNames = []string{"web"}
	parent.Items = []Item{
		{
			ID: "old-id", Name: "web", Status: "running", Committed: true,
			NewContainerID: "replacement-id", ProtectedNames: []string{"web"},
			RequiresAttention: true, Recovery: Recovery{Status: "unknown", Detail: "commit write outcome is uncertain"},
		},
		{ID: "old-durable", Name: "durable", Status: "running", Committed: true, NewContainerID: "new-durable"},
		{ID: "queued", Name: "not-started", Status: "queued"},
	}
	if err := store.Create(parent); err != nil {
		t.Fatal(err)
	}
	inspector := &readonlyInspectorFake{observations: map[string]ContainerObservation{
		"old-id":         {Name: "web", State: "exited", Running: false},
		"replacement-id": {Name: "web", State: "running", Running: true},
		"old-durable":    {Name: "durable", State: "exited", Running: false},
		"new-durable":    {Name: "durable", State: "running", Running: true},
	}}
	reconciler, err := NewStartupReconciler(store, inspector, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(parent.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInterrupted || got.Items[0].Status != "failed" || got.Items[0].Committed {
		t.Fatalf("uncertain commit was incorrectly promoted at restart: %#v", got)
	}
	if !got.RequiresAttention || !got.Items[0].RequiresAttention || got.Items[0].Recovery.Status != "unknown" || len(got.Items[0].ProtectedNames) != 1 || got.Items[0].ProtectedNames[0] != "web" {
		t.Fatalf("uncertain commit was incorrectly cleared at restart: %#v", got)
	}
	if cleanupEligible(got) {
		t.Fatal("record with uncertain committed item became cleanup-eligible")
	}
	if got.Items[1].Status != "succeeded" || !got.Items[1].Committed || got.Items[1].RequiresAttention {
		t.Fatalf("durable committed item did not retain success: %#v", got.Items[1])
	}
	if got.Items[2].Status != "skipped" || got.SuccessCount != 1 || got.FailedCount != 1 || got.SkippedCount != 1 {
		t.Fatalf("startup batch status/count normalization incorrect: %#v counts=%d/%d/%d", got.Items[2], got.SuccessCount, got.FailedCount, got.SkippedCount)
	}
}

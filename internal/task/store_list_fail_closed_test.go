package task

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestStoreListFailsClosedForCorruptAndQuarantinedRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	store, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	badID := uuid.NewString()
	if err := os.WriteFile(filepath.Join(dir, badID+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil {
		t.Fatal("List must fail instead of returning an incomplete task set")
	}
	if _, err := store.ProtectedImages(); err == nil {
		t.Fatal("image protection must fail closed when a task record is corrupt")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening quarantines the corrupt file. The quarantine itself must remain
	// an unresolved protection signal across restarts, rather than making the
	// next List appear complete just because the source file moved.
	store, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.List(); err == nil {
		t.Fatal("List must fail while quarantined task records remain unresolved")
	}
	if _, err := store.ProtectedImages(); err == nil {
		t.Fatal("image protection must fail while quarantined task records remain unresolved")
	}
	reconciler, err := NewStartupReconciler(store, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Run(context.Background()); err == nil {
		t.Fatal("startup reconciliation must refuse an incomplete task record set")
	}
}

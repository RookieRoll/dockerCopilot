package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestStableCleanupPreservesNewPortWritesAndProtectedNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ports.json")
	service := NewPortOverridesService(path)
	if err := service.Save("newly-configured", []types.ConfiguredPort{{Port: 8443, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	staleSnapshotGeneration := service.Generation()
	if err := service.Save("newly-configured", []types.ConfiguredPort{{Port: 9443, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	removed, _, stable, err := service.CleanupStable(nil, nil, staleSnapshotGeneration)
	if err != nil || stable || removed != 0 {
		t.Fatalf("stale list should be rejected: removed=%d stable=%v err=%v", removed, stable, err)
	}

	removed, _, err = service.CleanupProtected(nil, []string{"newly-configured"})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("unexpected removed count while protected: %d", removed)
	}
	entries, err := service.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := entries["newly-configured"]; len(got) != 1 || got[0].Port != 9443 {
		t.Fatalf("protected port mapping was lost: %#v", entries)
	}

	removed, entries, stable, err = service.CleanupStable(nil, nil, service.Generation())
	if err != nil || !stable || removed != 1 {
		t.Fatalf("stable cleanup = removed %d stable %v err %v", removed, stable, err)
	}
	if _, exists := entries["newly-configured"]; exists {
		t.Fatalf("unprotected orphan remained: %#v", entries)
	}
}

func TestPortOverrideMigrationMovesEntryAndAdvancesGeneration(t *testing.T) {
	service := NewPortOverridesService(filepath.Join(t.TempDir(), "ports.json"))
	if err := service.Save("before", []types.ConfiguredPort{{Port: 8443, Protocol: "tcp", Label: "https"}}); err != nil {
		t.Fatal(err)
	}
	before := service.Generation()
	if err := service.Migrate("before", "after"); err != nil {
		t.Fatal(err)
	}
	if service.Generation() <= before {
		t.Fatalf("migration did not advance generation: before=%d after=%d", before, service.Generation())
	}
	entries, err := service.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := entries["before"]; exists {
		t.Fatalf("old name mapping remained after migration: %#v", entries)
	}
	if got := entries["after"]; len(got) != 1 || got[0].Port != 8443 || got[0].Label != "https" {
		t.Fatalf("new name mapping was not preserved: %#v", entries)
	}
}

func TestPendingRenameSurvivesRestartAndRequiresMatchingContainerIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ports.json")
	first := NewPortOverridesService(path)
	if err := first.Save("before", []types.ConfiguredPort{{Port: 8443, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	if err := first.BeginRename("before", "after", "container-original"); err != nil {
		t.Fatal(err)
	}

	// A fresh service instance simulates a process restart. A different
	// container that happens to own the target name must not receive the ports.
	afterRestart := NewPortOverridesService(path)
	_, _, stable, err := afterRestart.CleanupStableWithContainers([]ContainerPortIdentity{{ID: "container-other", Name: "after"}}, nil, afterRestart.Generation())
	if err != nil || !stable {
		t.Fatalf("cleanup with another container identity: stable=%v err=%v", stable, err)
	}
	entries, err := afterRestart.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["before"]) != 1 || len(entries["after"]) != 0 {
		t.Fatalf("rename mapping was misapplied to another container: %#v", entries)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		RenameIntents map[string]ContainerPortRenameIntent `json:"renameIntents"`
	}
	if err := json.Unmarshal(content, &persisted); err != nil {
		t.Fatal(err)
	}
	if intent := persisted.RenameIntents["before"]; intent.NewName != "after" || intent.ContainerID != "container-original" {
		t.Fatalf("pending rename evidence was not retained: %#v", persisted.RenameIntents)
	}

	_, _, stable, err = afterRestart.CleanupStableWithContainers([]ContainerPortIdentity{{ID: "container-original", Name: "after"}}, nil, afterRestart.Generation())
	if err != nil || !stable {
		t.Fatalf("cleanup with matching renamed container: stable=%v err=%v", stable, err)
	}
	entries, err = afterRestart.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["before"]) != 0 || len(entries["after"]) != 1 || entries["after"][0].Port != 8443 {
		t.Fatalf("matching rename was not recovered: %#v", entries)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted.RenameIntents = nil
	if err := json.Unmarshal(content, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.RenameIntents) != 0 {
		t.Fatalf("completed intent remained after recovery: %#v", persisted.RenameIntents)
	}
}

func TestRenameCommitWriteFailurePreservesIntentUntilSameContainerRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ports.json")
	ports := NewPortOverridesService(path)
	if err := ports.Save("before", []types.ConfiguredPort{{Port: 8443, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	if err := ports.BeginRename("before", "after", "container-original"); err != nil {
		t.Fatal(err)
	}

	// Simulate the durable settings write failing after Docker rename succeeds.
	faulted := NewPortOverridesService(path)
	faulted.writeFile = func(string, containerPortOverridesFile) error { return errors.New("disk full (injected)") }
	if err := faulted.CompleteRename("before", "after"); err == nil {
		t.Fatal("expected injected atomic-write failure")
	}

	// A restarted process sees a foreign container with the target name. The
	// old mapping and recovery marker must remain until the original ID returns.
	restarted := NewPortOverridesService(path)
	_, _, stable, err := restarted.CleanupStableWithContainers([]ContainerPortIdentity{{ID: "foreign", Name: "after"}}, nil, restarted.Generation())
	if err != nil || !stable {
		t.Fatalf("foreign identity cleanup: stable=%v err=%v", stable, err)
	}
	entries, err := restarted.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["before"]) != 1 || entries["before"][0].Port != 8443 || len(entries["after"]) != 0 {
		t.Fatalf("failed write changed persisted mappings: %#v", entries)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		RenameIntents map[string]ContainerPortRenameIntent `json:"renameIntents"`
	}
	if err := json.Unmarshal(content, &persisted); err != nil {
		t.Fatal(err)
	}
	if intent := persisted.RenameIntents["before"]; intent.ContainerID != "container-original" || intent.NewName != "after" {
		t.Fatalf("failed write lost recovery intent: %#v", persisted.RenameIntents)
	}

	// Restore the normal atomic writer. The matching ID in the complete list
	// lets cleanup finish the rename migration and remove its marker.
	_, _, stable, err = restarted.CleanupStableWithContainers([]ContainerPortIdentity{{ID: "container-original", Name: "after"}}, nil, restarted.Generation())
	if err != nil || !stable {
		t.Fatalf("matching identity recovery: stable=%v err=%v", stable, err)
	}
	entries, err = restarted.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["before"]) != 0 || len(entries["after"]) != 1 || entries["after"][0].Port != 8443 {
		t.Fatalf("matching identity did not complete migration: %#v", entries)
	}
}

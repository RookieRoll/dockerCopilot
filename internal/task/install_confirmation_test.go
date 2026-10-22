package task

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConfirmPendingInstallRequiresReadyMatchingProofAndIsIdempotent(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 10, 22, 1, 2, 3, 0, time.UTC)
	record := startupRecord("00000000-0000-4000-8000-000000000071", StatusRunning)
	record.Type = TaskTypeProgramUpdate
	record.PendingInstall = &PendingInstallHandoff{
		HandoffID: "10000000-0000-4000-8000-000000000071", TargetVersion: "v2.3.4", GOARCH: "arm64",
		ArtifactSHA256: strings.Repeat("a", 64), PreparedAt: now.Add(-time.Minute),
	}
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	proof := PendingInstallProof{
		Verified: true, Ready: true, TaskID: record.TaskID, HandoffID: record.PendingInstall.HandoffID,
		TargetVersion: record.PendingInstall.TargetVersion, GOARCH: record.PendingInstall.GOARCH,
		ArtifactSHA256: record.PendingInstall.ArtifactSHA256,
	}
	preReady := proof
	preReady.Ready = false
	if err := store.ConfirmPendingInstall(record.TaskID, preReady); !errors.Is(err, ErrPendingInstallProofMismatch) {
		t.Fatalf("pre-readiness proof error=%v", err)
	}
	wrongArch := proof
	wrongArch.GOARCH = "amd64"
	if err := store.ConfirmPendingInstall(record.TaskID, wrongArch); !errors.Is(err, ErrPendingInstallProofMismatch) {
		t.Fatalf("wrong-architecture proof error=%v", err)
	}
	if err := store.ConfirmPendingInstall(record.TaskID, proof); err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.Get(record.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != StatusSucceeded || !confirmed.IsDone || confirmed.FinishedAt == nil || confirmed.Stage != "install-confirmed" || confirmed.PendingInstall == nil {
		t.Fatalf("confirmed task state = %#v", confirmed)
	}
	if err := store.ConfirmPendingInstall(record.TaskID, proof); err != nil {
		t.Fatalf("identical confirmation was not idempotent: %v", err)
	}
	wrongDigest := proof
	wrongDigest.ArtifactSHA256 = strings.Repeat("b", 64)
	if err := store.ConfirmPendingInstall(record.TaskID, wrongDigest); !errors.Is(err, ErrPendingInstallProofMismatch) {
		t.Fatalf("mismatched repeat confirmation error=%v", err)
	}
}

func TestConfirmPendingInstallCannotPromoteFailedOrInterruptedTask(t *testing.T) {
	for _, status := range []Status{StatusFailed, StatusInterrupted} {
		t.Run(string(status), func(t *testing.T) {
			store, err := Open(Options{Dir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			record := startupRecord("00000000-0000-4000-8000-000000000072", status)
			record.Type = TaskTypeProgramUpdate
			record.PendingInstall = &PendingInstallHandoff{
				HandoffID: "10000000-0000-4000-8000-000000000072", TargetVersion: "2.3.4", GOARCH: "amd64",
				ArtifactSHA256: strings.Repeat("c", 64), PreparedAt: time.Now().UTC(),
			}
			if err := store.Create(record); err != nil {
				t.Fatal(err)
			}
			proof := PendingInstallProof{Verified: true, Ready: true, TaskID: record.TaskID, HandoffID: record.PendingInstall.HandoffID, TargetVersion: record.PendingInstall.TargetVersion, GOARCH: record.PendingInstall.GOARCH, ArtifactSHA256: record.PendingInstall.ArtifactSHA256}
			if err := store.ConfirmPendingInstall(record.TaskID, proof); err == nil {
				t.Fatal("terminal task was promoted to success")
			}
			got, err := store.Get(record.TaskID)
			if err != nil || got.Status != status {
				t.Fatalf("terminal state changed: status=%q err=%v", got.Status, err)
			}
		})
	}
}

package task

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStoreCRUDPermissionsAndPublicProjection(t *testing.T) {
	dir := t.TempDir() + "/tasks"
	warnings := []string{}
	s, err := Open(Options{Dir: dir, Warnf: func(format string, args ...any) { warnings = append(warnings, format) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(r.TaskID, []byte(`{"Env":["TOKEN=private"]}`)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued || got.SchemaVersion != 1 {
		t.Fatalf("unexpected record: %#v", got)
	}
	pub, err := json.Marshal(Public(got))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"TOKEN=private", "snapshotRef", "privatePath"} {
		if strings.Contains(string(pub), secret) {
			t.Fatalf("public record leaked %q: %s", secret, pub)
		}
	}
	if !strings.Contains(string(pub), `"isDone":false`) {
		t.Fatalf("legacy isDone projection missing: %s", pub)
	}
	if runtimeMode, ok := os.Stat(dir); ok != nil {
		t.Fatal(ok)
	} else if runtimeMode.IsDir() && runtimeMode.Mode().Perm()&0077 != 0 && os.PathSeparator != '\\' {
		t.Fatalf("directory permissions are too broad: %o", runtimeMode.Mode().Perm())
	}
	file, err := os.Stat(filepath.Join(dir, r.TaskID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator != '\\' && file.Mode().Perm() != 0600 {
		t.Fatalf("record mode = %o, want 600", file.Mode().Perm())
	}
	if err := s.Delete(r.TaskID); err == nil {
		t.Fatal("active record was deleted")
	}
}

func TestStoreProtectedImageProjectionTracksUnresolvedTasksAndFailsClosed(t *testing.T) {
	s, err := Open(Options{Dir: filepath.Join(t.TempDir(), "tasks")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	active := sampleRecord()
	active.ImageIDs = []string{"sha256:active", "sha256:shared", "sha256:active"}
	active.Items = []Item{{ID: "container-1", ImageIDs: []string{"sha256:item"}}}
	if err := s.Create(active); err != nil {
		t.Fatal(err)
	}

	needsReview := sampleRecord()
	needsReview.TaskID = uuid.NewString()
	needsReview.SnapshotRef = needsReview.TaskID + ".snapshot"
	needsReview.Status = StatusQueued
	needsReview.ImageIDs = []string{"sha256:recovery", "sha256:shared"}
	if err := s.Create(needsReview); err != nil {
		t.Fatal(err)
	}
	needsReview, err = s.Get(needsReview.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	needsReview.Status, needsReview.StartedAt = StatusRunning, &started
	if err := s.Save(needsReview); err != nil {
		t.Fatal(err)
	}
	finished := started.Add(time.Second)
	needsReview.Status, needsReview.FinishedAt, needsReview.UpdatedAt = StatusFailed, &finished, finished
	needsReview.RequiresAttention = true
	needsReview.Recovery = Recovery{Status: "unknown", Detail: "manual check"}
	needsReview.ImageIDs = []string{"sha256:recovery", "sha256:shared"}
	if err := s.Save(needsReview); err != nil {
		t.Fatal(err)
	}

	resolved := sampleRecord()
	resolved.TaskID = uuid.NewString()
	resolved.SnapshotRef = resolved.TaskID + ".snapshot"
	resolved.ImageIDs = []string{"sha256:released"}
	resolved.ProtectedNames = []string{"old-container-name"}
	resolved.Items = []Item{{ID: "container-id", ImageIDs: []string{"sha256:item-released"}, ProtectedNames: []string{"item-old-name"}, Recovery: Recovery{Status: "not-needed"}}}
	if err := s.Create(resolved); err != nil {
		t.Fatal(err)
	}
	resolved, err = s.Get(resolved.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	resolved.Status = StatusRunning
	if err := s.Save(resolved); err != nil {
		t.Fatal(err)
	}
	finished = time.Now().UTC()
	resolved.Status, resolved.FinishedAt, resolved.UpdatedAt = StatusSucceeded, &finished, finished
	resolved.Recovery = Recovery{Status: "not-needed"}
	if err := s.Save(resolved); err != nil {
		t.Fatal(err)
	}

	protected, err := s.ProtectedImages()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		active.TaskID + ":sha256:active":        "",
		active.TaskID + ":sha256:shared":        "",
		active.TaskID + ":sha256:item":          "",
		needsReview.TaskID + ":sha256:shared":   "",
		needsReview.TaskID + ":sha256:recovery": "",
	}
	for _, image := range protected {
		delete(want, image.TaskID+":"+image.ImageID)
		if image.ImageID == "sha256:released" {
			t.Fatal("resolved failure still protects its image")
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing protected images: %#v; got %#v", want, protected)
	}
	ids, err := s.ProtectedImageIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 4 || ids[0] != "sha256:active" || ids[1] != "sha256:item" || ids[2] != "sha256:recovery" || ids[3] != "sha256:shared" {
		t.Fatalf("unexpected sorted protected image IDs: %#v", ids)
	}

	if err := os.WriteFile(filepath.Join(s.dir, uuid.NewString()+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProtectedImages(); err == nil {
		t.Fatal("corrupt task record must fail closed for image protection")
	}
}

func TestStoreRecoveryCheckpointPreservesTerminalOutcome(t *testing.T) {
	s, err := Open(Options{Dir: filepath.Join(t.TempDir(), "tasks")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	r, err = s.Get(r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	r.Status, r.StartedAt = StatusRunning, &started
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	finished := started.Add(time.Second)
	r.Status, r.FinishedAt, r.UpdatedAt = StatusFailed, &finished, finished
	r.Error = "operation failed"
	r.Recovery = Recovery{Status: "unknown", Detail: "recovery pending"}
	r.RequiresAttention = true
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r, err = s.Get(r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	r.Stage = "recovering-old"
	r.NewContainerID = "new-confirmed"
	r.ContainerIDs = append(r.ContainerIDs, "new-confirmed")
	if err := s.saveRecoveryCheckpoint(r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.Error != "operation failed" || got.FinishedAt == nil || !got.FinishedAt.Equal(finished) || got.Stage != "recovering-old" || got.NewContainerID != "new-confirmed" {
		t.Fatalf("recovery checkpoint changed or lost task facts: %#v", got)
	}
	bad := got
	bad.Error = "rewritten result"
	if err := s.saveRecoveryCheckpoint(bad); err == nil {
		t.Fatal("recovery checkpoint rewrote the terminal error")
	}
	bad = got
	bad.Status = StatusSucceeded
	if err := s.saveRecoveryCheckpoint(bad); err == nil {
		t.Fatal("recovery checkpoint reversed terminal status")
	}
}

func TestStoreItemSnapshotsArePrivateOwnedAndDeletedWithBatchTask(t *testing.T) {
	s, err := Open(Options{Dir: filepath.Join(t.TempDir(), "tasks")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	snapshotID := uuid.NewString()
	r.Items = []Item{{ID: "container-id", Name: "web", SnapshotRef: snapshotID}}
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	secret := []byte(`{"Env":["TOKEN=private"]}`)
	if err := s.SaveItemSnapshot(r.TaskID, snapshotID, secret); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveItemSnapshot(r.TaskID, snapshotID, []byte("replace")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("snapshot overwrite should be refused, err=%v", err)
	}
	if _, err := s.ReadItemSnapshot(uuid.NewString(), snapshotID); err == nil {
		t.Fatal("snapshot was readable using a different task ID")
	}
	stored, err := s.Get(r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(Public(stored))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), snapshotID) || strings.Contains(string(public), "snapshotRef") || strings.Contains(string(public), "TOKEN=private") {
		t.Fatalf("public batch projection leaked a private snapshot reference/content: %s", public)
	}
	if err := s.Delete(r.TaskID); err == nil {
		t.Fatal("active task with a private snapshot was deleted")
	}
	stored.Status = StatusSucceeded
	stored.FinishedAt = cloneTime(&stored.UpdatedAt)
	stored.Recovery = Recovery{Status: "not-needed"}
	stored.Items[0].Recovery = Recovery{Status: "not-needed"}
	if err := s.Save(stored); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(r.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(s.itemSnapshotPath(r.TaskID, snapshotID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private item snapshot survived task deletion: %v", err)
	}
}

func TestStoreCorruptRecordQuarantinedAndValidRecordsReadable(t *testing.T) {
	dir := t.TempDir()
	valid := sampleRecord()
	data, _ := encodeRecord(valid)
	if err := os.WriteFile(filepath.Join(dir, valid.TaskID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	badID := uuid.NewString()
	if err := os.WriteFile(filepath.Join(dir, badID+".json"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	warnings := 0
	s, err := Open(Options{Dir: dir, Warnf: func(string, ...any) { warnings++ }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if warnings == 0 {
		t.Fatal("expected corruption warning")
	}
	if _, err := s.Get(valid.TaskID); err != nil {
		t.Fatalf("valid record lost: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "corrupt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("quarantine entries = %d", len(entries))
	}
}

func TestStoreLockAcrossProcessesAndReleaseOnExit(t *testing.T) {
	if dir := os.Getenv("TASK_LOCK_HELPER_DIR"); dir != "" {
		_, err := Open(Options{Dir: dir})
		if err != nil {
			os.Exit(3)
		}
		// Deliberately exit without Close; the OS must release the lock.
		os.Exit(0)
	}
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreLockAcrossProcessesAndReleaseOnExit$")
	cmd.Env = append(os.Environ(), "TASK_LOCK_HELPER_DIR="+dir)
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 3 {
		t.Fatalf("second process did not fail specifically on lock acquisition: err=%v exit=%v", err, cmd.ProcessState.ExitCode())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cmd2 := exec.Command(os.Args[0], "-test.run=^TestStoreLockAcrossProcessesAndReleaseOnExit$")
	cmd2.Env = append(os.Environ(), "TASK_LOCK_HELPER_DIR="+dir)
	if out, err := cmd2.CombinedOutput(); err != nil || cmd2.ProcessState.ExitCode() != 0 {
		t.Fatalf("lock was not released after process exit: err=%v exit=%v: %s", err, cmd2.ProcessState.ExitCode(), out)
	}
}

func TestStoreTerminalStatusImmutableAndReconcileOnly(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	r.Status = StatusSucceeded
	r.FinishedAt = ptrTime(time.Now())
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	r.Status = StatusRunning
	if err := s.Save(r); err == nil {
		t.Fatal("terminal record regressed")
	}
	r.Status = StatusSucceeded
	if err := s.Reconcile(r.TaskID, Reconciliation{Recovery: Recovery{Status: "restored"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(r.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(r.TaskID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted record remains: %v", err)
	}
}

func TestReconcileClearsPendingInstallOnlyAfterVerifiedCleanup(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	r.Type = TaskTypeProgramUpdate
	r.Status = StatusFailed
	r.FinishedAt = ptrTime(time.Now())
	r.PendingInstall = &PendingInstallHandoff{HandoffID: "10000000-0000-4000-8000-000000000067", TargetVersion: "2.3.4", GOARCH: "amd64", ArtifactSHA256: strings.Repeat("a", 64), PreparedAt: time.Now().UTC()}
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(r.TaskID, Reconciliation{Recovery: Recovery{Status: "unknown"}, RequiresAttention: true}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(r.TaskID)
	if err != nil || got.PendingInstall == nil {
		t.Fatalf("ordinary reconciliation erased cleanup facts: marker=%#v err=%v", got.PendingInstall, err)
	}
	if err := s.Reconcile(r.TaskID, Reconciliation{Recovery: Recovery{Status: "unknown"}, RequiresAttention: true, ClearPendingInstall: true}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(r.TaskID)
	if err != nil || got.PendingInstall != nil || got.Status != StatusFailed {
		t.Fatalf("verified cleanup reconciliation did not clear marker immutably: %#v err=%v", got, err)
	}
}

func TestStoreRequiresCallerTaskID(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	r.TaskID = ""
	if err := s.Create(r); err == nil {
		t.Fatal("empty task ID must be rejected")
	}
}

func TestStoreDeleteProtectsNamesRecoveryAndActiveSnapshots(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(r.TaskID, []byte("private")); err != nil {
		t.Fatal(err)
	}
	r.Status = StatusFailed
	r.RequiresAttention = true
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSnapshot(r.TaskID); err == nil {
		t.Fatal("deleted an attention-protected snapshot")
	}
	if err := s.Delete(r.TaskID); err == nil {
		t.Fatal("deleted an attention-protected record")
	}
	if err := s.Reconcile(r.TaskID, Reconciliation{Recovery: Recovery{Status: "unknown"}, ProtectedNames: []string{"prod"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSnapshot(r.TaskID); err == nil {
		t.Fatal("deleted a snapshot with protected name and unresolved recovery")
	}
	if err := s.Reconcile(r.TaskID, Reconciliation{Recovery: Recovery{Status: "restored"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSnapshot(r.TaskID); err != nil {
		t.Fatalf("resolved snapshot deletion failed: %v", err)
	}
}

func TestStoreAtomicFailuresKeepPreviousRecord(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	s.atomicReplace = func(string, string) error { return errors.New("injected rename failure") }
	if err := s.Create(r); err == nil {
		t.Fatal("Create ignored rename failure")
	}
	if _, err := os.Stat(s.recordPath(r.TaskID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed Create left a record: %v", err)
	}
	s.atomicReplace = os.Rename
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	r.Status = StatusRunning
	s.atomicReplace = func(string, string) error { return errors.New("injected rename failure") }
	if err := s.Save(r); err == nil {
		t.Fatal("Save ignored rename failure")
	}
	got, err := s.Get(r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued {
		t.Fatalf("rename failure changed prior record to %q", got.Status)
	}
	s.atomicReplace = os.Rename
	failOnce := true
	s.dirSync = func(path string) error {
		if failOnce {
			failOnce = false
			return errors.New("injected directory sync failure")
		}
		return syncDir(path)
	}
	if err := s.Save(r); err == nil {
		t.Fatal("Save ignored directory sync failure")
	}
	got, err = s.Get(r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued {
		t.Fatalf("sync failure changed prior record to %q", got.Status)
	}
}

func TestStoreAtomicRollbackFailureRetainsBothErrors(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	r.Status = StatusRunning
	commitErr := errors.New("commit directory sync failed")
	rollbackErr := errors.New("rollback directory sync failed")
	calls := 0
	s.dirSync = func(string) error {
		calls++
		if calls == 1 {
			return commitErr
		}
		return rollbackErr
	}
	err = s.Save(r)
	if !errors.Is(err, commitErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("expected commit and rollback errors to be discoverable, got %v", err)
	}
	if !IsPersistenceOutcomeUncertain(err) {
		t.Fatalf("expected a typed uncertain persistence result, got %T: %v", err, err)
	}
}

func TestStoreSnapshotIsWriteOnceAndActiveOnly(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	baseline := []byte("first recovery baseline")
	if err := s.SaveSnapshot(r.TaskID, baseline); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(r.TaskID, []byte("replacement")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second snapshot write returned %v, want os.ErrExist", err)
	}
	got, err := s.ReadSnapshot(r.TaskID)
	if err != nil || string(got) != string(baseline) {
		t.Fatalf("baseline changed: %q, err=%v", got, err)
	}
	r.Status = StatusSucceeded
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(r.TaskID, []byte("late snapshot")); err == nil {
		t.Fatal("terminal task accepted a snapshot")
	}
	got, err = s.ReadSnapshot(r.TaskID)
	if err != nil || string(got) != string(baseline) {
		t.Fatalf("terminal task changed baseline: %q, err=%v", got, err)
	}
}

func TestPublicClonesPointerOptions(t *testing.T) {
	value := true
	r := sampleRecord()
	r.Options.DeleteOldContainer = &value
	view := Public(r)
	*view.Options.DeleteOldContainer = false
	if !*r.Options.DeleteOldContainer {
		t.Fatal("public projection shares the record's option pointer")
	}
}

func TestPublicIncludesValidatedBackupContainerName(t *testing.T) {
	record := sampleRecord()
	record.BackupContainerName = "copilot-old-42"
	view := Public(record)
	if view.BackupContainerName != record.BackupContainerName {
		t.Fatalf("public backup container name = %q, want %q", view.BackupContainerName, record.BackupContainerName)
	}
	if _, err := encodeRecord(record); err != nil {
		t.Fatalf("valid backup container name rejected: %v", err)
	}
	record.BackupContainerName = "/copilot-old-42"
	if _, err := encodeRecord(record); err == nil {
		t.Fatal("backup container name with leading slash was accepted")
	}
}

func TestStoreRejectsSymlinkRecord(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := sampleRecord()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, s.recordPath(r.TaskID)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := s.Get(r.TaskID); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestStoreSurfacesSnapshotFileFailureAndRejectsWritesAfterClose(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	r := sampleRecord()
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.snapshotPath(r.TaskID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(r.TaskID, []byte("private")); err == nil {
		t.Fatal("expected a non-regular snapshot path to fail")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(r); !errors.Is(err, errClosed) {
		t.Fatalf("write after close returned %v", err)
	}
}

func sampleRecord() Record {
	now := time.Now().UTC()
	id := uuid.NewString()
	return Record{SchemaVersion: SchemaVersion, TaskID: id, Type: "container-update", Status: StatusQueued, CreatedAt: now, UpdatedAt: now, RequestID: "req-1", Options: TaskOptions{Image: "nginx:latest"}, SnapshotRef: id + ".snapshot"}
}
func ptrTime(t time.Time) *time.Time { return &t }

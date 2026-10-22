package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPublicRecordRedactsSensitiveDiagnosticsWithoutMutatingFacts(t *testing.T) {
	secretURL := "https://user:pass@example.test/repo?access_token=secret-value"
	record := Record{
		TaskID: "d7dce3a9-c3e9-4c3f-a7d8-486b12c5e63f", Type: "container-update", Status: StatusFailed,
		Message: "failed at " + secretURL, DetailMsg: "daemon said " + secretURL,
		Error: "request failed " + secretURL, Recovery: Recovery{Status: "unknown", Detail: "retry with token=secret-value"},
		Warnings: []string{"auth=secret-value"},
		Items:    []Item{{ID: "container", Error: "authorization: Bearer abc.def.ghi"}},
	}
	public := Public(record)
	for field, value := range map[string]string{
		"message": public.Message, "detailMsg": public.DetailMsg, "error": public.Error,
		"recovery.detail": public.Recovery.Detail, "warning": public.Warnings[0], "item.error": public.Items[0].Error,
	} {
		if strings.Contains(value, "secret-value") || strings.Contains(value, "user:pass") || strings.Contains(value, "abc.def.ghi") {
			t.Errorf("public %s leaked credential text: %q", field, value)
		}
	}
	if record.Error != "request failed "+secretURL || record.Warnings[0] != "auth=secret-value" {
		t.Fatal("public projection mutated the persisted diagnostic facts")
	}
}

type readonlyInspectorFake struct {
	mu           sync.Mutex
	observations map[string]ContainerObservation
	errors       map[string]error
	calls        []string
}

func (f *readonlyInspectorFake) Inspect(ctx context.Context, id string) (ContainerObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	if err := ctx.Err(); err != nil {
		return ContainerObservation{}, err
	}
	if err := f.errors[id]; err != nil {
		return ContainerObservation{}, err
	}
	observation, ok := f.observations[id]
	if !ok {
		return ContainerObservation{}, errors.New("inspect failed")
	}
	return observation, nil
}

type pendingValidatorFake struct {
	proof PendingInstallProof
	err   error
	calls int
}

func (f *pendingValidatorFake) VerifyPendingInstall(_ context.Context, _ Record) (PendingInstallProof, error) {
	f.calls++
	return f.proof, f.err
}

func TestQueryServiceFiltersStablePagesAndKeepsPublicProjectionRestricted(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	records := []Record{
		{SchemaVersion: SchemaVersion, TaskID: "00000000-0000-4000-8000-000000000001", Type: "container-update", Status: StatusRunning, CreatedAt: created, Name: "web", OldContainerID: "old-a", ProtectedNames: []string{"web"}, SnapshotRef: "00000000-0000-4000-8000-000000000001.snapshot", PendingInstall: &PendingInstallHandoff{HandoffID: "private-handoff", TargetVersion: "secret-version", ArtifactSHA256: "secret-checksum"}},
		{SchemaVersion: SchemaVersion, TaskID: "00000000-0000-4000-8000-000000000002", Type: "container-update", Status: StatusSucceeded, CreatedAt: created, NewContainerID: "new-b", ContainerIDs: []string{"extra-b"}, ContainerChecks: []ContainerCheck{{ContainerID: "check-b", Name: "worker"}}},
		{SchemaVersion: SchemaVersion, TaskID: "00000000-0000-4000-8000-000000000003", Type: "backup", Status: StatusFailed, CreatedAt: created.Add(-time.Hour), Recovery: Recovery{Status: "not-needed"}},
		{SchemaVersion: SchemaVersion, TaskID: "00000000-0000-4000-8000-000000000004", Type: "container-update", Status: StatusFailed, CreatedAt: created.Add(-2 * time.Hour), OldContainerID: "old-a"},
	}
	for _, record := range records {
		if err := store.Create(record); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewQueryService(store, QueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.List(TaskFilter{Type: "container-update", ContainerID: "old-a", Page: 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || !page.HasMore || len(page.Items) != 1 || page.Items[0].TaskID != records[0].TaskID || page.Page != 1 || page.Limit != 1 {
		t.Fatalf("filtered page: %#v", page)
	}
	byName, err := service.List(TaskFilter{ContainerName: "worker"})
	if err != nil || byName.Total != 1 || byName.Items[0].TaskID != records[1].TaskID {
		t.Fatalf("name filter page=%#v err=%v", byName, err)
	}
	byState, err := service.List(TaskFilter{Status: StatusSucceeded})
	if err != nil || byState.Total != 1 || byState.Items[0].TaskID != records[1].TaskID {
		t.Fatalf("status filter page=%#v err=%v", byState, err)
	}
	byNewID, err := service.List(TaskFilter{ContainerID: "new-b"})
	if err != nil || byNewID.Total != 1 || byNewID.Items[0].TaskID != records[1].TaskID {
		t.Fatalf("new ID filter page=%#v err=%v", byNewID, err)
	}
	byExtraID, err := service.List(TaskFilter{ContainerID: "extra-b"})
	if err != nil || byExtraID.Total != 1 || byExtraID.Items[0].TaskID != records[1].TaskID {
		t.Fatalf("containerIDs filter page=%#v err=%v", byExtraID, err)
	}
	byProtectedName, err := service.List(TaskFilter{ContainerName: "web"})
	if err != nil || byProtectedName.Total != 1 || byProtectedName.Items[0].TaskID != records[0].TaskID {
		t.Fatalf("protected name filter page=%#v err=%v", byProtectedName, err)
	}
	defaultPage, err := service.List(TaskFilter{})
	if err != nil || defaultPage.Page != 1 || defaultPage.Limit != DefaultTaskPageSize {
		t.Fatalf("default page=%#v err=%v", defaultPage, err)
	}
	if _, err := service.List(TaskFilter{Status: "garbage"}); err == nil {
		t.Fatal("invalid status filter accepted")
	}
	if _, err := service.List(TaskFilter{Page: -1}); err == nil {
		t.Fatal("negative page accepted")
	}
	if _, err := service.List(TaskFilter{Limit: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
	if _, err := service.List(TaskFilter{Limit: MaximumTaskPageSize + 1}); err == nil {
		t.Fatal("oversized limit accepted")
	}
	if _, err := service.List(TaskFilter{Page: math.MaxInt}); err == nil {
		t.Fatal("overflowing page offset accepted")
	}
	legacy, err := service.GetProgress(records[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Percentage != 0 || legacy.Message != "" || legacy.Name != "web" || legacy.IsDone {
		t.Fatalf("legacy progress projection: %#v", legacy)
	}
	done, err := service.GetProgress(records[1].TaskID)
	if err != nil || !done.IsDone {
		t.Fatalf("terminal progress projection: %#v err=%v", done, err)
	}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"snapshotRef", "private.snapshot", "pendingInstall", "private-handoff", "secret-version", "secret-checksum"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("public query leaked %q: %s", private, encoded)
		}
	}
}

func TestStartupReconcileUsesRealStoreAndOnlyReadOnlyInspection(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	queued := startupRecord("00000000-0000-4000-8000-000000000011", StatusQueued)
	queued.ProtectedNames = []string{"queued-name"}
	running := startupRecord("00000000-0000-4000-8000-000000000012", StatusRunning)
	running.Stage = "rename-old"
	running.OldContainerID = "old-id"
	running.NewContainerID = "new-id"
	running.ContainerIDs = []string{"old-id", "extra-id"}
	running.ProtectedNames = []string{"prod-name"}
	completed := startupRecord("00000000-0000-4000-8000-000000000013", StatusSucceeded)
	completed.Recovery = Recovery{Status: "not-needed"}
	for _, record := range []Record{queued, running, completed} {
		if err := store.Create(record); err != nil {
			t.Fatal(err)
		}
		if record.Status == StatusRunning {
			if err := store.Save(record); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inspector := &readonlyInspectorFake{observations: map[string]ContainerObservation{
		"old-id":   {Name: "prod-name", State: "exited", Running: false},
		"new-id":   {Name: "prod-name-next", State: "running", Running: true},
		"extra-id": {Name: "other", State: "running", Running: true},
	}}
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	reconciler, err := NewStartupReconciler(store, inspector, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.InterruptedTaskIDs) != 2 || len(report.PendingInstallTaskIDs) != 0 {
		t.Fatalf("startup report=%#v", report)
	}
	gotQueued, err := store.Get(queued.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if gotQueued.Status != StatusInterrupted || !gotQueued.IsDone || gotQueued.Recovery.Status != "not-needed" || gotQueued.ProtectedNames[0] != "queued-name" {
		t.Fatalf("queued restart result=%#v", gotQueued)
	}
	gotRunning, err := store.Get(running.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRunning.Status != StatusInterrupted || gotRunning.Stage != "rename-old" || gotRunning.OldContainerID != "old-id" || gotRunning.NewContainerID != "new-id" || !gotRunning.RequiresAttention || gotRunning.Recovery.Status != "unknown" || gotRunning.ProtectedNames[0] != "prod-name" {
		t.Fatalf("running restart result=%#v", gotRunning)
	}
	if len(gotRunning.ContainerChecks) != 3 {
		t.Fatalf("read-only checks=%#v", gotRunning.ContainerChecks)
	}
	if len(inspector.calls) != 3 {
		t.Fatalf("inspector calls=%v", inspector.calls)
	}
	for _, check := range gotRunning.ContainerChecks {
		if check.Result != "found" {
			t.Fatalf("check did not preserve observed state: %#v", check)
		}
	}
	gotCompleted, err := store.Get(completed.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if gotCompleted.Status != StatusSucceeded || len(gotCompleted.ContainerChecks) != 0 {
		t.Fatalf("startup rewrote a terminal result: %#v", gotCompleted)
	}
	service, err := NewQueryService(store, QueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(TaskFilter{Status: StatusInterrupted}); err != nil {
		t.Fatal(err)
	}
}

func TestStartupReconcileInspectionFailureRetainsEvidenceAndAttention(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record := startupRecord(uuid.NewString(), StatusRunning)
	record.ContainerIDs = []string{"docker-id"}
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	inspector := &readonlyInspectorFake{errors: map[string]error{"docker-id": errors.New("daemon unavailable")}}
	reconciler, err := NewStartupReconciler(store, inspector, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) == 0 {
		t.Fatal("inspection failure was silently accepted")
	}
	got, err := store.Get(record.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInterrupted || !got.RequiresAttention || got.ContainerIDs[0] != "docker-id" || got.ContainerChecks[0].Result != "unavailable" {
		t.Fatalf("inspection failure lost evidence: %#v", got)
	}
	if strings.Contains(got.Error, "daemon unavailable") {
		t.Fatalf("raw inspector error exposed: %q", got.Error)
	}
}

func TestStartupReconcileDistinguishesNotFoundAndSurfacesPersistenceFailure(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record := startupRecord(uuid.NewString(), StatusRunning)
	record.NewContainerID = "missing-id"
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	inspector := &readonlyInspectorFake{errors: map[string]error{"missing-id": ErrContainerNotFound}}
	reconciler, err := NewStartupReconciler(store, inspector, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(record.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContainerChecks[0].Result != "not-found" || !got.RequiresAttention {
		t.Fatalf("not-found state was misrepresented: %#v", got)
	}

	faulty := &startupFaultStore{records: []Record{startupRecord(uuid.NewString(), StatusQueued)}, saveErr: errors.New("startup write fault")}
	reconciler, err = NewStartupReconciler(faulty, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "startup write fault") || len(report.InterruptedTaskIDs) != 0 {
		t.Fatalf("startup persistence failure hidden: report=%#v err=%v", report, err)
	}
}

func TestPendingInstallRequiresCompleteMatchingVerification(t *testing.T) {
	validMarker := PendingInstallHandoff{HandoffID: "10000000-0000-4000-8000-000000000001", TargetVersion: "v2.0.0", GOARCH: "amd64", ArtifactSHA256: strings.Repeat("a", 64), PreparedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	validProof := PendingInstallProof{Verified: true, TaskID: "00000000-0000-4000-8000-000000000021", HandoffID: validMarker.HandoffID, TargetVersion: validMarker.TargetVersion, GOARCH: validMarker.GOARCH, ArtifactSHA256: validMarker.ArtifactSHA256}
	cases := []struct {
		name        string
		taskType    string
		status      Status
		marker      *PendingInstallHandoff
		validator   *pendingValidatorFake
		wantPending bool
	}{
		{name: "verified complete handoff", taskType: TaskTypeProgramUpdate, marker: &validMarker, validator: &pendingValidatorFake{proof: validProof}, wantPending: true},
		{name: "type alone", taskType: TaskTypeProgramUpdate},
		{name: "missing marker field", taskType: TaskTypeProgramUpdate, marker: &PendingInstallHandoff{HandoffID: validMarker.HandoffID}},
		{name: "unsupported marker architecture", taskType: TaskTypeProgramUpdate, marker: func() *PendingInstallHandoff { m := validMarker; m.GOARCH = "386"; return &m }(), validator: &pendingValidatorFake{proof: validProof}},
		{name: "validator unavailable", taskType: TaskTypeProgramUpdate, marker: &validMarker},
		{name: "mismatched proof", taskType: TaskTypeProgramUpdate, marker: &validMarker, validator: &pendingValidatorFake{proof: PendingInstallProof{Verified: true, TaskID: "wrong", HandoffID: validMarker.HandoffID, TargetVersion: validMarker.TargetVersion, ArtifactSHA256: validMarker.ArtifactSHA256}}},
		{name: "mismatched target version", taskType: TaskTypeProgramUpdate, marker: &validMarker, validator: &pendingValidatorFake{proof: PendingInstallProof{Verified: true, TaskID: validProof.TaskID, HandoffID: validMarker.HandoffID, TargetVersion: "v2.0.1", ArtifactSHA256: validMarker.ArtifactSHA256}}},
		{name: "mismatched digest", taskType: TaskTypeProgramUpdate, marker: &validMarker, validator: &pendingValidatorFake{proof: PendingInstallProof{Verified: true, TaskID: validProof.TaskID, HandoffID: validMarker.HandoffID, TargetVersion: validMarker.TargetVersion, ArtifactSHA256: strings.Repeat("b", 64)}}},
		{name: "mismatched architecture", taskType: TaskTypeProgramUpdate, marker: &validMarker, validator: &pendingValidatorFake{proof: PendingInstallProof{Verified: true, TaskID: validProof.TaskID, HandoffID: validMarker.HandoffID, TargetVersion: validMarker.TargetVersion, GOARCH: "arm64", ArtifactSHA256: validMarker.ArtifactSHA256}}},
		{name: "validator error", taskType: TaskTypeProgramUpdate, marker: &validMarker, validator: &pendingValidatorFake{err: errors.New("bad handoff")}},
		{name: "other task type", taskType: "container-update", marker: &validMarker, validator: &pendingValidatorFake{proof: validProof}},
		{name: "queued handoff", taskType: TaskTypeProgramUpdate, marker: &validMarker, validator: &pendingValidatorFake{proof: validProof}, status: StatusQueued},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(Options{Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			status := tc.status
			if status == "" {
				status = StatusRunning
			}
			record := startupRecord(validProof.TaskID, status)
			record.Type, record.PendingInstall = tc.taskType, tc.marker
			if err := store.Create(record); err != nil {
				t.Fatal(err)
			}
			if err := store.Save(record); err != nil {
				t.Fatal(err)
			}
			var validator PendingInstallValidator
			if tc.validator != nil {
				validator = tc.validator
			}
			reconciler, err := NewStartupReconciler(store, nil, validator, nil)
			if err != nil {
				t.Fatal(err)
			}
			report, err := reconciler.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(record.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantPending {
				if got.Status != StatusRunning || len(report.PendingInstallTaskIDs) != 1 || len(report.InterruptedTaskIDs) != 0 {
					t.Fatalf("valid handoff was consumed: record=%#v report=%#v", got, report)
				}
			} else if got.Status != StatusInterrupted || len(report.PendingInstallTaskIDs) != 0 || len(report.InterruptedTaskIDs) != 1 {
				t.Fatalf("unverified handoff was skipped: record=%#v report=%#v", got, report)
			}
			if tc.validator != nil && tc.validator.err != nil && len(report.Warnings) == 0 {
				t.Fatal("validator failure was not surfaced")
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQueryCleanupRetainsProtectedAndEnforcesAgeAndCount(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	makeRecord := func(id string, age time.Duration, status Status, recovery string) Record {
		record := startupRecord(id, status)
		record.CreatedAt = now.Add(-age)
		record.UpdatedAt = record.CreatedAt
		record.Recovery = Recovery{Status: recovery}
		return record
	}
	records := []Record{
		makeRecord("00000000-0000-4000-8000-000000000031", 10*24*time.Hour, StatusFailed, "not-needed"), // expired
		makeRecord("00000000-0000-4000-8000-000000000032", 24*time.Hour, StatusSucceeded, "not-needed"), // newest eligible
		makeRecord("00000000-0000-4000-8000-000000000033", 48*time.Hour, StatusFailed, "restored"),      // count overflow
		makeRecord("00000000-0000-4000-8000-000000000034", 30*24*time.Hour, StatusFailed, "unknown"),    // unresolved
		makeRecord("00000000-0000-4000-8000-000000000035", 30*24*time.Hour, StatusRunning, ""),          // active
		makeRecord("00000000-0000-4000-8000-000000000036", 30*24*time.Hour, StatusFailed, "not-needed"), // protected name
	}
	records[5].ProtectedNames = []string{"protected-config"}
	records[3].RequiresAttention = true
	for _, record := range records {
		if err := store.Create(record); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewQueryService(store, QueryOptions{Retention: 7 * 24 * time.Hour, MaxRecords: 1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := service.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Deleted) != 2 || report.Retained != 1 || report.Protected < 3 {
		t.Fatalf("cleanup report=%#v", report)
	}
	for _, id := range []string{records[0].TaskID, records[2].TaskID} {
		if _, err := store.Get(id); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("eligible old task %s remains: %v", id, err)
		}
	}
	for _, record := range []Record{records[1], records[3], records[4], records[5]} {
		if _, err := store.Get(record.TaskID); err != nil {
			t.Fatalf("protected/recent task %s was removed: %v", record.TaskID, err)
		}
	}
}

func TestQueryCleanupSurfacesDeleteFailure(t *testing.T) {
	store := &faultQueryStore{records: []Record{startupRecord(uuid.NewString(), StatusSucceeded)}, deleteErr: errors.New("disk delete failure")}
	store.records[0].Recovery = Recovery{Status: "not-needed"}
	service, err := NewQueryService(store, QueryOptions{Retention: time.Hour, Now: func() time.Time { return time.Now().Add(10 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := service.Cleanup()
	if err == nil || !strings.Contains(err.Error(), "disk delete failure") || len(report.Deleted) != 0 {
		t.Fatalf("cleanup failure was hidden: report=%#v err=%v", report, err)
	}
}

func TestCleanupUsesFinishedAtAndLegacyUpdatedAtForRetention(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	oldCreated := startupRecord("00000000-0000-4000-8000-000000000071", StatusSucceeded)
	oldCreated.CreatedAt = now.Add(-30 * 24 * time.Hour)
	oldCreated.UpdatedAt = oldCreated.CreatedAt
	finished := now.Add(-time.Hour)
	oldCreated.FinishedAt = &finished
	oldCreated.Recovery = Recovery{Status: "not-needed"}
	legacy := startupRecord("00000000-0000-4000-8000-000000000072", StatusFailed)
	legacy.CreatedAt = now.Add(-30 * 24 * time.Hour)
	legacy.UpdatedAt = now.Add(-2 * time.Hour)
	legacy.Recovery = Recovery{Status: "not-needed"}
	for _, record := range []Record{oldCreated, legacy} {
		if err := store.Create(record); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewQueryService(store, QueryOptions{Retention: 7 * 24 * time.Hour, MaxRecords: 1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := service.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Deleted) != 1 || report.Deleted[0] != legacy.TaskID || report.Retained != 1 {
		t.Fatalf("cleanup did not use terminal time for age/order: %#v", report)
	}
	if _, err := store.Get(oldCreated.TaskID); err != nil {
		t.Fatalf("recently finished long-running task was removed: %v", err)
	}
	if _, err := store.Get(legacy.TaskID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old legacy record without FinishedAt was retained: %v", err)
	}
}

func TestBatchItemsProvideContainerLookupAndReadOnlyInspectionOnlyForContainerTasks(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	containerTask := startupRecord("00000000-0000-4000-8000-000000000081", StatusRunning)
	containerTask.Type = "container-batch-update"
	containerTask.Items = []Item{{ID: "batch-container-id", Name: "batch-container-name"}}
	containerTask.ImageIDs = []string{"image-only-id"}
	imageTask := startupRecord("00000000-0000-4000-8000-000000000082", StatusRunning)
	imageTask.Type = "image-cleanup"
	imageTask.Items = []Item{{ID: "image-item-id", Name: "image-item-name"}}
	imageTask.ImageIDs = []string{"image-only-id"}
	for _, record := range []Record{containerTask, imageTask} {
		if err := store.Create(record); err != nil {
			t.Fatal(err)
		}
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	query, err := NewQueryService(store, QueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, filter := range []TaskFilter{{ContainerID: "batch-container-id"}, {ContainerName: "batch-container-name"}} {
		page, err := query.List(filter)
		if err != nil || page.Total != 1 || page.Items[0].TaskID != containerTask.TaskID {
			t.Fatalf("batch item container filter=%#v page=%#v err=%v", filter, page, err)
		}
	}
	if page, err := query.List(TaskFilter{ContainerID: "image-item-id"}); err != nil || page.Total != 0 {
		t.Fatalf("image item matched a container filter: %#v err=%v", page, err)
	}
	inspector := &readonlyInspectorFake{observations: map[string]ContainerObservation{"batch-container-id": {Name: "batch-container-name", State: "running", Running: true}}}
	reconciler, err := NewStartupReconciler(store, inspector, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(inspector.calls) != 1 || inspector.calls[0] != "batch-container-id" {
		t.Fatalf("inspector received non-container item IDs: %v", inspector.calls)
	}
	got, err := store.Get(containerTask.TaskID)
	if err != nil || len(got.ContainerChecks) != 1 || got.ContainerChecks[0].ContainerID != "batch-container-id" {
		t.Fatalf("batch container inspection was not persisted: %#v err=%v", got, err)
	}
}

type faultQueryStore struct {
	records   []Record
	deleteErr error
}

type startupFaultStore struct {
	records []Record
	saveErr error
}

func (s *startupFaultStore) List() ([]Record, error) {
	out := make([]Record, len(s.records))
	for i, record := range s.records {
		out[i] = cloneRecord(record)
	}
	return out, nil
}
func (s *startupFaultStore) Save(Record) error { return s.saveErr }

func (s *faultQueryStore) Get(id string) (Record, error) {
	for _, r := range s.records {
		if r.TaskID == id {
			return cloneRecord(r), nil
		}
	}
	return Record{}, fmt.Errorf("not found")
}
func (s *faultQueryStore) List() ([]Record, error) {
	out := make([]Record, len(s.records))
	for i, r := range s.records {
		out[i] = cloneRecord(r)
	}
	return out, nil
}
func (s *faultQueryStore) Delete(string) error { return s.deleteErr }

func startupRecord(id string, status Status) Record {
	return Record{SchemaVersion: SchemaVersion, TaskID: id, Type: "container-update", Status: status, Stage: "preflight", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

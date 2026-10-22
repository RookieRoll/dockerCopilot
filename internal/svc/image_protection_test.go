package svc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/task"
)

func TestTaskImageProtectionIncludesTargetReferenceAndPrivateSnapshotImage(t *testing.T) {
	store, err := task.Open(task.Options{Dir: filepath.Join(t.TempDir(), "tasks")})
	if err != nil {
		t.Fatalf("open task store: %v", err)
	}
	defer store.Close()
	const taskID = "00000000-0000-4000-8000-000000000123"
	record := task.Record{
		SchemaVersion: task.SchemaVersion, TaskID: taskID, Type: "container-update", Status: task.StatusRunning,
		Stage: "preflight", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Options: task.TaskOptions{Image: "example/new:v2"}, SnapshotRef: taskID + ".snapshot",
	}
	if err := store.Create(record); err != nil {
		t.Fatalf("create active update record: %v", err)
	}
	snapshot := []byte(`{"Id":"container-id","Image":"sha256:old-image","Config":{"Image":"example/old:v1"}}`)
	if err := store.SaveSnapshot(taskID, snapshot); err != nil {
		t.Fatalf("save update snapshot: %v", err)
	}
	serviceCtx := &ServiceContext{TaskStore: store}
	protection, err := taskImageProtection{ctx: serviceCtx}.ProtectedImages(context.Background())
	if err != nil {
		t.Fatalf("load image protection: %v", err)
	}
	if got := protection.IDs["old-image"]; len(got) != 1 || got[0] != taskID {
		t.Fatalf("private snapshot did not protect old image with task attribution: %#v", protection.IDs)
	}
	for _, target := range []string{"example/new:v2", "example/old:v1"} {
		canonical := imageService.NormalizeReference(target)
		if got := protection.References[canonical]; len(got) != 1 || got[0] != taskID {
			t.Fatalf("snapshot/target ref %q not protected: %#v", target, protection.References)
		}
	}
}

func TestTaskImageProtectionFailsClosedForMissingDurableSnapshot(t *testing.T) {
	store, err := task.Open(task.Options{Dir: filepath.Join(t.TempDir(), "tasks")})
	if err != nil {
		t.Fatalf("open task store: %v", err)
	}
	defer store.Close()
	const taskID = "00000000-0000-4000-8000-000000000124"
	record := task.Record{
		SchemaVersion: task.SchemaVersion, TaskID: taskID, Type: "container-update", Status: task.StatusRunning,
		Stage: "snapshot-saved", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Options: task.TaskOptions{Image: "example/new:v2"}, SnapshotRef: taskID + ".snapshot",
	}
	if err := store.Create(record); err != nil {
		t.Fatalf("create active update record: %v", err)
	}
	_, err = taskImageProtection{ctx: &ServiceContext{TaskStore: store}}.ProtectedImages(context.Background())
	if err == nil || !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("missing update snapshot error=%v, want fail-closed snapshot error", err)
	}
}

func TestTaskImageProtectionSkipsItemSnapshotBeforeSnapshotSaved(t *testing.T) {
	for index, stage := range []string{"queued", "starting", "preflight"} {
		t.Run(stage, func(t *testing.T) {
			store, err := task.Open(task.Options{Dir: filepath.Join(t.TempDir(), "tasks")})
			if err != nil {
				t.Fatalf("open task store: %v", err)
			}
			defer store.Close()
			taskID := fmt.Sprintf("00000000-0000-4000-8000-%012d", 200+index)
			record := task.Record{
				SchemaVersion: task.SchemaVersion, TaskID: taskID, Type: "container-update-batch", Status: task.StatusRunning,
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
				Items: []task.Item{{
					ID: "container-id", Status: "running", Stage: stage,
					ImageRef: "example/new:v2", ImageIDs: []string{"sha256:admission-old-image"},
					SnapshotRef: taskID,
				}},
			}
			if err := store.Create(record); err != nil {
				t.Fatalf("create active batch task: %v", err)
			}

			protection, err := taskImageProtection{ctx: &ServiceContext{TaskStore: store}}.ProtectedImages(context.Background())
			if err != nil {
				t.Fatalf("protection read should not require a not-yet-written snapshot: %v", err)
			}
			if got := protection.IDs["admission-old-image"]; len(got) != 1 || got[0] != taskID {
				t.Fatalf("admission-time old image ID was not protected: %#v", protection.IDs)
			}
			ref := imageService.NormalizeReference("example/new:v2")
			if got := protection.References[ref]; len(got) != 1 || got[0] != taskID {
				t.Fatalf("admission-time image ref was not protected: %#v", protection.References)
			}
		})
	}
}

package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSettledContainerUpdateReleasesNamesAndRetentionRemovesSnapshot(t *testing.T) {
	for _, test := range []struct {
		name     string
		commit   bool
		warning  bool
		runErr   error
		recovery Recovery
		wantKeep bool
	}{
		{name: "successful commit with cleanup warning", commit: true, warning: true},
		{name: "failed before mutation", runErr: errors.New("pull failed")},
		{name: "failed but restored", runErr: errors.New("replacement failed"), recovery: Recovery{Status: "restored"}},
		{name: "unresolved recovery remains protected", runErr: errors.New("replacement failed"), recovery: Recovery{Status: "unknown"}, wantKeep: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, runner, taskID, snapshotPath, query := newRetentionFixture(t, time.Second)
			_, err := runner.Submit(TaskSpec{
				Record: Record{SchemaVersion: SchemaVersion, TaskID: taskID, Type: "container-update", Name: "web"},
				Run: func(_ context.Context, execution *Execution) error {
					if err := store.SaveSnapshot(taskID, []byte("private rollback baseline")); err != nil {
						return err
					}
					if err := execution.Critical("snapshot-saved", func(record *Record) {
						record.SnapshotRef = taskID + ".snapshot"
						record.ProtectedNames = []string{"web"}
					}); err != nil {
						return err
					}
					if test.commit {
						if err := execution.CriticalCommit("replacement-committed", func(record *Record) {
							record.NewContainerID = "new-container-id"
						}); err != nil {
							return err
						}
						if test.warning {
							execution.Warning("old container cleanup failed; new container is healthy")
						}
					}
					return test.runErr
				},
				Recover: func(context.Context, error) (Recovery, error) {
					if test.recovery.Status == "" {
						return Recovery{Status: "not-needed"}, nil
					}
					return test.recovery, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			waitRunnerIdle(t, runner)
			record, err := store.Get(taskID)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantKeep {
				if len(record.ProtectedNames) != 1 || !record.RequiresAttention || record.Recovery.Status != "unknown" {
					t.Fatalf("unresolved task lost protection: %#v", record)
				}
			} else if len(record.ProtectedNames) != 0 {
				t.Fatalf("settled task retained stale name protection: %#v", record.ProtectedNames)
			}
			if test.warning && len(record.Warnings) == 0 {
				t.Fatal("cleanup warning was not retained")
			}

			report, err := query.Cleanup()
			if err != nil {
				t.Fatal(err)
			}
			if test.wantKeep {
				if report.Deleted != nil && len(report.Deleted) != 0 {
					t.Fatalf("unresolved task was deleted: %#v", report)
				}
				if _, err := store.ReadSnapshot(taskID); err != nil {
					t.Fatalf("protected snapshot missing: %v", err)
				}
			} else {
				if len(report.Deleted) != 1 || report.Deleted[0] != taskID {
					t.Fatalf("settled task did not expire: %#v", report)
				}
				if _, err := store.Get(taskID); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("expired task still exists, err=%v", err)
				}
				if _, err := os.Stat(snapshotPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("private snapshot still exists, err=%v", err)
				}
			}
		})
	}
}

func TestCommittedUpdateKeepsProtectionUntilUncooperativeRunSettles(t *testing.T) {
	store, runner, taskID, snapshotPath, query := newRetentionFixture(t, time.Second)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	// Registered after the fixture's Runner.Close cleanup, so the callback is
	// always released before Close waits for the worker.
	t.Cleanup(unlock)
	started := make(chan struct{})
	_, err := runner.Submit(TaskSpec{
		Record: Record{SchemaVersion: SchemaVersion, TaskID: taskID, Type: "container-update", Name: "web"},
		Run: func(_ context.Context, execution *Execution) error {
			if err := store.SaveSnapshot(taskID, []byte("private rollback baseline")); err != nil {
				return err
			}
			if err := execution.Critical("snapshot-saved", func(record *Record) {
				record.SnapshotRef = taskID + ".snapshot"
				record.ProtectedNames = []string{"web"}
			}); err != nil {
				return err
			}
			if err := execution.CriticalCommit("replacement-committed", func(record *Record) {
				record.NewContainerID = "new-container-id"
			}); err != nil {
				return err
			}
			close(started)
			<-release // deliberately ignores task cancellation
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not reach committed, blocked cleanup state")
	}
	terminal := waitForTaskRecord(t, store, taskID, func(record Record) bool { return record.Status == StatusSucceeded })
	if len(terminal.ProtectedNames) != 1 {
		t.Fatalf("name protection released before Run returned: %#v", terminal.ProtectedNames)
	}
	if _, err := query.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshotPath); err != nil {
		t.Fatalf("snapshot deleted while cleanup callback was still running: %v", err)
	}
	unlock()
	waitRunnerIdle(t, runner)
	settled, err := store.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled.ProtectedNames) != 0 {
		t.Fatalf("settled task retained name protection: %#v", settled.ProtectedNames)
	}
	report, err := query.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Deleted) != 1 || report.Deleted[0] != taskID {
		t.Fatalf("settled task did not expire after cleanup returned: %#v", report)
	}
}

func newRetentionFixture(t *testing.T, taskTimeout time.Duration) (*Store, *Runner, string, string, *QueryService) {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second)
	dir := t.TempDir()
	store, err := Open(Options{Dir: dir, Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runner, err := NewRunner(store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: taskTimeout, RecoveryTimeout: time.Second, Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	query, err := NewQueryService(store, QueryOptions{Retention: 7 * 24 * time.Hour, Now: func() time.Time { return base.Add(8 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	taskID := uuid.NewString()
	return store, runner, taskID, filepath.Join(dir, taskID+".snapshot"), query
}

func waitRunnerIdle(t *testing.T, runner *Runner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runner.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func waitForTaskRecord(t *testing.T, store *Store, id string, predicate func(Record) bool) Record {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		record, err := store.Get(id)
		if err == nil && predicate(record) {
			return record
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("task did not reach expected state: %#v", record)
	return Record{}
}

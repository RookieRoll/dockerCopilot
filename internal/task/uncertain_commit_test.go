package task

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUncertainCommitCheckpointNeverRunsAutomaticRecovery(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch-item"
		}
		t.Run(name, func(t *testing.T) {
			store, err := Open(Options{Dir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			var syncCalls int
			commitErr := errors.New("injected commit directory sync failure")
			rollbackErr := errors.New("injected restore directory sync failure")
			store.dirSync = func(path string) error {
				syncCalls++
				switch syncCalls {
				case 4:
					return commitErr
				case 5:
					return rollbackErr
				default:
					return syncDir(path)
				}
			}

			runner, err := NewRunner(store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: 2 * time.Second, RecoveryTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer runner.Close()
			record := sampleRecord()
			record.Type = "container-update"
			if batch {
				record.Type = "container-batch-update"
				record.Items = []Item{{ID: "old-id", Name: "web", Status: "running", ProtectedNames: []string{"web"}}}
			}
			var recoveryCalls, fakeRemoveCalls int
			_, err = runner.Submit(TaskSpec{
				Record: record,
				Run: func(_ context.Context, execution *Execution) error {
					mutate := func(next *Record) {
						next.NewContainerID = "replacement-id"
						next.ContainerIDs = append(next.ContainerIDs, "replacement-id")
						next.ProtectedNames = append(next.ProtectedNames, "web")
						if batch {
							next.Items[0].NewContainerID = "replacement-id"
							next.Items[0].ProtectedNames = append(next.Items[0].ProtectedNames, "web")
						}
					}
					if err := execution.Critical("new-started", func(next *Record) {
						mutate(next)
						if batch {
							next.Items[0].Status = "running"
							next.Items[0].Stage = "new-started"
						}
					}); err != nil {
						return err
					}
					if batch {
						return execution.CriticalItemCommit(0, "replacement-committed", nil)
					}
					return execution.CriticalCommit("replacement-committed", nil)
				},
				Recover: func(context.Context, error) (Recovery, error) {
					recoveryCalls++
					fakeRemoveCalls++ // represents deleting the replacement before restoring the old container
					return Recovery{Status: "restored"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := runner.WaitIdle(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(record.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if recoveryCalls != 0 || fakeRemoveCalls != 0 {
				t.Fatalf("uncertain visible commit was automatically compensated: recover=%d remove=%d", recoveryCalls, fakeRemoveCalls)
			}
			public := Public(got)
			if public.Status != StatusFailed || !public.RequiresAttention || public.Recovery.Status != "unknown" || public.NewContainerID != "replacement-id" {
				t.Fatalf("uncertain commit was presented as success or lost facts: %#v", public)
			}
			if batch && (len(public.Items) != 1 || public.Items[0].Committed || public.Items[0].Status != "failed" || !public.Items[0].RequiresAttention || public.Items[0].NewContainerID != "replacement-id") {
				t.Fatalf("batch item uncertainty was misreported: %#v", public.Items)
			}
			if batch && (len(got.Items[0].ProtectedNames) == 0 || got.Items[0].ProtectedNames[0] != "web") {
				t.Fatalf("uncertain item lost its container protection: %#v", got.Items[0])
			}
		})
	}
}

func TestUncertainBatchItemCommitStopsInlineRecoveryAndLaterItems(t *testing.T) {
	store, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var syncCalls int
	store.dirSync = func(path string) error {
		syncCalls++
		switch syncCalls {
		case 5:
			return errors.New("injected item commit directory sync failure")
		case 6:
			return errors.New("injected item rollback directory sync failure")
		default:
			return syncDir(path)
		}
	}
	runner, err := NewRunner(store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: 2 * time.Second, RecoveryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	record := sampleRecord()
	record.Type = "container-batch-update"
	var recoverCalls, secondStarted int
	operations := []BatchOperation{
		{
			Item: Item{ID: "old-a", Name: "a", ProtectedNames: []string{"a"}},
			Run: func(_ context.Context, e *Execution) error {
				if err := e.Critical("replacement-created", func(r *Record) {
					r.Items[0].Status = "running"
					r.Items[0].NewContainerID = "new-a"
					r.Items[0].ProtectedNames = []string{"a"}
					r.NewContainerID = "new-a"
					r.ProtectedNames = []string{"a"}
				}); err != nil {
					return err
				}
				return e.CriticalItemCommit(0, "replacement-committed", nil)
			},
			Recover: func(context.Context, error) (Recovery, error) {
				recoverCalls++
				return Recovery{Status: "restored"}, nil
			},
		},
		{
			Item: Item{ID: "old-b", Name: "b"},
			Run: func(context.Context, *Execution) error {
				secondStarted++
				return nil
			},
		},
	}
	if _, err := runner.SubmitBatchWithHooks(record, operations, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runner.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(record.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if recoverCalls != 0 || secondStarted != 0 {
		t.Fatalf("uncertain item commit continued automatic work: recover=%d second=%d", recoverCalls, secondStarted)
	}
	if got.Status != StatusFailed || !got.RequiresAttention || got.Recovery.Status != "unknown" || got.Items[0].Committed || got.Items[0].Status != "failed" || !got.Items[0].RequiresAttention || got.Items[0].NewContainerID != "new-a" {
		t.Fatalf("uncertain batch commit facts were not conservatively retained: %#v", got)
	}
	if got.Items[1].Status != "skipped" {
		t.Fatalf("later batch item was not left unstarted: %#v", got.Items[1])
	}
	if public := Public(got); public.Status == StatusSucceeded || len(public.Items) < 1 || public.Items[0].Committed {
		t.Fatalf("public task incorrectly presented an uncertain commit as success: %#v", public)
	}
}

package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx/logtest"
)

func TestRunnerLogsCorrelatedLifecycleIncludingNormalRecovery(t *testing.T) {
	collector := logtest.NewCollector(t)
	store := newRunnerMemoryStore()
	runner := newTestRunner(t, store, RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1, TaskTimeout: time.Second, RecoveryTimeout: time.Second})
	record := runnerRecord()
	record.RequestID = "req-42"
	record.Name = "web"
	record.OldContainerID = "old-container-42"
	record.ImageRef = "registry.example/app:v2"
	record.ImageIDs = []string{"sha256:old-image", "sha256:new-image"}
	secret := "https://user:password@example.invalid/?token=secret-value"
	taskID, err := runner.Submit(TaskSpec{
		Record: record,
		Run: func(_ context.Context, execution *Execution) error {
			if err := execution.Progress("pulling-image", 10, "pulling"); err != nil {
				return err
			}
			if err := execution.Progress("pulling-image", 20, "still pulling"); err != nil {
				return err
			}
			if err := execution.Progress("stopping-old", 30, "stopping"); err != nil {
				return err
			}
			return errors.New(secret)
		},
		Recover: func(_ context.Context, _ error) (Recovery, error) {
			return Recovery{Status: "restored", Detail: "old container restored"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.WaitIdle(testContext(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	logs := collector.String()
	ordered := []string{"task.accepted", "task.started", "task.stage", "pulling-image", "task.stage", "stopping-old", "task.recovery_attempt", "task.recovery", "restored", "task.finished"}
	position := 0
	for _, value := range ordered {
		index := strings.Index(logs[position:], value)
		if index < 0 {
			t.Fatalf("lifecycle log missing or out of order %q: %s", value, logs)
		}
		position += index + len(value)
	}
	if strings.Count(logs, "task.stage") != 2 {
		t.Fatalf("repeated stage progress emitted duplicate stage events: %s", logs)
	}
	for _, value := range []string{taskID, "req-42", "old-container-42", "web", "registry.example/app:v2", "sha256:new-image", `"operation":"test"`, `"durationMs":`, `"errorCode":"INTERNAL_ERROR"`, `"error":`} {
		if !strings.Contains(logs, value) {
			t.Fatalf("lifecycle log lacks correlation value %q: %s", value, logs)
		}
	}
	if !strings.Contains(logs, `"status":"failed"`) || !strings.Contains(logs, `"severity":"ERROR"`) {
		t.Fatalf("failed terminal event lacks ERROR severity/status: %s", logs)
	}
	for _, value := range []string{"password", "secret-value"} {
		if strings.Contains(logs, value) {
			t.Fatalf("lifecycle log exposed secret material %q: %s", value, logs)
		}
	}
}

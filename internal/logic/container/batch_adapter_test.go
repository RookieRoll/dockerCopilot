package container

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
)

func TestBatchTaskContinuesAfterConflictAndReportsCounts(t *testing.T) {
	var mu sync.Mutex
	var started []string
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			writeUpdateInspect(w, id)
		case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/start") || strings.HasSuffix(r.URL.Path, "/stop") || strings.HasSuffix(r.URL.Path, "/restart")):
			mu.Lock()
			started = append(started, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 2)
	defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()

	owner, _, err := ctx.ContainerService().ReserveUpdate(context.Background(), "c2", "held-task", "existing-update")
	if err != nil || owner == nil {
		t.Fatalf("reserve conflict fixture: lease=%v err=%v", owner, err)
	}
	if err := owner.Confirm(); err != nil {
		t.Fatal(err)
	}
	defer owner.Release()

	taskID, err := NewBatchLogic(context.Background(), ctx).Submit(&BatchRequest{Action: "start", ContainerIDs: []string{"c1", "c2", "c1"}})
	if err != nil {
		t.Fatal(err)
	}
	record := waitForBatchTask(t, ctx, taskID)
	if record.Status != task.StatusSucceeded || record.SuccessCount != 1 || record.SkippedCount != 1 || record.FailedCount != 0 {
		t.Fatalf("batch result counts/status: %+v", record)
	}
	if len(record.Items) != 2 || record.Items[0].Status != "succeeded" || record.Items[1].Status != "skipped" || !strings.Contains(record.Items[1].Error, "held-task") {
		t.Fatalf("batch item results: %+v", record.Items)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 1 || !strings.HasSuffix(started[0], "/c1/start") {
		t.Fatalf("only unconflicted unique target should run, requests=%v", started)
	}
}

func TestBatchPreflightKeepsCaseSensitiveNamesAndRejectsEmptyInspect(t *testing.T) {
	var mu sync.Mutex
	var started []string
	const upperID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const lowerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			switch id {
			case "Web", upperID:
				writeUpdateInspectWithName(w, upperID, "Web", "old:image")
			case "web", lowerID:
				writeUpdateInspectWithName(w, lowerID, "web", "old:image")
			case "empty":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			default:
				http.NotFound(w, r)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			mu.Lock()
			started = append(started, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 2)
	defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()
	taskID, err := NewBatchLogic(context.Background(), ctx).Submit(&BatchRequest{Action: "start", ContainerIDs: []string{"Web", "web", "empty"}})
	if err != nil {
		t.Fatal(err)
	}
	record := waitForBatchTask(t, ctx, taskID)
	if record.Status != task.StatusFailed || record.SuccessCount != 2 || record.FailedCount != 1 || len(record.Items) != 3 {
		t.Fatalf("case-sensitive targets or empty inspect were mishandled: %+v", record)
	}
	if record.Items[0].Name != "Web" || record.Items[1].Name != "web" || record.Items[2].Status != "failed" || !strings.Contains(record.Items[2].Error, "no container identity") {
		t.Fatalf("unexpected preflight item facts: %+v", record.Items)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 2 || !strings.Contains(started[0], upperID) || !strings.Contains(started[1], lowerID) {
		t.Fatalf("both case-distinct Docker names should reach their own IDs: %v", started)
	}
}

func TestBatchTaskPersistsPartialFailureAndRunsIndependentItems(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			writeUpdateInspect(w, id)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/restart"):
			mu.Lock()
			calls = append(calls, r.URL.Path)
			mu.Unlock()
			if strings.Contains(r.URL.Path, "/c2/") {
				http.Error(w, "daemon refused restart", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 2)
	defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()
	taskID, err := NewBatchLogic(context.Background(), ctx).Submit(&BatchRequest{Action: "restart", ContainerIDs: []string{"c1", "c2"}})
	if err != nil {
		t.Fatal(err)
	}
	record := waitForBatchTask(t, ctx, taskID)
	if record.Status != task.StatusFailed || record.SuccessCount != 1 || record.FailedCount != 1 || record.SkippedCount != 0 {
		t.Fatalf("batch should persist partial failure: %+v", record)
	}
	if record.Items[0].Status != "succeeded" || record.Items[1].Status != "failed" || !strings.Contains(record.Items[1].Error, "daemon refused restart") {
		t.Fatalf("item results lost original failure: %+v", record.Items)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("failure in one item must not prevent remaining items, got %v", calls)
	}
}

func TestBatchTaskContinuesAfterRequestContextDisconnects(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			writeUpdateInspect(w, id)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			close(entered)
			<-release
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 1)
	defer func() {
		releaseOnce.Do(func() { close(release) })
		ctx.TaskRuntime.Close()
		_ = ctx.DockerClient.Close()
	}()
	requestCtx, cancel := context.WithCancel(context.Background())
	taskID, err := NewBatchLogic(requestCtx, ctx).Submit(&BatchRequest{Action: "start", ContainerIDs: []string{"c1"}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("batch action did not start")
	}
	cancel()
	releaseOnce.Do(func() { close(release) })
	record := waitForBatchTask(t, ctx, taskID)
	if record.Status != task.StatusSucceeded || record.SuccessCount != 1 {
		t.Fatalf("request disconnect cancelled accepted batch: %+v", record)
	}
}

func TestBatchRequestValidatesActionAndLimit(t *testing.T) {
	logic := &BatchLogic{}
	if _, err := logic.Submit(&BatchRequest{Action: "remove", ContainerIDs: []string{"c1"}}); err == nil {
		t.Fatal("unsupported action accepted")
	}
	if _, err := logic.Submit(&BatchRequest{Action: "start", ContainerIDs: make([]string, MaxContainerBatchTargets+1)}); err == nil {
		t.Fatal("over-limit batch accepted")
	}
}

func waitForBatchTask(t *testing.T, serviceContext *svc.ServiceContext, taskID string) task.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record, err := serviceContext.TaskStore.Get(taskID)
		if err == nil && record.Status.Terminal() {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	record, err := serviceContext.TaskStore.Get(taskID)
	t.Fatalf("batch task did not settle: task=%s record=%+v err=%v", taskID, record, err)
	return task.Record{}
}

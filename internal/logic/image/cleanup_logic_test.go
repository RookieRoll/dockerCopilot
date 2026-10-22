package image

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dockerClient "github.com/docker/docker/client"
	"github.com/google/uuid"
	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
)

func TestCleanupLogicPersistsPerImageTaskSummary(t *testing.T) {
	var daemon *httptest.Server
	deleted := make(map[string]bool)
	var deletesMu sync.Mutex
	daemon = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/images/json":
			_, _ = w.Write([]byte(`[
				{"Id":"sha256:success","RepoTags":["example/success:v1"],"Created":1,"Size":100},
				{"Id":"sha256:failure","RepoTags":["example/failure:v1"],"Created":2,"Size":200},
				{"Id":"sha256:protected","RepoTags":["example/protected:v1"],"Created":3,"Size":250},
				{"Id":"sha256:used","RepoTags":["example/used:v1"],"Created":4,"Size":300}
			]`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			_, _ = w.Write([]byte(`[{"Id":"container","ImageID":"sha256:used"}]`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/images/"), "/json")
			_, _ = fmt.Fprintf(w, `{"Id":%q,"RepoTags":[%q]}`, id, strings.TrimPrefix(id, "sha256:")+":v1")
		case r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/v1.45/images/")
			if id == "sha256:failure" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"daemon failure"}`))
				return
			}
			deletesMu.Lock()
			deleted[id] = true
			deletesMu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(daemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	store, err := task.Open(task.Options{Dir: filepath.Join(t.TempDir(), "tasks")})
	if err != nil {
		t.Fatal(err)
	}
	protectedTaskID := uuid.NewString()
	if err := store.Create(task.Record{TaskID: protectedTaskID, Type: "container-update", Status: task.StatusQueued, ImageIDs: []string{"sha256:protected"}}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	runner, err := task.NewRunner(store, task.RunnerOptions{MaxConcurrent: 1, QueueCapacity: 4})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	defer func() { runner.Close(); _ = store.Close() }()
	service := &svc.ServiceContext{
		DockerClient: docker, Images: imageService.NewService(docker),
		TaskStore: store, TaskRunner: runner, Mutations: operation.NewMutationGate(),
	}
	resp, err := NewCleanupLogic(context.Background(), service).Submit(&CleanupRequest{Policy: "unused"})
	if err != nil {
		t.Fatalf("submit cleanup: %v", err)
	}
	data, ok := resp.Data.(map[string]string)
	if !ok || data["taskID"] == "" || data["status"] != "queued" {
		t.Fatalf("unexpected acceptance response: %+v", resp)
	}
	var record task.Record
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record, err = store.Get(data["taskID"])
		if err == nil && record.Status.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || record.Status != task.StatusFailed {
		t.Fatalf("partial cleanup should end failed with details, record=%+v err=%v", record, err)
	}
	if record.SuccessCount != 1 || record.FailedCount != 1 || record.SkippedCount != 1 || len(record.Items) != 3 {
		t.Fatalf("task summary counts/items = success %d failed %d skipped %d items %+v", record.SuccessCount, record.FailedCount, record.SkippedCount, record.Items)
	}
	statuses := map[string]string{}
	for _, item := range record.Items {
		statuses[item.ID] = item.Status
	}
	if statuses["sha256:success"] != "succeeded" || statuses["sha256:failure"] != "failed" || statuses["sha256:protected"] != "skipped" {
		t.Fatalf("task item outcomes = %v", statuses)
	}
	for _, item := range record.Items {
		if item.ID == "sha256:protected" && !strings.Contains(item.Error, protectedTaskID) {
			t.Fatalf("protected image outcome omitted owning task id %q: %+v", protectedTaskID, item)
		}
	}
	deletesMu.Lock()
	defer deletesMu.Unlock()
	if !deleted["sha256:success"] || deleted["sha256:failure"] || deleted["sha256:protected"] || deleted["sha256:used"] {
		t.Fatalf("actual deletion results = %v", deleted)
	}
	encoded, err := json.Marshal(task.Public(record))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"failedCount":1`) {
		t.Fatalf("public task response omitted cleanup summary: %s", encoded)
	}
}

func TestCleanupLogicRejectsUnsupportedPolicy(t *testing.T) {
	logic := &CleanupLogic{}
	if _, err := logic.Submit(&CleanupRequest{Policy: "all"}); err == nil {
		t.Fatal("unsupported cleanup policy was accepted")
	}
}

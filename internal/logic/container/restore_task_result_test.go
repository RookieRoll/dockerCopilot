package container

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockerClient "github.com/docker/docker/client"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestRestoreTaskPersistsPerContainerPartialResults(t *testing.T) {
	backupDir := t.TempDir()
	backupData := `[
{"Name":"pull-fails","Config":{"Image":"example/broken:1"},"HostConfig":{}},
{"Name":"create-succeeds","Config":{"Image":"example/good:1"},"HostConfig":{}}
]`
	if err := os.WriteFile(filepath.Join(backupDir, "batch.json"), []byte(backupData), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.45","MinAPIVersion":"1.24"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			if strings.HasSuffix(r.URL.Query().Get("fromImage"), "/broken") {
				http.Error(w, `{"message":"denied"}`, http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, `{"status":"Downloaded"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"Id":"created"}`)
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
	runtime, err := svc.NewTaskRuntime(context.Background(), svc.TaskRuntimeOptions{
		Directory: t.TempDir(), MaxConcurrent: 1, QueueCapacity: 2,
		TaskTimeout: 10 * time.Second, RecoveryTimeout: time.Second,
		Retention: time.Hour, MaxRecords: 10,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	containers := containerService.NewService(docker)
	backups := backupService.NewService(docker, backupDir, containers.Operations())
	ctx := &svc.ServiceContext{
		DockerClient: docker, Containers: containers, Backups: backups,
		TaskRuntime: runtime, TaskStore: runtime.Store, TaskRunner: runtime.Runner, TaskQuery: runtime.Query,
		Mutations: operation.NewMutationGate(),
	}
	resp, err := NewRestoreLogic(context.Background(), ctx).Restore(&types.ContainerRestoreReq{Filename: "batch.json"})
	if err != nil || resp.Code != 200 {
		t.Fatalf("restore request response=%+v err=%v", resp, err)
	}
	data, _ := resp.Data.(map[string]interface{})
	taskID, _ := data["taskID"].(string)
	if taskID == "" {
		t.Fatalf("restore acceptance omitted task ID: %+v", resp)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runtime.Runner.WaitIdle(waitCtx); err != nil {
		t.Fatalf("wait for restore task: %v", err)
	}
	record, err := runtime.Store.Get(taskID)
	if err != nil || record.Status != task.StatusFailed || len(record.Items) != 2 {
		t.Fatalf("restore task status/items=%s/%+v err=%v", record.Status, record.Items, err)
	}
	if record.SuccessCount != 1 || record.FailedCount != 1 || record.SkippedCount != 0 {
		t.Fatalf("unexpected restore counts: success=%d failed=%d skipped=%d", record.SuccessCount, record.FailedCount, record.SkippedCount)
	}
	if record.Items[0].Name != "pull-fails" || record.Items[0].Status != "failed" || record.Items[0].Error == "" || record.Items[1].Name != "create-succeeds" || record.Items[1].Status != "succeeded" {
		t.Fatalf("task did not persist the per-container outcomes: %+v", record.Items)
	}
}

package container

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	dockerClient "github.com/docker/docker/client"
	"github.com/google/uuid"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestQueuedRestoreKeepsBackupFileInUseUntilTaskSettles(t *testing.T) {
	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "queued.json")
	if err := os.WriteFile(backupPath, []byte(`[{"Name":"restore-one","Config":{"Image":"example/app:latest"},"HostConfig":{}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			fmt.Fprintln(w, `{"status":"downloaded"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
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
		TaskTimeout: 10 * time.Second, RecoveryTimeout: time.Second, Retention: time.Hour, MaxRecords: 10,
	}, nil)
	if err != nil {
		t.Fatalf("create task runtime: %v", err)
	}
	defer runtime.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	if _, err := runtime.Runner.Submit(task.TaskSpec{
		Record: task.Record{TaskID: uuid.NewString(), Type: "test-blocker"},
		Run:    func(context.Context, *task.Execution) error { close(started); <-release; return nil },
	}); err != nil {
		t.Fatalf("submit blocker: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocker task did not start")
	}
	containers := containerServiceForRestoreAdmission(docker)
	backups := backupService.NewService(docker, backupDir, containers.Operations())
	ctx := &svc.ServiceContext{
		DockerClient: docker, Containers: containers, Backups: backups,
		TaskRuntime: runtime, TaskStore: runtime.Store, TaskRunner: runtime.Runner, TaskQuery: runtime.Query,
		Mutations: operation.NewMutationGate(),
	}
	response, err := NewRestoreLogic(context.Background(), ctx).Restore(&types.ContainerRestoreReq{Filename: "queued.json"})
	if err != nil || response.Code != http.StatusOK {
		t.Fatalf("submit restore response=%+v err=%v", response, err)
	}
	data, _ := response.Data.(map[string]interface{})
	taskID, _ := data["taskID"].(string)
	if taskID == "" {
		t.Fatalf("restore acceptance omitted task ID: %+v", response)
	}
	if record, getErr := runtime.Store.Get(taskID); getErr != nil || record.Status != task.StatusQueued {
		t.Fatalf("restore should remain queued behind blocker: status=%v err=%v", record.Status, getErr)
	}
	deleted, deleteErr := NewDelRestoreLogic(context.Background(), ctx).DelRestore(&types.DelContainerBackupReq{Filename: "queued.json"})
	var inUse interface{ HTTPStatus() int }
	if !errors.As(deleteErr, &inUse) || inUse.HTTPStatus() != http.StatusConflict || deleted.Code != http.StatusConflict {
		t.Fatalf("deleting queued restore file response=%+v err=%v; want 409", deleted, deleteErr)
	}
	unblock()
	waitCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := runtime.Runner.WaitIdle(waitCtx); err != nil {
		t.Fatalf("wait for restore completion: %v", err)
	}
	if _, err := NewDelRestoreLogic(context.Background(), ctx).DelRestore(&types.DelContainerBackupReq{Filename: "queued.json"}); err != nil {
		t.Fatalf("delete backup after restore settled: %v", err)
	}
	if _, err := os.Stat(backupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup file remains after delete: %v", err)
	}

	composePath := filepath.Join(backupDir, "legacy-compose.yml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDelRestoreLogic(context.Background(), ctx).DelRestore(&types.DelContainerBackupReq{Filename: "legacy-compose.yml"}); err != nil {
		t.Fatalf("delete .yml backup file: %v", err)
	}
}

func containerServiceForRestoreAdmission(docker *dockerClient.Client) *containerService.Service {
	return containerService.NewService(docker)
}

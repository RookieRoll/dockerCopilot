package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestUpdateLogicDeduplicatesEquivalentQueuedTaskAndRejectsDifferentOptions(t *testing.T) {
	pullEntered := make(chan struct{}, 2)
	releasePull := make(chan struct{})
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			writeUpdateInspect(w, id)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			select {
			case pullEntered <- struct{}{}:
			default:
			}
			<-releasePull
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"new-id","Warnings":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 2)
	defer func() { close(releasePull); ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()

	first := NewUpdateLogic(context.Background(), ctx)
	firstResp, err := first.Update(updateReq("c1", "one", "nginx:latest", false))
	if err != nil {
		t.Fatalf("submit first update: %v", err)
	}
	firstID := firstResp.Data.(map[string]string)["taskID"]
	select {
	case <-pullEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("first update never entered image pull")
	}

	queued := NewUpdateLogic(context.Background(), ctx)
	queuedResp, err := queued.Update(updateReq("c2", "two", "nginx:stable", false))
	if err != nil {
		t.Fatalf("submit queued update: %v", err)
	}
	queuedID := queuedResp.Data.(map[string]string)["taskID"]
	if queuedID == firstID {
		t.Fatal("distinct queued update reused the running task ID")
	}

	duplicateResp, err := NewUpdateLogic(context.Background(), ctx).Update(updateReq("c2", "two", "nginx:stable", false))
	if err != nil || duplicateResp.Data.(map[string]string)["taskID"] != queuedID {
		t.Fatalf("equivalent queued update should return accepted task ID %q, resp=%+v err=%v", queuedID, duplicateResp, err)
	}
	_, err = NewUpdateLogic(context.Background(), ctx).Update(updateReq("c2", "two", "nginx:stable", true))
	var conflict interface{ HTTPStatus() int }
	if !errors.As(err, &conflict) || conflict.HTTPStatus() != http.StatusConflict {
		t.Fatalf("different effective options should conflict with queued owner, got %v", err)
	}
}

func TestUpdateLogicReleasesReservationWhenInitialTaskPersistenceFails(t *testing.T) {
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") {
			writeUpdateInspect(w, "c1")
			return
		}
		http.NotFound(w, r)
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 1)
	defer func() { _ = ctx.DockerClient.Close() }()
	ctx.TaskRuntime.Close()
	runner, err := task.NewRunner(failCreateStore{}, task.RunnerOptions{MaxConcurrent: 1, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	ctx.TaskRunner = runner
	ctx.TaskRuntime.Runner = runner

	_, err = NewUpdateLogic(context.Background(), ctx).Update(updateReq("c1", "one", "nginx:latest", false))
	if err == nil {
		t.Fatal("expected initial task persistence failure")
	}
	lease, duplicateID, reserveErr := ctx.ContainerService().ReserveUpdate(context.Background(), "c1", "next-owner", "different-fingerprint")
	if reserveErr != nil || lease == nil || duplicateID != "" {
		t.Fatalf("reservation leaked after failed durable submit: lease=%v duplicate=%q err=%v", lease, duplicateID, reserveErr)
	}
	lease.Release()
}

func TestUpdateLogicPersistsCreatedContainerCheckpoint(t *testing.T) {
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			writeUpdateInspect(w, "c1")
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"new-container-id","Warnings":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 1)
	defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()
	resp, err := NewUpdateLogic(context.Background(), ctx).Update(updateReq("c1", "one", "nginx:latest", false))
	if err != nil {
		t.Fatal(err)
	}
	taskID := resp.Data.(map[string]string)["taskID"]
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record, getErr := ctx.TaskStore.Get(taskID)
		if getErr == nil && record.Status == task.StatusSucceeded {
			if record.NewContainerID != "new-container-id" {
				t.Fatalf("new container ID was not durably checkpointed: %+v", record)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	record, _ := ctx.TaskStore.Get(taskID)
	t.Fatalf("update did not finish with persisted checkpoint: %+v", record)
}

func newUpdateAdapterDaemon(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func newUpdateAdapterContext(t *testing.T, dockerURL string, workers, queue int) *svc.ServiceContext {
	t.Helper()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(dockerURL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := task.Open(task.Options{Dir: filepath.Join(t.TempDir(), "tasks")})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := task.NewRunner(store, task.RunnerOptions{MaxConcurrent: workers, QueueCapacity: queue})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	return &svc.ServiceContext{
		DockerClient: docker,
		Containers:   serviceFromContext(&svc.ServiceContext{DockerClient: docker}),
		TaskStore:    store, TaskRunner: runner,
		TaskRuntime: &svc.TaskRuntime{Store: store, Runner: runner},
	}
}

func writeUpdateInspect(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "application/json")
	name := id
	if id == "c1" {
		name = "one"
	} else if id == "c2" {
		name = "two"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"Id": id, "Name": "/" + name,
		"Config":          map[string]any{"Image": "old:image", "Hostname": "host"},
		"HostConfig":      map[string]any{},
		"NetworkSettings": map[string]any{"Networks": map[string]any{}},
	})
}

func updateReq(id, name, image string, deleteOld bool) *types.ContainerUpdateReq {
	return &types.ContainerUpdateReq{IdReq: types.IdReq{Id: id}, ContainerName: name, ImageNameAndTag: image, DelOldContainer: &deleteOld}
}

type failCreateStore struct{}

func (failCreateStore) Create(task.Record) error {
	return errors.New("intentional persistence failure")
}
func (failCreateStore) Save(task.Record) error { return nil }
func (failCreateStore) Get(string) (task.Record, error) {
	return task.Record{}, errors.New("not found")
}

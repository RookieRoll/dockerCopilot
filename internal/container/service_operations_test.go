package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
)

func newOperationTestService(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	daemon := httptest.NewServer(handler)
	t.Cleanup(daemon.Close)
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(daemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	return NewService(docker)
}

func writeInspectedContainer(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"Id":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","Name":"/web","RestartCount":0,"State":{"Running":true,"Status":"running","StartedAt":"2026-10-22T00:00:00Z"},"Config":{"Image":"old:image"},"HostConfig":{},"NetworkSettings":{"Networks":{}}}`)
}

func testUpdateCallbacks(onCreated func(string) error) UpdateCallbacks {
	return UpdateCallbacks{
		SaveSnapshot: func(snapshot []byte) error {
			if len(snapshot) == 0 {
				return errors.New("empty snapshot")
			}
			return nil
		},
		Checkpoint: func(UpdateCheckpoint) error { return nil },
		OnCreated:  onCreated,
		Validation: UpdateValidationOptions{HealthcheckWait: 20 * time.Millisecond, RunningObservation: 3 * time.Second, PollInterval: 20 * time.Millisecond},
	}
}

func TestBasicMutationsShareIDAndNameLeaseWhileReadOnlyListContinues(t *testing.T) {
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseStart) }) }
	defer release()
	var stopCalls atomic.Int32
	service := newOperationTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			writeInspectedContainer(w)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			close(startEntered)
			<-releaseStart
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			stopCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})

	startErr := make(chan error, 1)
	go func() { startErr <- service.Start(context.Background(), "short-id") }()
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("start did not reach Docker")
	}
	var conflict *operation.ConflictError
	if err := service.Stop(context.Background(), "web"); !errors.As(err, &conflict) || conflict.TaskID == "" {
		t.Fatalf("stop by canonical name should conflict with start by short ID, got %v", err)
	}
	if stopCalls.Load() != 0 {
		t.Fatal("conflicting stop reached Docker")
	}
	if _, err := service.List(context.Background()); err != nil {
		t.Fatalf("read-only list was blocked by active mutation: %v", err)
	}
	release()
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
	if err := service.Stop(context.Background(), "web"); err != nil {
		t.Fatalf("lease was not released after start: %v", err)
	}
}

func TestRenameReservesOldAndNewNamesBeforeCallingDocker(t *testing.T) {
	var renameCalls atomic.Int32
	service := newOperationTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			writeInspectedContainer(w)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			renameCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	other, err := service.Operations().Reserve(operation.Request{TaskID: "other", Kind: "container-stop", Target: operation.Target{Name: "taken"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	var conflict *operation.ConflictError
	if err := service.Rename(context.Background(), "short-id", "taken"); !errors.As(err, &conflict) || conflict.TaskID != "other" {
		t.Fatalf("rename should conflict on new target name before Docker: %v", err)
	}
	if renameCalls.Load() != 0 {
		t.Fatal("conflicting rename reached Docker")
	}
	other.Lease.Release()
	if err := service.Rename(context.Background(), "short-id", "new-name"); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateAddsNewContainerIDAliasBeforeStartingIt(t *testing.T) {
	var service *Service
	startSawAlias := false
	checkpointedID := ""
	service = newOperationTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.45")
			fmt.Fprint(w, "OK")
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			writeInspectedContainer(w)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"status":"Downloaded newer image"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"sha256:resolved-image"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"new-container-id","Warnings":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			startSawAlias = service.Operations().Owns("task-owner", operation.Target{ID: "new-container-id", Name: "web"}) && checkpointedID == "new-container-id"
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	lease, duplicateID, err := service.ReserveUpdate(context.Background(), "short-id", "task-owner", FingerprintUpdate("web", "nginx:latest", false))
	if err != nil || lease == nil || duplicateID != "" {
		t.Fatalf("ReserveUpdate lease=%v duplicate=%q err=%v", lease, duplicateID, err)
	}
	if err := lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdateWithOptions(context.Background(), "short-id", "web", "nginx:latest", false, "task-owner", testUpdateCallbacks(func(newID string) error {
		checkpointedID = newID
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if !startSawAlias {
		t.Fatal("new ID alias and created-ID checkpoint were not completed before the new container start")
	}
	if !service.Operations().Owns("task-owner", operation.Target{ID: "new-container-id", Name: "web"}) {
		t.Fatal("update did not retain new ID and name aliases through task settlement")
	}
	lease.Release()
	if service.Operations().Owns("task-owner", operation.Target{ID: "new-container-id", Name: "web"}) {
		t.Fatal("task lease was not released")
	}
}

func TestUpdateCallsCreatedCheckpointEvenWhenNewIDAliasFails(t *testing.T) {
	var service *Service
	var startCalls atomic.Int32
	service = newOperationTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.45")
			fmt.Fprint(w, "OK")
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			writeInspectedContainer(w)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"status":"Downloaded newer image"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"sha256:resolved-image"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"colliding-new-id","Warnings":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			startCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	lease, _, err := service.ReserveUpdate(context.Background(), "short-id", "task-owner", FingerprintUpdate("web", "nginx:latest", false))
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	other, err := service.Operations().Reserve(operation.Request{TaskID: "other-owner", Kind: "container-stop", Target: operation.Target{ID: "colliding-new-id"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	createdID := ""
	err = service.UpdateWithOptions(context.Background(), "short-id", "web", "nginx:latest", false, "task-owner", testUpdateCallbacks(func(newID string) error {
		createdID = newID
		return nil
	}))
	var conflict *operation.ConflictError
	if !errors.As(err, &conflict) || createdID != "colliding-new-id" {
		t.Fatalf("created fact was lost after alias conflict: id=%q err=%v", createdID, err)
	}
	if startCalls.Load() != 0 {
		t.Fatal("update continued to start the new container after alias conflict")
	}
	if !service.Operations().Owns("other-owner", operation.Target{ID: "colliding-new-id"}) {
		t.Fatal("alias failure affected the existing identity owner")
	}
	lease.Release()
	other.Lease.Release()
}

func TestUpdateFingerprintIsStableAndIncludesEffectiveOptions(t *testing.T) {
	first := FingerprintUpdate("/web", " nginx:latest ", true)
	if first != FingerprintUpdate("web", "nginx:latest", true) {
		t.Fatal("canonical-equivalent update options produced different keys")
	}
	if first == FingerprintUpdate("web", "nginx:latest", false) || first == FingerprintUpdate("web", "nginx:stable", true) {
		t.Fatal("behavior-changing update options were omitted from fingerprint")
	}
	var check any
	if err := json.Unmarshal([]byte(`"`+first+`"`), &check); err != nil {
		t.Fatal(err)
	}
}

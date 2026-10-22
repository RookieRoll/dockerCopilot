package container

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockerClient "github.com/docker/docker/client"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestConfiguredPortsSaveConflictsWithReservedUpdate(t *testing.T) {
	var nameMu sync.RWMutex
	inspectName := "/web"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/c1/json" {
			nameMu.RLock()
			name := inspectName
			nameMu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": "c1", "Name": name,
				"HostConfig": map[string]any{"NetworkMode": "host"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer daemon.Close()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(daemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	ports := settings.NewPortOverridesService(filepath.Join(t.TempDir(), "ports.json"))
	if err := ports.Save("web", []types.ConfiguredPort{{Port: 8080, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	service := containerService.NewService(docker)
	ctx := &svc.ServiceContext{DockerClient: docker, Containers: service, Ports: ports}

	lease, existingID, err := service.ReserveUpdate(context.Background(), "c1", "update-task", "web/nginx:next")
	if err != nil || lease == nil || existingID != "" {
		t.Fatalf("reserve update: lease=%v existing=%q err=%v", lease, existingID, err)
	}
	defer lease.Release()
	if err := lease.Confirm(); err != nil {
		t.Fatalf("confirm update reservation: %v", err)
	}
	if err := lease.AddAlias(operation.Target{ID: "c1", Name: "web-backup-task"}); err != nil {
		t.Fatalf("reserve temporary backup name: %v", err)
	}
	nameMu.Lock()
	inspectName = "/web-backup-task"
	nameMu.Unlock()

	resp, err := NewConfiguredPortsLogic(context.Background(), ctx).Save(&types.ContainerConfiguredPortsReq{
		Id: "c1", Ports: []types.ConfiguredPort{{Port: 9090, Protocol: "tcp"}},
	})
	if err == nil {
		t.Fatalf("save should conflict while update owns the inspected container; got response %+v", resp)
	}
	if resp == nil || resp.Code != http.StatusConflict {
		t.Fatalf("expected HTTP 409 conflict, got response=%+v err=%v", resp, err)
	}
	entries, err := ports.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["web"]) != 1 || entries["web"][0].Port != 8080 {
		t.Fatalf("conflicting save changed original mapping: %#v", entries)
	}
	if _, exists := entries["web-backup-task"]; exists {
		t.Fatalf("conflicting save persisted under temporary name: %#v", entries)
	}
	lease.Release()
	_, saveLease, err := service.ReserveContainerMutation(context.Background(), "c1", "container-configured-ports")
	if err != nil || saveLease == nil {
		t.Fatalf("reservation should become available after update settles: lease=%v err=%v", saveLease, err)
	}
	saveLease.Release()
}

func TestConfiguredPortsReservationRefreshesNameAfterInspectRace(t *testing.T) {
	var mu sync.Mutex
	currentName := "/web"
	firstInspect := true
	firstInspectStarted := make(chan struct{})
	releaseFirstInspect := make(chan struct{})
	var releaseOnce sync.Once
	unblockInspect := func() { releaseOnce.Do(func() { close(releaseFirstInspect) }) }
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/c1/json" {
			mu.Lock()
			name := currentName
			block := firstInspect
			firstInspect = false
			mu.Unlock()
			if block {
				close(firstInspectStarted)
				<-releaseFirstInspect
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": "c1", "Name": name,
				"HostConfig": map[string]any{"NetworkMode": "host"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer daemon.Close()
	defer unblockInspect()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(daemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	service := containerService.NewService(docker)
	type result struct {
		inspect dockertypes.ContainerJSON
		lease   *operation.Lease
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		inspect, lease, err := service.ReserveContainerMutation(context.Background(), "c1", "container-configured-ports")
		resultCh <- result{inspect: inspect, lease: lease, err: err}
	}()
	select {
	case <-firstInspectStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial inspect did not start")
	}

	// Complete a rename after the caller saw /web but before it can reserve.
	reservation, err := service.Operations().Reserve(operation.Request{
		TaskID: "rename-task", Kind: "container-rename", Target: operation.Target{ID: "c1", Name: "web"},
	})
	if err != nil || reservation.Lease == nil {
		t.Fatalf("reserve rename: lease=%v err=%v", reservation.Lease, err)
	}
	if err := reservation.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Lease.AddAlias(operation.Target{ID: "c1", Name: "renamed"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	currentName = "/renamed"
	mu.Unlock()
	reservation.Lease.Release()
	unblockInspect()

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("container reservation did not complete")
	}
	if got.err != nil || got.lease == nil {
		t.Fatalf("reserve after rename: lease=%v err=%v", got.lease, got.err)
	}
	defer got.lease.Release()
	if got.inspect.Name != "renamed" {
		t.Fatalf("reservation returned stale inspect name %q, want renamed", got.inspect.Name)
	}
	_, err = service.Operations().Reserve(operation.Request{
		TaskID: "other-task", Kind: "container-configured-ports", Target: operation.Target{Name: "renamed"},
	})
	var conflict interface{ HTTPStatus() int }
	if !errors.As(err, &conflict) || conflict.HTTPStatus() != http.StatusConflict {
		t.Fatalf("refreshed name was not protected by returned lease, got %v", err)
	}
}

func TestConfiguredPortsMissingContainerBodyCodeMatchesNotFound(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container: missing"}`))
	}))
	defer daemon.Close()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(daemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	ctx := &svc.ServiceContext{
		DockerClient: docker,
		Containers:   containerService.NewService(docker),
		Ports:        settings.NewPortOverridesService(filepath.Join(t.TempDir(), "ports.json")),
	}
	resp, err := NewConfiguredPortsLogic(context.Background(), ctx).Save(&types.ContainerConfiguredPortsReq{Id: "missing"})
	if err == nil {
		t.Fatal("expected missing-container error")
	}
	if resp == nil || resp.Code != http.StatusNotFound {
		t.Fatalf("expected body code 404 to match HTTP status 404, got response=%+v err=%v", resp, err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/container/configured-ports", nil)
	observability.WriteAPIResult(response, request, resp, err)
	var envelope observability.HTTPErrorResponse
	if decodeErr := json.Unmarshal(response.Body.Bytes(), &envelope); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if response.Code != http.StatusNotFound || envelope.Code != http.StatusNotFound {
		t.Fatalf("missing-container HTTP/body status mismatch: HTTP=%d body=%d envelope=%+v", response.Code, envelope.Code, envelope)
	}
}

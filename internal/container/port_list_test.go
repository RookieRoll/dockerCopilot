package container

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestListDetailsDoesNotCleanupAfterOperationChangesDuringSnapshot(t *testing.T) {
	entered := make(chan struct{})
	resume := make(chan struct{})
	var enteredOnce, resumeOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json" {
			enteredOnce.Do(func() { close(entered) })
			<-resume
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	defer resumeOnce.Do(func() { close(resume) })
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(server.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	service := NewService(docker)
	ports := settings.NewPortOverridesService(filepath.Join(t.TempDir(), "ports.json"))
	if err := ports.Save("web", []types.ConfiguredPort{{Port: 8080, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := service.ListDetailsWithProtectedNames(context.Background(), ports, nil)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("container list did not start")
	}
	reservation, err := service.Operations().Reserve(operation.Request{TaskID: "rename-during-list", Kind: "container-rename", Target: operation.Target{Name: "web"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	reservation.Lease.Release()
	resumeOnce.Do(func() { close(resume) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("container list did not finish")
	}
	entries, err := ports.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["web"]) != 1 || entries["web"][0].Port != 8080 {
		t.Fatalf("stale list removed port mapping during a mutation: %#v", entries)
	}
}

func TestListDetailsPreservesSettingsWhenDockerListFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(server.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	service := NewService(docker)
	ports := settings.NewPortOverridesService(filepath.Join(t.TempDir(), "ports.json"))
	if err := ports.Save("web", []types.ConfiguredPort{{Port: 8080, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListDetails(context.Background(), ports); err == nil {
		t.Fatal("expected Docker list error")
	}
	entries, err := ports.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["web"]) != 1 {
		t.Fatalf("failed Docker list removed port mapping: %#v", entries)
	}
}

func TestListDetailsWithoutCleanupPreservesMappings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json" {
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(server.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	service := NewService(docker)
	ports := settings.NewPortOverridesService(filepath.Join(t.TempDir(), "ports.json"))
	if err := ports.Save("temporarily-unknown", []types.ConfiguredPort{{Port: 8080, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListDetailsWithoutCleanupWithOperationVersion(context.Background(), ports, service.Operations().CaptureVersion()); err != nil {
		t.Fatal(err)
	}
	entries, err := ports.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["temporarily-unknown"]) != 1 {
		t.Fatalf("list without durable task protections removed mapping: %#v", entries)
	}
}

func TestListDetailsKeepsConfiguredPortsForStoppedContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[{"Id":"container-id","Names":["/stopped"],"State":"exited","Status":"Exited (0)","Image":"image","Created":1}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/container-id/json":
			fmt.Fprint(w, `{"Id":"container-id","Name":"/stopped","Config":{"Image":"image"},"HostConfig":{"NetworkMode":"host"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(server.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	service := NewService(docker)
	ports := settings.NewPortOverridesService(filepath.Join(t.TempDir(), "ports.json"))
	if err := ports.Save("stopped", []types.ConfiguredPort{{Port: 8080, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	items, err := service.ListDetails(context.Background(), ports)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "exited" || len(items[0].ConfiguredPorts) != 1 || items[0].ConfiguredPorts[0].Port != 8080 {
		t.Fatalf("stopped container lost its configured port mapping: %#v", items)
	}
}

package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	dockerClient "github.com/docker/docker/client"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestRenameReportsPartialFailureWhenPortConfigMigrationFails(t *testing.T) {
	renamed := false
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/c1/json":
			fmt.Fprint(w, `{"Id":"c1","Name":"/before"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/c1/rename":
			renamed = true
			w.WriteHeader(http.StatusNoContent)
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
	path := filepath.Join(t.TempDir(), "ports.json")
	ports := settings.NewPortOverridesService(path)
	if err := ports.Save("before", []types.ConfiguredPort{{Port: 8080, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	if err := ports.Save("after", []types.ConfiguredPort{{Port: 9090, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	service := &svc.ServiceContext{DockerClient: docker, Ports: ports}
	response, err := NewRenameLogic(context.Background(), service).Rename(&types.ContainerRenameReq{IdReq: types.IdReq{Id: "c1"}, NewName: "after"})
	if err == nil {
		t.Fatal("expected port migration error")
	}
	var partial portMigrationError
	if !errors.As(err, &partial) || partial.HTTPStatus() != http.StatusInternalServerError || partial.ErrorCode() != "PORT_CONFIG_MIGRATION_FAILED" {
		t.Fatalf("unexpected migration failure classification: %v", err)
	}
	if !renamed {
		t.Fatal("Docker rename did not complete before port migration failed")
	}
	if response.Code != http.StatusInternalServerError || response.Msg != partial.Error() {
		t.Fatalf("response does not disclose partial completion: %#v", response)
	}
	data, ok := response.Data.(map[string]interface{})
	if !ok || data["renamed"] != true || data["portsMigrated"] != false || data["containerName"] != "after" {
		t.Fatalf("missing partial-result data: %#v", response.Data)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/containers/rename", nil)
	recorder := httptest.NewRecorder()
	observability.WriteAPIResult(recorder, request, response, err)
	var envelope struct {
		ErrorCode string         `json:"errorCode"`
		Data      map[string]any `json:"data"`
	}
	if decodeErr := json.Unmarshal(recorder.Body.Bytes(), &envelope); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if recorder.Code != http.StatusInternalServerError || envelope.ErrorCode != "PORT_CONFIG_MIGRATION_FAILED" || envelope.Data["portsMigrated"] != false {
		t.Fatalf("invalid partial migration HTTP response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	entries, err := ports.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["before"]) != 1 || entries["before"][0].Port != 8080 {
		t.Fatalf("old port mapping was lost after migration failure: %#v", entries)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		RenameIntents map[string]settings.ContainerPortRenameIntent `json:"renameIntents"`
	}
	if err := json.Unmarshal(content, &persisted); err != nil {
		t.Fatal(err)
	}
	intent := persisted.RenameIntents["before"]
	if intent.NewName != "after" || intent.ContainerID != "c1" {
		t.Fatalf("rename recovery intent was not retained: %#v", persisted.RenameIntents)
	}

	// A fresh process whose Docker list has another container under the target
	// name must not move or delete the old mapping.
	listDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[{"Id":"foreign-id","Names":["/after"],"State":"running","Status":"Up","Image":"image","Created":1}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/foreign-id/json":
			fmt.Fprint(w, `{"Id":"foreign-id","Name":"/after","Config":{"Image":"image"},"HostConfig":{"NetworkMode":"host"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer listDaemon.Close()
	listClient, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(listDaemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer listClient.Close()
	newProcessPorts := settings.NewPortOverridesService(path)
	if _, err := containerService.NewService(listClient).ListDetails(context.Background(), newProcessPorts); err != nil {
		t.Fatal(err)
	}
	entries, err = newProcessPorts.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["before"]) != 1 || len(entries["after"]) != 1 {
		t.Fatalf("new process list damaged mappings during unresolved rename: %#v", entries)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted.RenameIntents = nil
	if err := json.Unmarshal(content, &persisted); err != nil {
		t.Fatal(err)
	}
	if intent := persisted.RenameIntents["before"]; intent.NewName != "after" || intent.ContainerID != "c1" {
		t.Fatalf("fresh-process list dropped unresolved rename evidence: %#v", persisted.RenameIntents)
	}
}

func TestRenameRejectsDockerMutationWhenIntentCannotBePersisted(t *testing.T) {
	renamed := false
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/c1/json":
			fmt.Fprint(w, `{"Id":"c1","Name":"/before"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/c1/rename":
			renamed = true
			w.WriteHeader(http.StatusNoContent)
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
	path := filepath.Join(t.TempDir(), "ports.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &svc.ServiceContext{DockerClient: docker, Ports: settings.NewPortOverridesService(path)}
	response, err := NewRenameLogic(context.Background(), service).Rename(&types.ContainerRenameReq{IdReq: types.IdReq{Id: "c1"}, NewName: "after"})
	if err == nil {
		t.Fatal("expected intent persistence failure")
	}
	var classified observability.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorCode() != "PORT_CONFIG_RENAME_INTENT_FAILED" || classified.HTTPStatus() != http.StatusInternalServerError {
		t.Fatalf("unexpected intent failure classification: %v", err)
	}
	if renamed || response.Code != http.StatusInternalServerError {
		t.Fatalf("Docker rename ran without a durable intent: renamed=%v response=%#v", renamed, response)
	}
}

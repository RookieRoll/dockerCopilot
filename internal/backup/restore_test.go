package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

type restoreDaemon struct {
	mu        sync.Mutex
	existing  []string
	pulls     []string
	created   []map[string]json.RawMessage
	started   int
	listCalls int
}

func (d *restoreDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/version":
		fmt.Fprint(w, `{"ApiVersion":"1.45","MinAPIVersion":"1.24"}`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
		d.mu.Lock()
		d.listCalls++
		var entries []string
		for _, name := range d.existing {
			entries = append(entries, fmt.Sprintf(`{"Id":%q,"Names":[%q],"State":"running"}`, "existing-id-"+name, "/"+name))
		}
		d.mu.Unlock()
		fmt.Fprintf(w, "[%s]", strings.Join(entries, ","))
	case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
		image := r.URL.Query().Get("fromImage")
		tag := r.URL.Query().Get("tag")
		d.mu.Lock()
		d.pulls = append(d.pulls, image+":"+tag)
		d.mu.Unlock()
		if strings.HasSuffix(image, "/broken") {
			http.Error(w, `{"message":"pull denied"}`, http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"status":"Downloaded"}`)
	case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.created = append(d.created, payload)
		d.mu.Unlock()
		fmt.Fprint(w, `{"Id":"created-id"}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
		d.mu.Lock()
		d.started++
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func newRestoreTestService(t *testing.T, directory string, daemon *restoreDaemon, coordinator *operation.Coordinator) *Service {
	t.Helper()
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(server.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	return NewService(docker, directory, coordinator)
}

func TestRestorePrevalidatesWholeDocumentBeforeDockerCalls(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "malformed JSON", data: `[{"Name":"unfinished"`},
		{name: "invalid later entry", data: `[{"Name":"valid-first","Config":{"Image":"example/good:1"},"HostConfig":{}},{"Name":"invalid-second","Config":{"Env":["X=1"]},"HostConfig":{}}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			filename := "invalid.json"
			if err := os.WriteFile(filepath.Join(directory, filename), []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			daemon := &restoreDaemon{}
			service := newRestoreTestService(t, directory, daemon, operation.NewCoordinator())
			result, err := service.RestoreWithResults(context.Background(), filename, "task-invalid")
			if err == nil || len(result.Items) != 0 {
				t.Fatalf("invalid document result=%+v err=%v; want validation error before item execution", result, err)
			}
			if daemon.listCalls != 0 || len(daemon.pulls) != 0 || len(daemon.created) != 0 || daemon.started != 0 {
				t.Fatalf("invalid document caused Docker side effects: lists=%d pulls=%v creates=%d starts=%d", daemon.listCalls, daemon.pulls, len(daemon.created), daemon.started)
			}
		})
	}
}

func TestRestoreLegacyJSONPreservesConfigAndDoesNotStart(t *testing.T) {
	directory := t.TempDir()
	fixture, err := os.ReadFile(filepath.Join("..", "utiles", "testdata", "legacy-container-backup.json"))
	if err != nil {
		t.Fatal(err)
	}
	filename := "legacy.json"
	if err := os.WriteFile(filepath.Join(directory, filename), fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := &restoreDaemon{}
	service := newRestoreTestService(t, directory, daemon, operation.NewCoordinator())
	var progress []types.TaskProgress
	service.SetProgressCallbacks(ProgressCallbacks{Update: func(_ string, update types.TaskProgress) { progress = append(progress, update) }})
	result, err := service.RestoreWithResults(context.Background(), filename, "task-legacy")
	if err != nil || len(result.Items) != 1 || result.Items[0].Status != "succeeded" || result.Items[0].Name != "legacy-service" {
		t.Fatalf("legacy restore result=%+v err=%v", result, err)
	}
	if len(daemon.pulls) != 1 || daemon.pulls[0] != "docker.io/example/legacy:1.2" || len(daemon.created) != 1 || daemon.started != 0 {
		t.Fatalf("legacy restore did not pull/create without start: pulls=%v creates=%d starts=%d", daemon.pulls, len(daemon.created), daemon.started)
	}
	var image string
	var env []string
	var hostConfig struct {
		NetworkMode   string `json:"NetworkMode"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	}
	var networking struct {
		EndpointsConfig map[string]json.RawMessage `json:"EndpointsConfig"`
	}
	if json.Unmarshal(daemon.created[0]["Image"], &image) != nil || json.Unmarshal(daemon.created[0]["Env"], &env) != nil || json.Unmarshal(daemon.created[0]["HostConfig"], &hostConfig) != nil || json.Unmarshal(daemon.created[0]["NetworkingConfig"], &networking) != nil {
		t.Fatalf("decode Docker create request failed: %#v", daemon.created[0])
	}
	if image != "example/legacy:1.2" || len(env) != 1 || env[0] != "MODE=compat" || hostConfig.NetworkMode != "bridge" || hostConfig.RestartPolicy.Name != "unless-stopped" || networking.EndpointsConfig["legacy-net"] == nil {
		t.Fatalf("legacy configuration was not preserved in create request: %#v", daemon.created[0])
	}
	if len(progress) == 0 || !progress[len(progress)-1].IsDone || !strings.Contains(progress[len(progress)-1].DetailMsg, "legacy-service恢复成功") {
		t.Fatalf("final progress omitted per-container result: %+v", progress)
	}
}

func TestRestoreReportsNameConflictAndPullFailureAlongsideSuccess(t *testing.T) {
	directory := t.TempDir()
	data := `[
{"Name":"already-there","Config":{"Image":"example/conflict:1"},"HostConfig":{}},
{"Name":"pull-fails","Config":{"Image":"example/broken:1"},"HostConfig":{}},
{"Name":"created-ok","Config":{"Image":"example/good:1","Hostname":"custom-host","Env":["A=B"]},"HostConfig":{"Binds":["/source:/target:ro"],"NetworkMode":"bridge"},"NetworkingConfig":{"EndpointsConfig":{"existing-net":{"Aliases":["created-ok"]}}}}
]`
	filename := "partial.json"
	if err := os.WriteFile(filepath.Join(directory, filename), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := &restoreDaemon{existing: []string{"already-there"}}
	service := newRestoreTestService(t, directory, daemon, operation.NewCoordinator())
	result, err := service.RestoreWithResults(context.Background(), filename, "task-partial")
	if err == nil || len(result.Items) != 3 || result.Items[0].Status != "skipped" || result.Items[1].Status != "failed" || result.Items[2].Status != "succeeded" {
		t.Fatalf("partial restore result=%+v err=%v", result, err)
	}
	if len(daemon.created) != 1 || daemon.started != 0 {
		t.Fatalf("restore should create only the eligible item and never start: creates=%d starts=%d", len(daemon.created), daemon.started)
	}
	var image string
	var hostname string
	var env []string
	if json.Unmarshal(daemon.created[0]["Image"], &image) != nil || json.Unmarshal(daemon.created[0]["Hostname"], &hostname) != nil || json.Unmarshal(daemon.created[0]["Env"], &env) != nil || image != "example/good:1" || hostname != "custom-host" || len(env) != 1 || env[0] != "A=B" {
		t.Fatalf("create request lost configuration: %#v", daemon.created[0])
	}
}

func TestRestoreSkipsContainerOwnedByAnotherOperation(t *testing.T) {
	directory := t.TempDir()
	filename := "owned.json"
	data := `[{"Name":"owned-service","Config":{"Image":"example/owned:1"},"HostConfig":{}}]`
	if err := os.WriteFile(filepath.Join(directory, filename), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator := operation.NewCoordinator()
	active, err := coordinator.Reserve(operation.Request{TaskID: "update-task", Kind: operation.ContainerUpdate, Target: operation.Target{Name: "owned-service"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := active.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	defer active.Lease.Release()
	daemon := &restoreDaemon{}
	service := newRestoreTestService(t, directory, daemon, coordinator)
	result, err := service.RestoreWithResults(context.Background(), filename, "restore-task")
	if err == nil || len(result.Items) != 1 || result.Items[0].Status != "skipped" || !strings.Contains(result.Items[0].Error, "container-update") {
		t.Fatalf("active operation conflict result=%+v err=%v", result, err)
	}
	if len(daemon.pulls) != 0 || len(daemon.created) != 0 || daemon.started != 0 {
		t.Fatalf("restore performed Docker mutation for an owned container: pulls=%v creates=%d starts=%d", daemon.pulls, len(daemon.created), daemon.started)
	}
}

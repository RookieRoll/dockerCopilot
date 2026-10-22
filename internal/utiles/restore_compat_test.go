package utiles

import (
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
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func TestRestoreLegacyBackupCreatesContainerWithoutStarting(t *testing.T) {
	backupDir := t.TempDir()
	fixture, err := os.ReadFile(filepath.Join("testdata", "legacy-container-backup.json"))
	if err != nil {
		t.Fatalf("read legacy backup fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "legacy.json"), fixture, 0o600); err != nil {
		t.Fatalf("write fixture into temp backup dir: %v", err)
	}
	t.Setenv("BACKUP_DIR", backupDir)

	var stateMu sync.Mutex
	var pulled, created, started bool
	var pullQuery string
	var createdConfig map[string]any
	mark := func(update func()) {
		stateMu.Lock()
		defer stateMu.Unlock()
		update()
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1.45/images/create"):
			mark(func() {
				pullQuery = r.URL.RawQuery
				pulled = r.URL.Query().Get("fromImage") == "docker.io/example/legacy" && r.URL.Query().Get("tag") == "1.2"
			})
			fmt.Fprintln(w, `{"status":"Pull complete"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var config map[string]any
			if err := json.Unmarshal(body, &config); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mark(func() {
				created = r.URL.Query().Get("name") == "legacy-service"
				createdConfig = config
			})
			fmt.Fprintln(w, `{"Id":"new-container-id"}`)
		case strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/start"):
			mark(func() { started = true })
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(daemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatalf("create fake Docker client: %v", err)
	}
	defer docker.Close()

	service := &svc.ServiceContext{DockerClient: docker, ProgressStore: make(svc.ProgressStoreType)}
	if err := RestoreContainer(service, "legacy.json", "compat-task"); err != nil {
		t.Fatalf("restore legacy backup: %v", err)
	}
	stateMu.Lock()
	pullOK, createOK, didStart, query, requestConfig := pulled, created, started, pullQuery, createdConfig
	stateMu.Unlock()
	if !pullOK || !createOK {
		t.Fatalf("legacy backup did not pull and create expected container: pulled=%v query=%q created=%v", pullOK, query, createOK)
	}
	if didStart {
		t.Fatal("restore compatibility contract changed: restored containers must remain stopped")
	}
	config, ok := requestConfig["Env"].([]any)
	if !ok || len(config) != 1 || config[0] != "MODE=compat" {
		t.Fatalf("legacy container environment was not preserved in create request: %#v", requestConfig)
	}
	hostConfig, ok := requestConfig["HostConfig"].(map[string]any)
	if !ok || hostConfig["NetworkMode"] != "bridge" {
		t.Fatalf("legacy HostConfig network mode was not preserved: %#v", requestConfig)
	}
	restartPolicy, ok := hostConfig["RestartPolicy"].(map[string]any)
	if !ok || restartPolicy["Name"] != "unless-stopped" {
		t.Fatalf("legacy restart policy was not preserved: %#v", requestConfig)
	}
	networking, ok := requestConfig["NetworkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("legacy NetworkingConfig was not preserved: %#v", requestConfig)
	}
	endpoints, ok := networking["EndpointsConfig"].(map[string]any)
	if !ok {
		t.Fatalf("legacy network endpoints were not preserved: %#v", requestConfig)
	}
	legacyNetwork, ok := endpoints["legacy-net"].(map[string]any)
	if !ok {
		t.Fatalf("legacy network attachment was not preserved: %#v", requestConfig)
	}
	aliases, ok := legacyNetwork["Aliases"].([]any)
	if !ok || len(aliases) != 1 || aliases[0] != "legacy-service" {
		t.Fatalf("legacy network aliases were not preserved: %#v", requestConfig)
	}
	progress, ok := service.GetProgress("compat-task")
	if !ok || !progress.IsDone || progress.Percentage != 100 || !strings.Contains(progress.DetailMsg, "legacy-service恢复成功") {
		t.Fatalf("unexpected successful restore progress fields: %#v, found=%v", progress, ok)
	}
}

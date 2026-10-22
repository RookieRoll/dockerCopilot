package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	dockerBackend "github.com/docker/docker/api/types/backend"
	dockerClient "github.com/docker/docker/client"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
)

const asyncBackupInspect = `{
  "Id":"async-container-id",
  "Name":"/async-service",
  "Config":{"Image":"example/async:1.0","Env":["MODE=test"],"Tty":true},
  "HostConfig":{"NetworkMode":"bridge","Binds":["/srv/test:/data:ro"],"RestartPolicy":{"Name":"unless-stopped"}},
  "NetworkSettings":{"Networks":{}},
  "Mounts":[{"Type":"bind","Source":"/srv/test","Destination":"/data","RW":false}]
}`

type listPause struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *listPause) unblock() { p.once.Do(func() { close(p.release) }) }

type backupTestDaemon struct {
	mu        sync.Mutex
	nextPause *listPause
}

func (d *backupTestDaemon) pauseNextList() *listPause {
	pause := &listPause{entered: make(chan struct{}), release: make(chan struct{})}
	d.mu.Lock()
	d.nextPause = pause
	d.mu.Unlock()
	return pause
}

func (d *backupTestDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/version":
		fmt.Fprint(w, `{"ApiVersion":"1.45","MinAPIVersion":"1.24"}`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
		d.mu.Lock()
		pause := d.nextPause
		d.nextPause = nil
		d.mu.Unlock()
		if pause != nil {
			close(pause.entered)
			<-pause.release
		}
		fmt.Fprint(w, `[{"Id":"async-container-id","Names":["/async-service"],"Image":"example/async:1.0","State":"running"}]`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/async-container-id/json":
		fmt.Fprint(w, asyncBackupInspect)
	default:
		http.NotFound(w, r)
	}
}

type responseEnvelope struct {
	Code int            `json:"code"`
	Msg  string         `json:"msg"`
	Data map[string]any `json:"data"`
}

func TestBackupGETWaitsForCompleteFileAndPOSTReturnsTrackableAcceptance(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		extension   string
		getHandler  func(*svc.ServiceContext) http.HandlerFunc
		postHandler func(*svc.ServiceContext) http.HandlerFunc
	}{
		{name: "JSON", extension: ".json", getHandler: BackupHandler, postHandler: BackupTaskHandler},
		{name: "Compose", extension: ".yaml", getHandler: Backup2composeHandler, postHandler: Backup2composeTaskHandler},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			directory := t.TempDir()
			daemon := &backupTestDaemon{}
			server := httptest.NewServer(daemon)
			t.Cleanup(server.Close)
			docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(server.URL), dockerClient.WithVersion("1.45"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = docker.Close() })
			runtime, err := svc.NewTaskRuntime(context.Background(), svc.TaskRuntimeOptions{
				Directory: t.TempDir(), MaxConcurrent: 1, QueueCapacity: 2,
				TaskTimeout: 10 * time.Second, RecoveryTimeout: time.Second,
				Retention: time.Hour, MaxRecords: 10,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = runtime.Close()
			})
			containers := containerService.NewService(docker)
			ctx := &svc.ServiceContext{
				DockerClient: docker, Containers: containers,
				Backups:     backupService.NewService(docker, directory, containers.Operations()),
				TaskRuntime: runtime, TaskStore: runtime.Store, TaskRunner: runtime.Runner, TaskQuery: runtime.Query,
				Mutations: operation.NewMutationGate(),
			}

			// Legacy GET remains synchronous: while Docker is blocked on its list
			// response, the HTTP call must still be pending and no file is visible.
			getPause := daemon.pauseNextList()
			t.Cleanup(getPause.unblock)
			getDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				response := httptest.NewRecorder()
				scenario.getHandler(ctx).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/container/backup", nil))
				getDone <- response
			}()
			select {
			case <-getPause.entered:
			case <-time.After(5 * time.Second):
				getPause.unblock()
				t.Fatal("legacy GET did not reach the fake daemon")
			}
			select {
			case response := <-getDone:
				t.Fatalf("legacy GET returned before the snapshot completed: %d %s", response.Code, response.Body.String())
			default:
			}
			if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
				t.Fatalf("legacy GET exposed a partial file: entries=%v err=%v", entries, err)
			}
			getPause.unblock()
			var getResponse *httptest.ResponseRecorder
			select {
			case getResponse = <-getDone:
			case <-time.After(5 * time.Second):
				t.Fatal("legacy GET did not finish after the fake daemon resumed")
			}
			if getResponse.Code != http.StatusOK {
				t.Fatalf("legacy GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
			}
			var getEnvelope responseEnvelope
			if err := json.Unmarshal(getResponse.Body.Bytes(), &getEnvelope); err != nil || getEnvelope.Code != http.StatusOK || getEnvelope.Msg != "success" {
				t.Fatalf("legacy GET did not report completed success: response=%+v err=%v", getEnvelope, err)
			}
			getFile := onlyNewBackup(t, directory, scenario.extension, nil)
			if info, err := os.Stat(getFile); err != nil || info.Size() == 0 {
				t.Fatalf("legacy GET success returned before a complete file existed: info=%v err=%v", info, err)
			}

			// New POST responds while Docker is still blocked; its response carries
			// a task ID, and only the terminal task record means export is complete.
			beforePost := map[string]struct{}{filepath.Base(getFile): {}}
			postPause := daemon.pauseNextList()
			t.Cleanup(postPause.unblock)
			postResponse := httptest.NewRecorder()
			scenario.postHandler(ctx).ServeHTTP(postResponse, httptest.NewRequest(http.MethodPost, "/api/container/backup", nil))
			if postResponse.Code != http.StatusOK {
				t.Fatalf("POST acceptance status=%d body=%s", postResponse.Code, postResponse.Body.String())
			}
			var postEnvelope responseEnvelope
			if err := json.Unmarshal(postResponse.Body.Bytes(), &postEnvelope); err != nil || postEnvelope.Code != http.StatusOK || postEnvelope.Msg != "accepted" {
				t.Fatalf("POST did not return task acceptance: response=%+v err=%v", postEnvelope, err)
			}
			taskID, _ := postEnvelope.Data["taskID"].(string)
			if taskID == "" {
				t.Fatalf("POST response omitted taskID: %+v", postEnvelope)
			}
			select {
			case <-postPause.entered:
			case <-time.After(5 * time.Second):
				postPause.unblock()
				t.Fatal("async POST task did not reach the fake daemon")
			}
			if files := listFilesWithExtension(t, directory, scenario.extension); len(files) != 1 {
				t.Fatalf("POST acceptance should precede the second complete file: %v", files)
			}
			record, err := runtime.Store.Get(taskID)
			if err != nil || record.Status.Terminal() {
				t.Fatalf("accepted task should remain queryable and nonterminal while export is blocked: status=%s err=%v", record.Status, err)
			}
			postPause.unblock()
			waitContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := runtime.Runner.WaitIdle(waitContext); err != nil {
				t.Fatalf("wait for async export: %v", err)
			}
			record, err = runtime.Store.Get(taskID)
			if err != nil || record.Status != task.StatusSucceeded {
				t.Fatalf("async export task status=%s err=%v; want succeeded", record.Status, err)
			}
			postFile := onlyNewBackup(t, directory, scenario.extension, beforePost)
			compareCompletedBackups(t, scenario.extension, getFile, postFile)
		})
	}
}

func listFilesWithExtension(t *testing.T, directory, extension string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == extension {
			files = append(files, filepath.Join(directory, entry.Name()))
		}
	}
	return files
}

func onlyNewBackup(t *testing.T, directory, extension string, before map[string]struct{}) string {
	t.Helper()
	var added []string
	for _, path := range listFilesWithExtension(t, directory, extension) {
		if _, exists := before[filepath.Base(path)]; !exists {
			added = append(added, path)
		}
	}
	if len(added) != 1 {
		t.Fatalf("expected one new %s backup, found %v", extension, added)
	}
	return added[0]
}

func compareCompletedBackups(t *testing.T, extension, first, second string) {
	t.Helper()
	firstData, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if extension == ".yaml" {
		if !bytes.Equal(firstData, secondData) {
			t.Fatalf("sync and async Compose formats differ:\n%s\n---\n%s", firstData, secondData)
		}
		return
	}
	var firstConfigs, secondConfigs []dockerBackend.ContainerCreateConfig
	if err := json.Unmarshal(firstData, &firstConfigs); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(secondData, &secondConfigs); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstConfigs, secondConfigs) || len(firstConfigs) != 1 || !strings.Contains(firstConfigs[0].Config.Image, "example/async") {
		t.Fatalf("sync and async JSON formats differ: %#v vs %#v", firstConfigs, secondConfigs)
	}
}

package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/task"
)

func TestBatchUpdatePersistsPrivateSnapshotAndPerItemFacts(t *testing.T) {
	var created int
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path != "/v1.45/containers/json" && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			if id == "new-1" || id == "new-2" {
				name := "one"
				if id == "new-2" {
					name = "two"
				}
				writeUpdateInspectWithName(w, id, name, "nginx:latest")
				return
			}
			imageRef := "nginx:latest"
			if id == "c2" {
				imageRef = "nginx:stable"
			}
			writeUpdateInspectWithName(w, id, map[string]string{"c1": "one", "c2": "two"}[id], imageRef)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			writeUpdateImageInspect(w)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			created++
			id := fmt.Sprintf("new-%d", created)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"Id":%q,"Warnings":[]}`, id)
		default:
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 2)
	defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()
	deleteOld := false
	taskID, err := NewBatchLogic(context.Background(), ctx).Submit(&BatchRequest{Action: "update", ContainerIDs: []string{"c1", "c2"}, DeleteOldContainer: &deleteOld})
	if err != nil {
		t.Fatal(err)
	}
	record := waitForBatchTask(t, ctx, taskID)
	if record.Status != task.StatusSucceeded || record.SuccessCount != 2 || record.FailedCount != 0 {
		t.Fatalf("batch update did not commit both items: %+v", record)
	}
	for index, item := range record.Items {
		if item.Status != "succeeded" || item.Stage != "committed" || item.Recovery.Status != "not-needed" || item.SnapshotRef == "" || item.NewContainerID != fmt.Sprintf("new-%d", index+1) {
			t.Fatalf("batch update item facts=%+v", item)
		}
		snapshot, readErr := ctx.TaskStore.ReadItemSnapshot(taskID, item.SnapshotRef)
		if readErr != nil || !json.Valid(snapshot) {
			t.Fatalf("private item snapshot %d not readable: %v", index, readErr)
		}
	}
	public, err := json.Marshal(task.Public(record))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "snapshotRef") || strings.Contains(string(public), record.Items[0].SnapshotRef) {
		t.Fatalf("public batch task leaked item snapshot reference: %s", public)
	}
}

func TestBatchUpdateCommitCheckpointFailureRestoresOldContainer(t *testing.T) {
	var mu sync.Mutex
	oldName, newID, replacementLabel := "web", "", ""
	oldRunning, replacementRunning, replacementExists := true, false, false
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.45")
			fmt.Fprint(w, "OK")
		case r.Method == http.MethodGet && r.URL.Path != "/v1.45/containers/json" && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			if id == "c1" || (id == oldName && oldName == "web") {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Id": "c1", "Image": "sha256:old-image", "Name": "/" + oldName,
					"State":  map[string]any{"Running": oldRunning, "Status": map[bool]string{true: "running", false: "exited"}[oldRunning], "StartedAt": "2026-10-22T00:00:00Z"},
					"Config": map[string]any{"Image": "nginx:latest", "Hostname": "host"}, "HostConfig": map[string]any{},
					"NetworkSettings": map[string]any{"Networks": map[string]any{}},
				})
				return
			}
			if replacementExists && (id == newID || id == "web") {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Id": newID, "Image": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "Name": "/web",
					"RestartCount": 0,
					"State":        map[string]any{"Running": replacementRunning, "Status": map[bool]string{true: "running", false: "exited"}[replacementRunning], "StartedAt": "2026-10-22T00:00:00Z"},
					"Config":       map[string]any{"Image": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "Labels": map[string]string{"io.dockercopilot.update-task": replacementLabel}},
					"HostConfig":   map[string]any{}, "NetworkSettings": map[string]any{"Networks": map[string]any{}},
				})
				return
			}
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			if replacementExists {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": newID, "Names": []string{"/web"}}})
			} else {
				fmt.Fprint(w, `[]`)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			writeUpdateImageInspect(w)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			oldRunning = false
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			oldName = r.URL.Query().Get("name")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			if config, ok := request["Labels"].(map[string]any); ok {
				replacementLabel, _ = config["io.dockercopilot.update-task"].(string)
			}
			newID, replacementExists = "new-1", true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"new-1","Warnings":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/start")
			if id == "c1" {
				oldRunning = true
			} else if id == "new-1" {
				replacementRunning = true
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.45/containers/new-1":
			replacementExists = false
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 2)
	ctx.TaskRunner.Close()
	records := newFailingBatchRecordStore(ctx.TaskStore, "batch-item-replacement-committed")
	runner, err := task.NewRunner(records, task.RunnerOptions{MaxConcurrent: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx.TaskRunner = runner
	ctx.TaskRuntime.Runner = runner
	defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()
	deleteOld := false
	taskID, err := NewBatchLogic(context.Background(), ctx).Submit(&BatchRequest{Action: "update", ContainerIDs: []string{"c1"}, DeleteOldContainer: &deleteOld})
	if err != nil {
		t.Fatal(err)
	}
	var result task.Record
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var getErr error
		result, getErr = records.Get(taskID)
		if getErr == nil && result.Status.Terminal() && len(result.Items) == 1 && result.Items[0].Recovery.Status == "restored" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !result.Status.Terminal() || len(result.Items) != 1 || result.Items[0].Recovery.Status != "restored" {
		t.Fatalf("batch update task recovery did not settle: %+v", result)
	}
	if result.Status != task.StatusFailed || len(result.Items) != 1 || result.Items[0].Status != "failed" || result.Items[0].Committed || result.Items[0].Recovery.Status != "restored" || result.Items[0].NewContainerID != "new-1" {
		t.Fatalf("nondurable batch commit was promoted or recovery facts lost: %+v", result)
	}
	mu.Lock()
	restoredName, restoredRunning, replacementStillExists := oldName, oldRunning, replacementExists
	mu.Unlock()
	if restoredName != "web" || !restoredRunning || replacementStillExists {
		t.Fatalf("failed commit did not restore old instance conservatively: oldName=%q running=%v newExists=%v", restoredName, restoredRunning, replacementStillExists)
	}
}

type failingBatchRecordStore struct {
	*task.Store
	mu        sync.Mutex
	failStage string
	failed    bool
}

func newFailingBatchRecordStore(delegate *task.Store, stage string) *failingBatchRecordStore {
	return &failingBatchRecordStore{Store: delegate, failStage: stage}
}

func (s *failingBatchRecordStore) Create(record task.Record) error {
	return s.Store.Create(record)
}

func (s *failingBatchRecordStore) Save(record task.Record) error {
	s.mu.Lock()
	if !s.failed && record.Status == task.StatusRunning && record.Stage == s.failStage {
		s.failed = true
		s.mu.Unlock()
		return errors.New("synthetic durable item commit failure")
	}
	s.mu.Unlock()
	return s.Store.Save(record)
}

func (s *failingBatchRecordStore) Get(id string) (task.Record, error) {
	return s.Store.Get(id)
}

func TestBatchUpdatePartialFailureKeepsSuccessfulItemAndReportsPreflightPhase(t *testing.T) {
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path != "/v1.45/containers/json" && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			imageRef := "nginx:latest"
			if id == "c2" {
				imageRef = "nginx:broken"
			}
			writeUpdateInspectWithName(w, id, map[string]string{"c1": "one", "c2": "two"}[id], imageRef)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			if r.URL.Query().Get("fromImage") == "docker.io/library/nginx" && r.URL.Query().Get("tag") == "broken" {
				http.Error(w, "image pull rejected", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			writeUpdateImageInspect(w)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"new-1","Warnings":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 2)
	defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()
	deleteOld := false
	taskID, err := NewBatchLogic(context.Background(), ctx).Submit(&BatchRequest{Action: "update", ContainerIDs: []string{"c1", "c2"}, DeleteOldContainer: &deleteOld})
	if err != nil {
		t.Fatal(err)
	}
	record := waitForBatchTask(t, ctx, taskID)
	if record.Status != task.StatusFailed || record.SuccessCount != 1 || record.FailedCount != 1 || record.Items[0].Status != "succeeded" || record.Items[1].Status != "failed" {
		t.Fatalf("batch update should keep prior success and expose failed pull: %+v", record)
	}
	if record.Items[1].Recovery.Status != "not-needed" || !strings.Contains(record.Items[1].Error, "pull rejected") {
		t.Fatalf("pre-destructive failure facts were not retained: %+v", record.Items[1])
	}
}

func TestBatchUpdateRestoresFailedItemBeforePullingNextImage(t *testing.T) {
	type oldContainer struct {
		name    string
		running bool
	}
	old := map[string]*oldContainer{
		"c1": {name: "one", running: true},
		"c2": {name: "two", running: true},
	}
	var mu sync.Mutex
	var events []string
	addEvent := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	var pulls, creates int
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path != "/v1.45/containers/json" && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			if container, ok := old[id]; ok {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Id": id, "Image": "sha256:old-image", "Name": "/" + container.name, "RestartCount": 0,
					"State":  map[string]any{"Running": container.running, "Status": map[bool]string{true: "running", false: "exited"}[container.running], "StartedAt": "2026-10-22T00:00:00Z"},
					"Config": map[string]any{"Image": "nginx:latest", "Hostname": "host"}, "HostConfig": map[string]any{},
					"NetworkSettings": map[string]any{"Networks": map[string]any{}},
				})
				return
			}
			if id == "new-b" {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"Id":"new-b","Name":"/two","Image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","State":{"Running":true,"Status":"running","StartedAt":"2026-10-22T00:00:00Z","RestartCount":0},"Config":{"Image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"HostConfig":{},"NetworkSettings":{"Networks":{}}}`)
				return
			}
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			pulls++
			if pulls == 1 {
				addEvent("A-pull")
			} else {
				addEvent("B-pull")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			writeUpdateImageInspect(w)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/stop")
			old[id].running = false
			addEvent("A-stop")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/rename")
			if id == "c1" && r.URL.Query().Get("name") == "one" {
				addEvent("A-restore-rename")
			} else if id == "c1" {
				addEvent("A-backup-rename")
			}
			old[id].name = r.URL.Query().Get("name")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/start")
			if container, ok := old[id]; ok {
				container.running = true
				addEvent("A-restore-start")
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			creates++
			if creates == 1 {
				addEvent("A-create-failed")
				http.Error(w, "synthetic create failure", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"new-b","Warnings":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 2)
	defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()
	deleteOld := false
	taskID, err := NewBatchLogic(context.Background(), ctx).Submit(&BatchRequest{Action: "update", ContainerIDs: []string{"c1", "c2"}, DeleteOldContainer: &deleteOld})
	if err != nil {
		t.Fatal(err)
	}
	record := waitForBatchTask(t, ctx, taskID)
	if record.Status != task.StatusFailed || record.Items[0].Recovery.Status != "restored" || record.Items[1].Status != "succeeded" {
		t.Fatalf("unexpected sequential recovery result: %+v", record)
	}
	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	indexOf := func(want string) int {
		for i, event := range got {
			if event == want {
				return i
			}
		}
		return -1
	}
	if indexOf("A-create-failed") < 0 || indexOf("A-restore-start") < 0 || indexOf("B-pull") < 0 || indexOf("A-create-failed") >= indexOf("A-restore-start") || indexOf("A-restore-start") >= indexOf("B-pull") {
		t.Fatalf("batch advanced before item recovery completed: %v", got)
	}
}

func writeUpdateInspectWithName(w http.ResponseWriter, id, name, imageRef string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"Id": id, "Image": "sha256:old-image", "Name": "/" + name,
		"RestartCount":    0,
		"State":           map[string]any{"Running": true, "Status": "running", "StartedAt": "2026-10-22T00:00:00Z"},
		"Config":          map[string]any{"Image": imageRef, "Hostname": "host"},
		"HostConfig":      map[string]any{},
		"NetworkSettings": map[string]any{"Networks": map[string]any{}},
	})
}

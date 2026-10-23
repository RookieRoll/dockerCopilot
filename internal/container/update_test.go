package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockerc "github.com/docker/docker/api/types/container"
	dockermount "github.com/docker/docker/api/types/mount"
	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
)

const updateInspectJSON = `{"Id":"old-canonical-id","Image":"sha256:old-image","Name":"/web","RestartCount":0,"State":{"Running":true,"Status":"running","StartedAt":"2026-10-22T00:00:00Z"},"Config":{"Image":"web:latest","Hostname":"stable-host","Labels":{"custom":"keep","io.dockercopilot.update-task":"previous"},"StopSignal":"SIGTERM","StopTimeout":23,"Volumes":{"/data":{}}},"HostConfig":{"Binds":["/host/config:/config:ro"],"RestartPolicy":{"Name":"always"}},"NetworkSettings":{"Networks":{"bridge":{"Aliases":["web-alias"],"IPAddress":"172.17.0.2","NetworkID":"old-network-id","EndpointID":"old-endpoint-id"}}},"Mounts":[{"Type":"volume","Name":"anonymous-volume-id","Source":"/var/lib/docker/volumes/anonymous-volume-id/_data","Destination":"/data","Mode":"rw","RW":true}]}`

func newUpdateTestService(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(server.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	return NewService(docker)
}

func serveUpdateInspect(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, updateInspectJSON)
}

func TestUpdatePullFailureLeavesOriginalUnmodified(t *testing.T) {
	var stopCalls, renameCalls, createCalls int
	service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.45")
			fmt.Fprint(w, "OK")
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/"):
			serveUpdateInspect(w)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			http.Error(w, "registry unavailable", http.StatusBadGateway)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			stopCalls++
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			renameCalls++
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			createCalls++
		default:
			http.NotFound(w, r)
		}
	})
	callbacks := testUpdateCallbacks(func(string) error { return nil })
	err := service.UpdateWithOptions(context.Background(), "old-canonical-id", "web", "web:next", false, "task-pull-failure", callbacks)
	if err == nil {
		t.Fatal("expected image pull failure")
	}
	if stopCalls != 0 || renameCalls != 0 || createCalls != 0 {
		t.Fatalf("old container was modified after pull failure: stop=%d rename=%d create=%d", stopCalls, renameCalls, createCalls)
	}
}

func TestUpdateRuntimeSnapshotFailuresPreventDockerMutation(t *testing.T) {
	for _, failAt := range []string{"save-snapshot", "snapshot-checkpoint"} {
		t.Run(failAt, func(t *testing.T) {
			var pulls, stops, renames, creates int
			service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/_ping":
					w.Header().Set("API-Version", "1.45")
					fmt.Fprint(w, "OK")
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/"):
					serveUpdateInspect(w)
				case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
					pulls++
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
					stops++
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
					renames++
				case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
					creates++
				default:
					http.NotFound(w, r)
				}
			})
			callbacks := testUpdateCallbacks(func(string) error { return nil })
			if failAt == "save-snapshot" {
				callbacks.SaveSnapshot = func([]byte) error { return errors.New("task runtime is unavailable") }
			} else {
				callbacks.Checkpoint = func(checkpoint UpdateCheckpoint) error {
					if checkpoint.Stage == "snapshot-saved" {
						return errors.New("task runtime is unavailable")
					}
					return nil
				}
			}
			err := service.UpdateWithOptions(context.Background(), "old-canonical-id", "web", "web:next", true, "task-snapshot-failure", callbacks)
			if err == nil {
				t.Fatal("expected injected task-state failure")
			}
			if pulls != 0 || stops != 0 || renames != 0 || creates != 0 {
				t.Fatalf("Docker changed before runtime snapshot was recorded: pull=%d stop=%d rename=%d create=%d", pulls, stops, renames, creates)
			}
		})
	}
}

func TestRecoverUpdateRestoresOnlyVerifiedReplacement(t *testing.T) {
	for _, tc := range []struct {
		name               string
		stage              string
		recordedNewID      string
		candidateLabel     string
		candidateImage     string
		nameOccupant       string
		wantStatus         string
		wantRemove         bool
		wantRename         bool
		wantStart          bool
		wantCheckpoint     bool
		snapshotStopped    bool
		failCheckpoint     bool
		wantError          bool
		multipleCandidates bool
		ignoreStart        bool
	}{
		{name: "owned replacement", recordedNewID: "new-id", candidateLabel: "task-recovery", candidateImage: "sha256:resolved-image-id", nameOccupant: "new-id", wantStatus: "restored", wantRemove: true, wantRename: true, wantStart: true},
		{name: "create response lost", candidateLabel: "task-recovery", candidateImage: "sha256:resolved-image-id", nameOccupant: "new-id", wantStatus: "restored", wantRemove: true, wantRename: true, wantStart: true, wantCheckpoint: true},
		{name: "external target occupant", candidateLabel: "task-recovery", candidateImage: "sha256:resolved-image-id", nameOccupant: "external-id", wantStatus: "unknown", wantCheckpoint: true},
		{name: "image mismatch", candidateLabel: "task-recovery", candidateImage: "sha256:other-image", nameOccupant: "new-id", wantStatus: "unknown"},
		{name: "task label mismatch", recordedNewID: "new-id", candidateLabel: "other-task", candidateImage: "sha256:resolved-image-id", nameOccupant: "new-id", wantStatus: "unknown"},
		{name: "multiple candidates", candidateLabel: "task-recovery", candidateImage: "sha256:resolved-image-id", nameOccupant: "new-id", wantStatus: "unknown", multipleCandidates: true},
		{name: "committed replacement is not rolled back", stage: "replacement-committed", candidateLabel: "task-recovery", candidateImage: "sha256:resolved-image-id", nameOccupant: "new-id", wantStatus: "not-needed"},
		{name: "cleanup stage is not rolled back", stage: "removing-old", candidateLabel: "task-recovery", candidateImage: "sha256:resolved-image-id", nameOccupant: "new-id", wantStatus: "not-needed"},
		{name: "original was stopped", recordedNewID: "new-id", candidateLabel: "task-recovery", candidateImage: "sha256:resolved-image-id", nameOccupant: "new-id", wantStatus: "restored", wantRemove: true, wantRename: true, snapshotStopped: true},
		{name: "start response did not restore running state", recordedNewID: "new-id", candidateLabel: "task-recovery", candidateImage: "sha256:resolved-image-id", nameOccupant: "new-id", wantStatus: "unknown", wantRemove: true, wantRename: true, wantStart: true, ignoreStart: true},
		{name: "recovery checkpoint failure", recordedNewID: "new-id", candidateLabel: "task-recovery", candidateImage: "sha256:resolved-image-id", nameOccupant: "new-id", wantStatus: "unknown", failCheckpoint: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldName := "/web-backup-task"
			oldRunning := false
			candidateExists := true
			var removes, renames, starts int
			service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/_ping":
					w.Header().Set("API-Version", "1.45")
					fmt.Fprint(w, "OK")
				case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
					w.Header().Set("Content-Type", "application/json")
					items := make([]map[string]any, 0, 2)
					if candidateExists {
						items = append(items, map[string]any{"Id": "new-id", "Names": []string{"/web"}, "Image": "web:next", "ImageID": "sha256:resolved-image-id", "Labels": map[string]string{updateTaskLabel: "task-recovery"}, "State": "running", "Status": "Up"})
						if tc.multipleCandidates {
							items = append(items, map[string]any{"Id": "second-id", "Names": []string{"/web"}, "Image": "web:next", "ImageID": "sha256:resolved-image-id", "Labels": map[string]string{updateTaskLabel: "task-recovery"}, "State": "running", "Status": "Up"})
						}
					}
					_ = json.NewEncoder(w).Encode(items)
				case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/old-canonical-id/json":
					fmt.Fprintf(w, `{"Id":"old-canonical-id","Name":%q,"Image":"sha256:old-image","Config":{"Image":"web:latest","StopSignal":"SIGTERM","StopTimeout":23},"HostConfig":{},"State":{"Running":%t}}`, oldName, oldRunning)
				case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/new-id/json":
					if !candidateExists {
						http.Error(w, "no such container", http.StatusNotFound)
						return
					}
					fmt.Fprintf(w, `{"Id":"new-id","Name":"/web","Image":%q,"Config":{"Labels":{"%s":%q}},"HostConfig":{},"State":{"Running":true}}`, tc.candidateImage, updateTaskLabel, tc.candidateLabel)
				case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/second-id/json":
					fmt.Fprintf(w, `{"Id":"second-id","Name":"/web","Image":%q,"Config":{"Labels":{"%s":"task-recovery"}},"HostConfig":{},"State":{"Running":true}}`, tc.candidateImage, updateTaskLabel)
				case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/web/json":
					if tc.nameOccupant == "new-id" && candidateExists {
						fmt.Fprintf(w, `{"Id":"new-id","Name":"/web","Image":%q,"Config":{"Labels":{"%s":%q}},"HostConfig":{},"State":{"Running":true}}`, tc.candidateImage, updateTaskLabel, tc.candidateLabel)
					} else if tc.nameOccupant == "external-id" {
						fmt.Fprintf(w, `{"Id":"external-id","Name":"/web","Image":"sha256:other-image","Config":{"Labels":{"%s":"external"}},"HostConfig":{},"State":{"Running":true}}`, updateTaskLabel)
					} else {
						http.Error(w, "no such container", http.StatusNotFound)
					}
				case r.Method == http.MethodDelete && r.URL.Path == "/v1.45/containers/new-id":
					removes++
					candidateExists = false
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/old-canonical-id/rename":
					renames++
					oldName = "/" + r.URL.Query().Get("name")
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/old-canonical-id/start":
					starts++
					if !tc.ignoreStart {
						oldRunning = true
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			})
			reserved, err := service.Operations().Reserve(operation.Request{TaskID: "task-recovery", Kind: operation.ContainerUpdate, Target: operation.Target{ID: "old-canonical-id", Name: "web"}})
			if err != nil || reserved.Lease == nil {
				t.Fatalf("reserve lease: %+v, %v", reserved, err)
			}
			if err := reserved.Lease.Confirm(); err != nil {
				t.Fatal(err)
			}
			defer reserved.Lease.Release()
			var discoveredID string
			stage := tc.stage
			if stage == "" {
				stage = "created-new"
			}
			snapshot := []byte(updateInspectJSON)
			if tc.snapshotStopped {
				snapshot = []byte(strings.Replace(string(snapshot), `"Running":true`, `"Running":false`, 1))
			}
			result, err := service.RecoverUpdate(context.Background(), "task-recovery", snapshot, UpdateRecoveryState{
				Stage: stage, OldContainerID: "old-canonical-id", NewContainerID: tc.recordedNewID, BackupName: "web-backup-task", ResolvedImageID: "sha256:resolved-image-id",
			}, func(checkpoint UpdateCheckpoint) error {
				if checkpoint.Stage == "recovery-new-discovered" {
					discoveredID = checkpoint.NewContainerID
				}
				if tc.failCheckpoint && checkpoint.Stage == "recovery-removing-new" {
					return errors.New("task runtime is unavailable")
				}
				return nil
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("recovery error=%v wantError=%t", err, tc.wantError)
			}
			if err != nil && !tc.wantError {
				t.Fatal(err)
			}
			if result.Status != tc.wantStatus || (removes > 0) != tc.wantRemove || (renames > 0) != tc.wantRename || (starts > 0) != tc.wantStart {
				t.Fatalf("result=%+v remove=%d rename=%d start=%d", result, removes, renames, starts)
			}
			if (discoveredID == "new-id") != tc.wantCheckpoint {
				t.Fatalf("discovered ID checkpoint=%q want=%t", discoveredID, tc.wantCheckpoint)
			}
		})
	}
}

func TestUpdatePinsResolvedImagePreservesConfigurationAndHonorsDeleteOption(t *testing.T) {
	for _, deleteOld := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete-old-%t", deleteOld), func(t *testing.T) {
			var createBody struct {
				Image      string            `json:"Image"`
				Hostname   string            `json:"Hostname"`
				Labels     map[string]string `json:"Labels"`
				Volumes    map[string]any    `json:"Volumes"`
				HostConfig struct {
					Binds  []string `json:"Binds"`
					Mounts []struct {
						Type     string `json:"Type"`
						Source   string `json:"Source"`
						Target   string `json:"Target"`
						ReadOnly bool   `json:"ReadOnly"`
					} `json:"Mounts"`
					RestartPolicy struct {
						Name string `json:"Name"`
					} `json:"RestartPolicy"`
				} `json:"HostConfig"`
				NetworkingConfig struct {
					EndpointsConfig map[string]struct {
						Aliases    []string `json:"Aliases"`
						NetworkID  string   `json:"NetworkID"`
						EndpointID string   `json:"EndpointID"`
						IPAddress  string   `json:"IPAddress"`
					} `json:"EndpointsConfig"`
				} `json:"NetworkingConfig"`
			}
			var stopSignal, stopTimeout string
			var pullCount, removeCount int
			var backupNamePlanned, renameCalled, renameConfirmed atomic.Bool
			service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/_ping":
					w.Header().Set("API-Version", "1.45")
					fmt.Fprint(w, "OK")
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
					serveUpdateInspect(w)
				case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
					pullCount++
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintln(w, `{"status":"pulled"}`)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"Id":"sha256:resolved-image-id"}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
					stopSignal, stopTimeout = r.URL.Query().Get("signal"), r.URL.Query().Get("t")
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
					if !backupNamePlanned.Load() {
						t.Error("Docker rename ran before backup-name checkpoint")
					}
					renameCalled.Store(true)
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
					if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
						t.Errorf("decode create body: %v", err)
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"Id":"new-canonical-id","Warnings":[]}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/old-canonical-id"):
					removeCount++
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			})
			var previousImageSaved bool
			callbacks := testUpdateCallbacks(func(string) error { return nil })
			callbacks.Checkpoint = func(checkpoint UpdateCheckpoint) error {
				if checkpoint.PreviousImageID == "sha256:old-image" {
					previousImageSaved = true
				}
				if checkpoint.Stage == "renaming-old" {
					if checkpoint.BackupName == "" {
						return errors.New("planned backup name missing")
					}
					backupNamePlanned.Store(true)
				}
				if checkpoint.Stage == "old-renamed" {
					if !renameCalled.Load() {
						return errors.New("rename success checkpoint preceded Docker rename")
					}
					renameConfirmed.Store(true)
				}
				return nil
			}
			err := service.UpdateWithOptions(context.Background(), "old-canonical-id", "web", "web:mutable", deleteOld, "task-pinned-image", callbacks)
			if err != nil {
				t.Fatal(err)
			}
			if !previousImageSaved || pullCount != 1 {
				t.Fatalf("image facts missing: previous=%t pulls=%d", previousImageSaved, pullCount)
			}
			if !backupNamePlanned.Load() || !renameConfirmed.Load() {
				t.Fatal("planned backup name and post-rename confirmation checkpoints were not recorded")
			}
			if createBody.Image != "sha256:resolved-image-id" {
				t.Fatalf("create used mutable reference instead of inspected ID: %q", createBody.Image)
			}
			if createBody.Labels[updateTaskLabel] != "task-pinned-image" {
				t.Fatalf("task ownership label missing from replacement create config: %+v", createBody.Labels)
			}
			if createBody.Labels["custom"] != "keep" {
				t.Fatalf("user container labels were not preserved: %+v", createBody.Labels)
			}
			if createBody.Hostname != "stable-host" {
				t.Fatalf("hostname changed: %q", createBody.Hostname)
			}
			if _, ok := createBody.Volumes["/data"]; !ok {
				t.Fatalf("image-declared volume was not preserved: %+v", createBody.Volumes)
			}
			if len(createBody.HostConfig.Binds) != 1 || createBody.HostConfig.Binds[0] != "/host/config:/config:ro" || createBody.HostConfig.RestartPolicy.Name != "always" {
				t.Fatalf("host config was not preserved: %+v", createBody.HostConfig)
			}
			foundVolume := false
			for _, item := range createBody.HostConfig.Mounts {
				if item.Target == "/data" && item.Source == "anonymous-volume-id" && item.Type == "volume" && !item.ReadOnly {
					foundVolume = true
				}
			}
			if !foundVolume {
				t.Fatalf("observed anonymous volume was not retained: %+v", createBody.HostConfig.Mounts)
			}
			network := createBody.NetworkingConfig.EndpointsConfig["bridge"]
			if len(network.Aliases) != 1 || network.Aliases[0] != "web-alias" || network.NetworkID != "" || network.EndpointID != "" || network.IPAddress != "" {
				t.Fatalf("network declarations/runtime IDs incorrect: %+v", network)
			}
			if stopSignal != "SIGTERM" || stopTimeout != "23" {
				t.Fatalf("original stop options not forwarded: signal=%q timeout=%q", stopSignal, stopTimeout)
			}
			if (removeCount == 1) != deleteOld {
				t.Fatalf("deleteOld=%t, remove calls=%d", deleteOld, removeCount)
			}
		})
	}
}

func TestPreserveObservedVolumesReusesAnonymousMountAndBindDeclarations(t *testing.T) {
	host := &dockerc.HostConfig{
		Mounts:      []dockermount.Mount{{Type: dockermount.TypeVolume, Target: "/mount-style", VolumeOptions: &dockermount.VolumeOptions{NoCopy: true}}},
		Binds:       []string{"/bind-style", "/host/data:/named:ro"},
		VolumesFrom: []string{"source:ro"},
	}
	observed := []dockerc.MountPoint{
		{Type: dockermount.TypeVolume, Name: "mount-existing-volume", Destination: "/mount-style", RW: false},
		{Type: dockermount.TypeVolume, Name: "bind-existing-volume", Destination: "/bind-style", RW: true},
		{Type: dockermount.TypeVolume, Name: "image-declared-volume", Destination: "/image-volume", RW: true},
	}
	got := preserveObservedVolumes(host, observed)
	if len(got.Mounts) != 3 {
		t.Fatalf("expected three stable volume mounts, got %+v", got.Mounts)
	}
	byTarget := make(map[string]dockermount.Mount)
	for _, item := range got.Mounts {
		byTarget[item.Target] = item
	}
	if got.Mounts[0].Source != "mount-existing-volume" || got.Mounts[0].VolumeOptions == nil || !got.Mounts[0].VolumeOptions.NoCopy || !got.Mounts[0].ReadOnly {
		t.Fatalf("existing mount options or source were not preserved: %+v", got.Mounts[0])
	}
	if byTarget["/bind-style"].Source != "bind-existing-volume" || len(got.Binds) != 1 || got.Binds[0] != "/host/data:/named:ro" {
		t.Fatalf("anonymous -v declaration was duplicated or named bind changed: binds=%v mounts=%+v", got.Binds, got.Mounts)
	}
	if byTarget["/image-volume"].Source != "image-declared-volume" || len(got.VolumesFrom) != 1 || got.VolumesFrom[0] != "source:ro" {
		t.Fatalf("image volume or VolumesFrom behavior changed: %+v", got)
	}
}

func TestUpdateUsesDockerDefaultStopOptionsWhenUnset(t *testing.T) {
	var stopSignal, stopTimeout string
	service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.45")
			fmt.Fprint(w, "OK")
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			var record dockertypes.ContainerJSON
			if err := json.Unmarshal([]byte(updateInspectJSON), &record); err != nil {
				t.Fatal(err)
			}
			record.Config.StopSignal, record.Config.StopTimeout = "", nil
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			fmt.Fprint(w, `{"Id":"sha256:resolved-image-id"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			stopSignal, stopTimeout = r.URL.Query().Get("signal"), r.URL.Query().Get("t")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			fmt.Fprint(w, `{"Id":"new-id","Warnings":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	if err := service.UpdateWithOptions(context.Background(), "old-canonical-id", "web", "web:next", false, "task-default-stop", testUpdateCallbacks(func(string) error { return nil })); err != nil {
		t.Fatal(err)
	}
	if stopSignal != "" || stopTimeout != "" {
		t.Fatalf("Docker defaults should be used when unset, got signal=%q timeout=%q", stopSignal, stopTimeout)
	}
}

func TestUpdateCleanupFailureAfterCommitIsSuccessfulWithWarning(t *testing.T) {
	var committed atomic.Bool
	var cleanupCalls atomic.Int32
	var cleanupWarning string
	var finalProgressWarning string
	service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.45")
			fmt.Fprint(w, "OK")
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			if id == "new-id" {
				fmt.Fprint(w, `{"Id":"new-id","Name":"/web","Image":"sha256:resolved-image-id","State":{"Running":true,"Status":"running","StartedAt":"2026-10-22T00:00:00Z"},"Config":{"Image":"sha256:resolved-image-id"},"HostConfig":{},"NetworkSettings":{"Networks":{}}}`)
			} else {
				serveUpdateInspect(w)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			fmt.Fprint(w, `{"Id":"sha256:resolved-image-id"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			fmt.Fprint(w, `{"Id":"new-id","Warnings":[]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.45/containers/old-canonical-id":
			cleanupCalls.Add(1)
			if !committed.Load() {
				t.Error("old container cleanup started before replacement commit checkpoint")
			}
			http.Error(w, "daemon cleanup refused", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	callbacks := testUpdateCallbacks(func(string) error { return nil })
	callbacks.Validation = UpdateValidationOptions{RunningObservation: time.Millisecond, PollInterval: time.Millisecond}
	callbacks.Checkpoint = func(checkpoint UpdateCheckpoint) error {
		if checkpoint.Stage == "replacement-committed" {
			committed.Store(true)
		}
		return nil
	}
	callbacks.Warning = func(message string) { cleanupWarning = message }
	callbacks.Progress = func(stage string, _ int, _ string) error {
		if stage == "completed" {
			return errors.New("final progress save fault")
		}
		return nil
	}
	callbacks.Warning = func(message string) {
		if strings.Contains(message, "最终进度") {
			finalProgressWarning = message
			return
		}
		cleanupWarning = message
	}
	err := service.UpdateWithOptions(context.Background(), "old-canonical-id", "web", "web:next", true, "task-cleanup-warning", callbacks)
	if err != nil {
		t.Fatalf("cleanup failure changed successful replacement into task error: %v", err)
	}
	if !committed.Load() || cleanupCalls.Load() != 1 {
		t.Fatalf("cleanup ordering/calls: committed=%v cleanupCalls=%d", committed.Load(), cleanupCalls.Load())
	}
	if !strings.Contains(cleanupWarning, "old-canonical-id") || !strings.Contains(cleanupWarning, "daemon cleanup refused") {
		t.Fatalf("cleanup failure warning omitted retained old ID or cause: %q", cleanupWarning)
	}
	if !strings.Contains(finalProgressWarning, "final progress save fault") {
		t.Fatalf("post-commit final progress save fault was not downgraded to a warning: %q", finalProgressWarning)
	}
}

func TestUpdateCleanupCancellationDoesNotRollbackCommittedReplacement(t *testing.T) {
	deleteStarted := make(chan struct{})
	var oldStartCalls atomic.Int32
	var cleanupWarning string
	service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.45")
			fmt.Fprint(w, "OK")
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/containers/"), "/json")
			if id == "new-id" {
				fmt.Fprint(w, `{"Id":"new-id","Name":"/web","Image":"sha256:resolved-image-id","State":{"Running":true,"Status":"running","StartedAt":"2026-10-22T00:00:00Z"},"Config":{"Image":"sha256:resolved-image-id"},"HostConfig":{},"NetworkSettings":{"Networks":{}}}`)
			} else {
				serveUpdateInspect(w)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			fmt.Fprint(w, `{"Id":"sha256:resolved-image-id"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			if strings.Contains(r.URL.Path, "old-canonical-id") {
				oldStartCalls.Add(1)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			fmt.Fprint(w, `{"Id":"new-id","Warnings":[]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.45/containers/old-canonical-id":
			close(deleteStarted)
			<-r.Context().Done()
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/"):
			serveUpdateInspect(w)
		default:
			http.NotFound(w, r)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callbacks := testUpdateCallbacks(func(string) error { return nil })
	callbacks.Validation = UpdateValidationOptions{RunningObservation: time.Millisecond, PollInterval: time.Millisecond}
	callbacks.Warning = func(message string) { cleanupWarning = message }
	finished := make(chan error, 1)
	go func() {
		finished <- service.UpdateWithOptions(ctx, "old-canonical-id", "web", "web:next", true, "task-cleanup-deadline", callbacks)
	}()
	select {
	case <-deleteStarted:
		cancel() // Cancel only after cleanup begins; setup speed must not select the phase.
	case <-time.After(time.Second):
		t.Fatal("cleanup request did not start")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("cleanup cancellation changed committed update result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("update did not finish after cleanup cancellation")
	}
	if oldStartCalls.Load() != 0 {
		t.Fatalf("old container was restarted after committed replacement: %d starts", oldStartCalls.Load())
	}
	if !strings.Contains(cleanupWarning, "旧容器仍存在") {
		t.Fatalf("cleanup warning did not report independently inspected old-container state: %q", cleanupWarning)
	}
}

func TestUpdateValidatesHealthyOrContinuouslyRunningReplacement(t *testing.T) {
	tests := []struct {
		name            string
		healthcheck     bool
		healthStatus    string
		healthAfter     int32
		omitHealth      bool
		newHealthcheck  bool
		running         bool
		validationWait  time.Duration
		observationWait time.Duration
		inspectDelay    time.Duration
		restartAfter    int32
		startedAtAfter  int32
		stopAfter       int32
		wantErr         bool
		wantCompleted   string
	}{
		{name: "healthy after starting", healthcheck: true, healthStatus: "starting", healthAfter: 2, running: true, wantCompleted: "healthcheck"},
		{name: "new image introduces healthcheck", healthcheck: true, newHealthcheck: true, healthStatus: "starting", healthAfter: 2, running: true, wantCompleted: "healthcheck"},
		{name: "configured healthcheck status missing", healthcheck: true, omitHealth: true, running: true, wantErr: true},
		{name: "continuous running without healthcheck", running: true, wantCompleted: "仅验证容器持续运行"},
		{name: "healthcheck configured timeout", healthcheck: true, healthStatus: "starting", running: true, validationWait: 8 * time.Millisecond, wantErr: true},
		{name: "healthy result after health deadline", healthcheck: true, healthStatus: "healthy", running: true, validationWait: 8 * time.Millisecond, inspectDelay: 25 * time.Millisecond, wantErr: true},
		{name: "unhealthy", healthcheck: true, healthStatus: "unhealthy", running: true, wantErr: true},
		{name: "immediate exit", running: false, wantErr: true},
		{name: "inspect delayed past validation deadline", running: true, observationWait: 8 * time.Millisecond, inspectDelay: 1200 * time.Millisecond, wantErr: true},
		{name: "final inspect catches late exit", running: true, observationWait: 20 * time.Millisecond, stopAfter: 2, wantErr: true},
		{name: "already restarted before validation", running: true, restartAfter: 1, wantErr: true},
		{name: "restart count changes during observation", running: true, restartAfter: 2, wantErr: true},
		{name: "startedAt changes during observation", running: true, startedAtAfter: 2, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var validationCalls, removeCalls atomic.Int32
			service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/_ping":
					w.Header().Set("API-Version", "1.45")
					fmt.Fprint(w, "OK")
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/old-canonical-id"):
					if tc.healthcheck && !tc.newHealthcheck {
						var original map[string]any
						if err := json.Unmarshal([]byte(updateInspectJSON), &original); err != nil {
							t.Fatal(err)
						}
						original["Config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(original)
					} else {
						serveUpdateInspect(w)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
					fmt.Fprintln(w, `{"status":"pulled"}`)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
					fmt.Fprint(w, `{"Id":"sha256:resolved-image-id"}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"), r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
					fmt.Fprint(w, `{"Id":"new-container-id","Warnings":[]}`)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/new-container-id"):
					call := validationCalls.Add(1)
					if tc.inspectDelay > 0 {
						time.Sleep(tc.inspectDelay)
					}
					state, health := "running", ""
					if !tc.running {
						state = "exited"
					}
					if tc.stopAfter > 0 && call >= tc.stopAfter {
						state = "exited"
					}
					if tc.healthcheck {
						health = tc.healthStatus
						if tc.healthAfter > 0 && call >= tc.healthAfter {
							health = "healthy"
						}
					}
					w.Header().Set("Content-Type", "application/json")
					restartCount := int32(0)
					if tc.restartAfter > 0 && call >= tc.restartAfter {
						restartCount = 1
					}
					startedAt := "2026-10-22T00:00:00Z"
					if tc.startedAtAfter > 0 && call >= tc.startedAtAfter {
						startedAt = "2026-10-22T00:00:01Z"
					}
					if tc.healthcheck && !tc.omitHealth {
						running := tc.running && !(tc.stopAfter > 0 && call >= tc.stopAfter)
						fmt.Fprintf(w, `{"Id":"new-container-id","Name":"/web","RestartCount":%d,"State":{"Running":%t,"Status":%q,"StartedAt":%q,"Health":{"Status":%q}},"Config":{"Image":"sha256:resolved-image-id","Healthcheck":{"Test":["CMD","true"]}},"HostConfig":{},"NetworkSettings":{"Networks":{}}}`, restartCount, running, state, startedAt, health)
					} else {
						running := tc.running && !(tc.stopAfter > 0 && call >= tc.stopAfter)
						fmt.Fprintf(w, `{"Id":"new-container-id","Name":"/web","RestartCount":%d,"State":{"Running":%t,"Status":%q,"StartedAt":%q},"Config":{"Image":"sha256:resolved-image-id"},"HostConfig":{},"NetworkSettings":{"Networks":{}}}`, restartCount, running, state, startedAt)
					}
				case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1.45/containers/"):
					removeCalls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			})
			callbacks := testUpdateCallbacks(func(string) error { return nil })
			wait := tc.validationWait
			if wait == 0 {
				wait = time.Second
			}
			observationWait := tc.observationWait
			if observationWait == 0 {
				observationWait = time.Second
			}
			callbacks.Validation = UpdateValidationOptions{HealthcheckWait: wait, RunningObservation: observationWait, PollInterval: 20 * time.Millisecond}
			completed := ""
			validationStarted := time.Time{}
			callbacks.Progress = func(stage string, _ int, message string) error {
				if stage == "validating-new" && validationStarted.IsZero() {
					validationStarted = time.Now()
				}
				if stage == "completed" {
					completed = message
				}
				return nil
			}
			err := service.UpdateWithOptions(context.Background(), "old-canonical-id", "web", "web:next", true, "task-validation", callbacks)
			if (err != nil) != tc.wantErr {
				t.Fatalf("UpdateWithOptions error=%v wantErr=%t validationCalls=%d", err, tc.wantErr, validationCalls.Load())
			}
			if tc.wantErr {
				if tc.name == "healthcheck configured timeout" && time.Since(validationStarted) > 500*time.Millisecond {
					t.Fatalf("configured health timeout was not respected: %s", time.Since(validationStarted))
				}
				if removeCalls.Load() != 0 {
					t.Fatal("old container was removed before replacement validation succeeded")
				}
				return
			}
			if completed == "" || !strings.Contains(completed, tc.wantCompleted) {
				t.Fatalf("result did not explain validation scope: %q", completed)
			}
			if tc.healthcheck && validationCalls.Load() < 2 {
				t.Fatalf("healthcheck was not polled, calls=%d", validationCalls.Load())
			}
			if !tc.healthcheck && time.Since(validationStarted) < 10*time.Millisecond {
				t.Fatalf("running validation ended before the configured observation window: %s", time.Since(validationStarted))
			}
			if removeCalls.Load() != 1 {
				t.Fatalf("old container cleanup should happen after validation, calls=%d", removeCalls.Load())
			}
		})
	}
}

func TestUpdateKeepsOriginallyStoppedReplacementStopped(t *testing.T) {
	var startCalls atomic.Int32
	var completed string
	service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.45")
			fmt.Fprint(w, "OK")
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/old-canonical-id"):
			var original map[string]any
			if err := json.Unmarshal([]byte(updateInspectJSON), &original); err != nil {
				t.Fatal(err)
			}
			original["State"] = map[string]any{"Running": false, "Status": "exited"}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(original)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			fmt.Fprint(w, `{"Id":"sha256:resolved-image-id"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			fmt.Fprint(w, `{"Id":"new-container-id","Warnings":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			startCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	callbacks := testUpdateCallbacks(func(string) error { return nil })
	callbacks.Progress = func(stage string, _ int, message string) error {
		if stage == "completed" {
			completed = message
		}
		return nil
	}
	if err := service.UpdateWithOptions(context.Background(), "old-canonical-id", "web", "web:next", false, "task-stopped-original", callbacks); err != nil {
		t.Fatal(err)
	}
	if startCalls.Load() != 0 {
		t.Fatal("replacement was started although the original container was stopped")
	}
	if !strings.Contains(completed, "保持停止") || !strings.Contains(completed, "未执行运行验证") {
		t.Fatalf("result did not explain stopped-container behavior: %q", completed)
	}
}

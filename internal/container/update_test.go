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

	dockertypes "github.com/docker/docker/api/types"
	dockerc "github.com/docker/docker/api/types/container"
	dockermount "github.com/docker/docker/api/types/mount"
	dockerClient "github.com/docker/docker/client"
)

const updateInspectJSON = `{"Id":"old-canonical-id","Image":"sha256:old-image","Name":"/web","State":{"Running":true},"Config":{"Image":"web:latest","Hostname":"stable-host","StopSignal":"SIGTERM","StopTimeout":23,"Volumes":{"/data":{}}},"HostConfig":{"Binds":["/host/config:/config:ro"],"RestartPolicy":{"Name":"always"}},"NetworkSettings":{"Networks":{"bridge":{"Aliases":["web-alias"],"IPAddress":"172.17.0.2","NetworkID":"old-network-id","EndpointID":"old-endpoint-id"}}},"Mounts":[{"Type":"volume","Name":"anonymous-volume-id","Source":"/var/lib/docker/volumes/anonymous-volume-id/_data","Destination":"/data","Mode":"rw","RW":true}]}`

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

func TestUpdateCriticalSnapshotFailuresPreventDockerMutation(t *testing.T) {
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
				callbacks.SaveSnapshot = func([]byte) error { return errors.New("snapshot disk full") }
			} else {
				callbacks.Checkpoint = func(checkpoint UpdateCheckpoint) error {
					if checkpoint.Stage == "snapshot-saved" {
						return errors.New("task checkpoint disk full")
					}
					return nil
				}
			}
			err := service.UpdateWithOptions(context.Background(), "old-canonical-id", "web", "web:next", true, "task-snapshot-failure", callbacks)
			if err == nil {
				t.Fatal("expected injected critical persistence failure")
			}
			if pulls != 0 || stops != 0 || renames != 0 || creates != 0 {
				t.Fatalf("Docker changed before snapshot was durable: pull=%d stop=%d rename=%d create=%d", pulls, stops, renames, creates)
			}
		})
	}
}

func TestUpdatePinsResolvedImagePreservesConfigurationAndHonorsDeleteOption(t *testing.T) {
	for _, deleteOld := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete-old-%t", deleteOld), func(t *testing.T) {
			var createBody struct {
				Image      string         `json:"Image"`
				Hostname   string         `json:"Hostname"`
				Volumes    map[string]any `json:"Volumes"`
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
				t.Fatal("planned backup name and post-rename confirmation checkpoints were not persisted")
			}
			if createBody.Image != "sha256:resolved-image-id" {
				t.Fatalf("create used mutable reference instead of inspected ID: %q", createBody.Image)
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

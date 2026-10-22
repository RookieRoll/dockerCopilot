package container

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestBasicContainerOperationsKeepSuccessResponseAndDaemonCalls(t *testing.T) {
	var callsMu sync.Mutex
	calls := map[string]bool{}
	markCall := func(name string, value bool) {
		callsMu.Lock()
		defer callsMu.Unlock()
		calls[name] = value
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/c1/start":
			markCall("start", true)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/c1/stop":
			markCall("stop", true)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/c1/restart":
			markCall("restart", true)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/c1/json":
			fmt.Fprint(w, `{"Id":"c1","Name":"/before"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/c1/rename":
			markCall("rename", r.URL.Query().Get("name") == "after")
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
	service := &svc.ServiceContext{DockerClient: docker}
	t.Setenv("CONTAINER_PORT_OVERRIDES_PATH", filepath.Join(t.TempDir(), "container-port-overrides.json"))

	operations := []struct {
		name string
		run  func() (*types.Resp, error)
	}{
		{"start", func() (*types.Resp, error) {
			return NewStartLogic(context.Background(), service).Start(&types.IdReq{Id: "c1"})
		}},
		{"stop", func() (*types.Resp, error) {
			return NewStopLogic(context.Background(), service).Stop(&types.IdReq{Id: "c1"})
		}},
		{"restart", func() (*types.Resp, error) {
			return NewRestartLogic(context.Background(), service).Restart(&types.IdReq{Id: "c1"})
		}},
		{"rename", func() (*types.Resp, error) {
			return NewRenameLogic(context.Background(), service).Rename(&types.ContainerRenameReq{IdReq: types.IdReq{Id: "c1"}, NewName: "after"})
		}},
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			resp, err := op.run()
			if err != nil {
				t.Fatalf("operation: %v", err)
			}
			encoded, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			var got struct {
				Code int            `json:"code"`
				Msg  string         `json:"msg"`
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.Code != 200 || got.Msg != "success" || got.Data == nil {
				t.Fatalf("unexpected success response fields: %s", encoded)
			}
			callsMu.Lock()
			called := calls[op.name]
			callsMu.Unlock()
			if !called {
				t.Fatalf("expected %s call to reach fake Docker daemon", op.name)
			}
		})
	}
}

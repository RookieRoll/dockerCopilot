package image

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestImageListAndDeleteCompatibilityWithFakeDaemon(t *testing.T) {
	var deleteMu sync.Mutex
	var sawDelete bool
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/images/json":
			fmt.Fprint(w, `[{"Id":"sha256:compat-image","RepoTags":["example/app:v1"],"Created":1700000000,"Size":1048576}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.45/images/sha256:compat-image":
			force := r.URL.Query().Get("force")
			// Docker omits the false-valued force option from the query string.
			deleteMu.Lock()
			sawDelete = force == "" || force == "false" || force == "0"
			deleteMu.Unlock()
			fmt.Fprint(w, `[]`)
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
	checks := module.NewImageCheck()
	checks.Data["sha256:compat-image"] = module.ImageCheckList{Known: true, NeedUpdate: true, Stale: true, Error: "registry timeout"}
	service := &svc.ServiceContext{DockerClient: docker, HubImageInfo: checks}

	listResp, err := NewImagesListLogic(context.Background(), service).ImagesList()
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	encoded, err := json.Marshal(listResp)
	if err != nil {
		t.Fatalf("marshal image list response: %v", err)
	}
	var list struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Tag         string `json:"tag"`
			Size        string `json:"size"`
			InUsed      bool   `json:"inUsed"`
			UpdateCheck struct {
				Known      bool   `json:"known"`
				NeedUpdate bool   `json:"needUpdate"`
				Stale      bool   `json:"stale"`
				Error      string `json:"error"`
			} `json:"updateCheck"`
		} `json:"data"`
	}
	if err := json.Unmarshal(encoded, &list); err != nil {
		t.Fatalf("decode image list response: %v", err)
	}
	if list.Code != 200 || list.Msg != "success" || len(list.Data) != 1 {
		t.Fatalf("unexpected image list contract: %s", encoded)
	}
	if list.Data[0].ID != "sha256:compat-image" || list.Data[0].Name != "example/app" || list.Data[0].Tag != "v1" || list.Data[0].Size != "1 Mb" || list.Data[0].InUsed {
		t.Fatalf("unexpected image fields: %s", encoded)
	}
	if !list.Data[0].UpdateCheck.Known || !list.Data[0].UpdateCheck.NeedUpdate || !list.Data[0].UpdateCheck.Stale || list.Data[0].UpdateCheck.Error != "registry timeout" {
		t.Fatalf("image response hid stale registry failure or prior update fact: %s", encoded)
	}

	removeResp, err := NewRemoveLogic(context.Background(), service).Remove(&types.RemoveImageReq{IdReq: types.IdReq{Id: "sha256:compat-image"}})
	if err != nil {
		t.Fatalf("remove image: %v", err)
	}
	encoded, err = json.Marshal(removeResp)
	if err != nil {
		t.Fatalf("marshal image remove response: %v", err)
	}
	var removed struct {
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(encoded, &removed); err != nil {
		t.Fatalf("decode image remove response: %v", err)
	}
	deleteMu.Lock()
	ordinaryDelete := sawDelete
	deleteMu.Unlock()
	if removed.Code != 200 || removed.Msg != "success" || removed.Data == nil || !ordinaryDelete {
		t.Fatalf("unexpected remove behavior: response=%s non-force-delete=%v", encoded, ordinaryDelete)
	}
}

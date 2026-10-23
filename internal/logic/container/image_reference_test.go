package container

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/task"
)

func TestUpdateRejectsLocalImageIDBeforeAdmission(t *testing.T) {
	daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Docker request: %s", r.URL)
		w.WriteHeader(500)
	})
	defer daemon.Close()
	ctx := newUpdateAdapterContext(t, daemon.URL, 1, 0)
	defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()
	resp, err := NewUpdateLogic(context.Background(), ctx).Update(updateReq("c1", "one", "sha256:"+strings.Repeat("a", 64), false))
	if err == nil || resp == nil || resp.Code != http.StatusBadRequest {
		t.Fatalf("expected actionable 400 before admission: %+v, %v", resp, err)
	}
	records, listErr := ctx.TaskStore.List()
	if listErr != nil || len(records) != 0 {
		t.Fatalf("invalid image created operation: %+v, %v", records, listErr)
	}
}

func TestBatchUpdateUsesRecordedSourceAndRejectsUnknownIDSource(t *testing.T) {
	for _, knownSource := range []bool{true, false} {
		t.Run(fmt.Sprintf("known-source-%t", knownSource), func(t *testing.T) {
			var pulls atomic.Int32
			daemon := newUpdateAdapterDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
					label := ""
					if knownSource {
						label = `,"Labels":{"io.dockercopilot.update-image-ref":"tailscale/tailscale:latest"}`
					}
					fmt.Fprintf(w, `{"Id":"c1","Name":"/one","Image":"sha256:%s","Config":{"Image":"sha256:%s"%s},"HostConfig":{},"State":{"Running":false}}`, strings.Repeat("a", 64), strings.Repeat("a", 64), label)
				case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
					pulls.Add(1)
					if strings.TrimPrefix(r.URL.Query().Get("fromImage"), "docker.io/") != "tailscale/tailscale" || r.URL.Query().Get("tag") != "latest" {
						t.Errorf("pulled wrong registry source: %s", r.URL)
					}
					http.Error(w, "fixture registry unavailable", http.StatusBadGateway)
				default:
					t.Errorf("unexpected Docker mutation: %s %s", r.Method, r.URL)
					w.WriteHeader(500)
				}
			})
			defer daemon.Close()
			ctx := newUpdateAdapterContext(t, daemon.URL, 1, 0)
			defer func() { ctx.TaskRuntime.Close(); _ = ctx.DockerClient.Close() }()
			id, err := NewBatchLogic(context.Background(), ctx).Submit(&BatchRequest{Action: "update", ContainerIDs: []string{"c1"}})
			if err != nil {
				t.Fatal(err)
			}
			record := waitForBatchTask(t, ctx, id)
			if record.Status != task.StatusFailed || len(record.Items) != 1 {
				t.Fatalf("unexpected batch: %+v", record)
			}
			if knownSource {
				if pulls.Load() != 1 || record.Items[0].ImageRef != "tailscale/tailscale:latest" {
					t.Fatalf("recorded source not used: pulls=%d record=%+v", pulls.Load(), record)
				}
			} else if pulls.Load() != 0 || !strings.Contains(record.Items[0].Error, "镜像名称") {
				t.Fatalf("unknown ID source reached registry: pulls=%d record=%+v", pulls.Load(), record)
			}
		})
	}
}

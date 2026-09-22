package module

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dockerImage "github.com/docker/docker/api/types/image"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestImageUpdateDataConcurrentAccess(t *testing.T) {
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/v2/app/manifests/latest":
			w.Header().Set("Docker-Content-Digest", "sha256:remote")
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()
	host := strings.TrimPrefix(registry.URL, "https://")

	img := types.Image{
		Summary: dockerImage.Summary{
			ID:          "sha256:test-image",
			RepoDigests: []string{"app@sha256:local"},
		},
		ImageName: host + "/app",
		ImageTag:  "latest",
	}
	images := []types.Image{img}
	data := NewImageCheck()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				data.CheckUpdate(images)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = data.Get(img.ID)
			}
		}()
	}
	wg.Wait()

	if result, ok := data.Get(img.ID); !ok || !result.NeedUpdate {
		t.Fatalf("digest 不一致后应有 NeedUpdate 结果, got %#v (ok=%v)", result, ok)
	}
}

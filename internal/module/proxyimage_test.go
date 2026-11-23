package module

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RookieRoll/dockerCopilot/internal/types"
	dockerImage "github.com/docker/docker/api/types/image"
	"github.com/google/go-containerregistry/pkg/v1/empty"
)

func TestProxyCheckComparesRegistryDigest(t *testing.T) {
	manifest, err := empty.Image.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := empty.Image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
		case "/v2/example/app/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			_, _ = w.Write(manifest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	img := types.Image{Summary: dockerImage.Summary{ID: "loaded"}, ImageName: "localhost:54321/example/app", ImageTag: "latest"}
	img.RepoDigests = []string{img.ImageName + "@" + digest.String()}
	after := NewImageCheck()
	if err := after.InitSettings(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	after.CheckUpdate([]types.Image{img})
	result, ok := after.Get(img.ID)
	if !ok || result.NeedUpdate {
		t.Fatalf("unexpected digest check: %+v", result)
	}
	img.RepoDigests = nil
	after.CheckUpdate([]types.Image{img})
	result, ok = after.Get(img.ID)
	if !ok || !result.NeedUpdate {
		t.Fatal("missing local digest should indicate an update")
	}
}

func TestProxyOptionsAllowDirectAndRejectInvalid(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	if _, err := ProxyOptions(context.Background(), EnvProxy()); err != nil {
		t.Fatal("direct connection rejected")
	}
	t.Setenv("HTTPS_PROXY", "file://secret-password")
	if _, err := ProxyOptions(context.Background(), EnvProxy()); err == nil || strings.Contains(err.Error(), "secret-password") {
		t.Fatal("invalid proxy must fail without exposing credentials")
	}
}

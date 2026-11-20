package module

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dockerImage "github.com/docker/docker/api/types/image"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestProxyCheckUsesPersistedDigestForLoadedImage(t *testing.T) {
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
	tag, err := name.NewTag(img.ImageName + ":latest")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	before := NewImageCheck()
	before.ProxyStateDir = dir
	if err = before.Remember(tag, img.ID, digest.String()); err != nil {
		t.Fatal(err)
	}
	after := NewImageCheck()
	after.InitSettings(dir, false)
	after.CheckUpdate([]types.Image{img})
	result, ok := after.Get(img.ID)
	if !ok || result.NeedUpdate || result.Digest != digest.String() || result.Reference != tag.Name() {
		t.Fatalf("unexpected check after restart: %+v", result)
	}
	img.ID = "manually-replaced"
	after.CheckUpdate([]types.Image{img})
	result, ok = after.Get(img.ID)
	if !ok || !result.NeedUpdate {
		t.Fatal("record for previous image was reused")
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

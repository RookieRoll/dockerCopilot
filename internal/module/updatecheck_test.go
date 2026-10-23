package module

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dockerImage "github.com/docker/docker/api/types/image"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestPinnedVersionTagPattern(t *testing.T) {
	matched := []string{"v2.2.0", "2.2.0", "1.4", "v1.2.3.4"}
	for _, tag := range matched {
		if !pinnedVersionTagPattern.MatchString(tag) {
			t.Fatalf("expected %q to match pinned version tag pattern", tag)
		}
	}
	unmatched := []string{"latest", "alpine", "v2.0.0-rc1", "2.0.0-beta", "sha-abc", ""}
	for _, tag := range unmatched {
		if pinnedVersionTagPattern.MatchString(tag) {
			t.Fatalf("expected %q not to match pinned version tag pattern", tag)
		}
	}
}

func TestPinnedVersionTagSkipped(t *testing.T) {
	// 服务端一律报错:若锁版本 tag 仍被请求,检查会失败并留下痕迹,以此验证它被跳过
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer registry.Close()
	host := strings.TrimPrefix(registry.URL, "https://")

	img := types.Image{
		Summary:   dockerImage.Summary{ID: "sha256:pinned", RepoDigests: []string{"app@sha256:current"}},
		ImageName: host + "/app",
		ImageTag:  "v2.2.0",
	}
	data := NewImageCheck()
	data.CheckUpdate([]types.Image{img})
	if _, ok := data.Get(img.ID); ok {
		t.Fatal("pinned version tag should be skipped by update check")
	}
}

func TestRollingTagDigestMismatchReportsUpdate(t *testing.T) {
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Docker-Content-Digest", "sha256:remote")
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()
	host := strings.TrimPrefix(registry.URL, "https://")

	img := types.Image{
		Summary:   dockerImage.Summary{ID: "sha256:rolling", RepoDigests: []string{"app@sha256:local", "b@sha256:other"}},
		ImageName: host + "/app",
		ImageTag:  "latest",
	}
	data := NewImageCheck()
	data.CheckUpdate([]types.Image{img})
	result, ok := data.Get(img.ID)
	if !ok || !result.NeedUpdate {
		t.Fatalf("expected update for moved digest, got ok=%v result=%#v", ok, result)
	}
}

func TestContainsDigestMatchesAnyLocalDigest(t *testing.T) {
	if !containsDigest([]string{"a@sha256:one", "b@sha256:two"}, "sha256:two") {
		t.Fatal("expected digest to be found among local repo digests")
	}
	if containsDigest([]string{"a@sha256:one"}, "sha256:three") {
		t.Fatal("expected unknown digest not to match")
	}
}

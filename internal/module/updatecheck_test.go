package module

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dockerImage "github.com/docker/docker/api/types/image"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestVersionGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v2.2.0", "2.1.9", true},
		{"2.10.0", "2.9.9", true},
		{"v1.2.1", "v1.2", true},
		{"v1.2", "v1.2.1", false},
		{"v1.2.1", "v1.2.1", false},
		{"v2.0.0", "", true},
		{"", "v1.0", false},
	}
	for _, c := range cases {
		if got := versionGreater(c.a, c.b); got != c.want {
			t.Fatalf("versionGreater(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestReleaseTagPattern(t *testing.T) {
	matched := []string{"v2.2.0", "2.2.0", "1.4", "v1.2.3.4"}
	for _, tag := range matched {
		if !releaseTagPattern.MatchString(tag) {
			t.Fatalf("expected %q to match release tag pattern", tag)
		}
	}
	unmatched := []string{"latest", "alpine", "v2.0.0-rc1", "2.0.0-beta", "sha-abc", ""}
	for _, tag := range unmatched {
		if releaseTagPattern.MatchString(tag) {
			t.Fatalf("expected %q not to match release tag pattern", tag)
		}
	}
}

func newTagRegistry(t *testing.T, tags string, manifestDigest string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tags))
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestCheckSingleImageDetectsNewerVersion(t *testing.T) {
	registry := newTagRegistry(t, `{"name":"app","tags":["1.0.0","1.1.0","latest","v2.0.0-rc1"]}`, "sha256:current")
	defer registry.Close()
	host := strings.TrimPrefix(registry.URL, "https://")

	img := types.Image{
		Summary:   dockerImage.Summary{ID: "sha256:img-versioned", RepoDigests: []string{"app@sha256:current"}},
		ImageName: host + "/app",
		ImageTag:  "1.0.0",
	}
	data := NewImageCheck()
	data.CheckUpdate([]types.Image{img})

	result, ok := data.Get(img.ID)
	if !ok {
		t.Fatal("missing check result")
	}
	if !result.NeedUpdate || result.LatestVersion != "1.1.0" {
		t.Fatalf("expected update to 1.1.0, got %#v", result)
	}
}

func TestCheckSingleImageNoNewerVersion(t *testing.T) {
	registry := newTagRegistry(t, `{"name":"app","tags":["1.0.0","0.9.0","latest"]}`, "sha256:current")
	defer registry.Close()
	host := strings.TrimPrefix(registry.URL, "https://")

	img := types.Image{
		Summary:   dockerImage.Summary{ID: "sha256:img-uptodate", RepoDigests: []string{"app@sha256:current"}},
		ImageName: host + "/app",
		ImageTag:  "1.0.0",
	}
	data := NewImageCheck()
	data.CheckUpdate([]types.Image{img})

	result, ok := data.Get(img.ID)
	if !ok {
		t.Fatal("missing check result")
	}
	if result.NeedUpdate {
		t.Fatalf("expected no update, got %#v", result)
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

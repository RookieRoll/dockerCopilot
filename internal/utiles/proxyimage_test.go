package utiles

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/client"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/RookieRoll/dockerCopilot/internal/module"
	"github.com/RookieRoll/dockerCopilot/internal/svc"
)

func TestLoadProxyImageUsesCheckedDigest(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	tag, err := name.NewTag(strings.TrimPrefix(server.URL, "http://") + "/demo/image:latest")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &v1.ConfigFile{OS: "linux", Architecture: "arm64"}
	first, err := mutate.ConfigFile(empty.Image, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Write(tag, first); err != nil {
		t.Fatal(err)
	}
	digest, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := first.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Write(tag, empty.Image); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", server.URL)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/ctr/json"):
			io.WriteString(w, `{"Id":"ctr","Image":"old"}`)
		case strings.HasSuffix(r.URL.Path, "/images/old/json"):
			io.WriteString(w, `{"Os":"linux","Architecture":"arm64"}`)
		case strings.HasSuffix(r.URL.Path, "/images/load"):
			file := t.TempDir() + "/image.tar"
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if err = os.WriteFile(file, data, 0600); err != nil {
				t.Error(err)
				return
			}
			img, err := tarball.ImageFromPath(file, &tag)
			if err != nil {
				t.Error(err)
				return
			}
			got, err := img.ConfigName()
			if err != nil || got != expected {
				t.Errorf("loaded %v, want %v", got, expected)
			}
			io.WriteString(w, `{"stream":"loaded"}`)
		case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			json.NewEncoder(w).Encode(map[string]string{"Id": expected.String()})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(daemon.URL, "http://")), client.WithVersion("1.48"), client.WithHTTPClient(daemon.Client()))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	checks := module.NewImageCheck()
	checks.ProxyStateDir = t.TempDir()
	checks.Data["old"] = module.ImageCheckList{Reference: tag.Name(), Digest: digest.String(), NeedUpdate: true}
	s := &svc.ServiceContext{DockerClient: cli, HubImageInfo: checks, ProgressStore: make(svc.ProgressStoreType)}
	got, err := loadProxyImage(context.Background(), s, "ctr", tag.Name(), "test", server.URL)
	if err != nil || got != expected.String() {
		t.Fatalf("load returned %q, %v", got, err)
	}
}

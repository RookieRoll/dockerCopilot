package module

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ref "github.com/distribution/reference"
	dockerImage "github.com/docker/docker/api/types/image"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestImageCheckTLSRejectsUntrustedAndAcceptsConfiguredCA(t *testing.T) {
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodHead && r.URL.Path == "/v2/app/manifests/latest" {
			w.Header().Set(ContentDigestHeader, "sha256:remote")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer registry.Close()
	img := registryImage(registry, "sha256:local")

	defaultClient, err := NewImageCheckWithOptions(ImageCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = defaultClient.CheckUpdateContext(context.Background(), []types.Image{img})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "certificate") {
		t.Fatalf("untrusted registry TLS error = %v, want certificate verification failure", err)
	}
	if result, ok := defaultClient.Get(img.ID); !ok || result.Known || !result.Stale || result.Error == "" {
		t.Fatalf("first TLS failure should be unknown and stale with a reason: ok=%v result=%+v", ok, result)
	}

	caPath := writeServerCA(t, registry)
	trusted, err := NewImageCheckWithOptions(ImageCheckOptions{TrustedCAFile: caPath})
	if err != nil {
		t.Fatalf("load configured trusted CA: %v", err)
	}
	if err := trusted.CheckUpdateContext(context.Background(), []types.Image{img}); err != nil {
		t.Fatalf("digest check with configured CA: %v", err)
	}
	result, ok := trusted.Get(img.ID)
	if !ok || !result.Known || result.Stale || !result.NeedUpdate || result.LastSuccessAt.IsZero() {
		t.Fatalf("trusted registry result = ok:%v %+v", ok, result)
	}
}

func TestImageCheckUsesOneTrustedClientForChallengeAndManifest(t *testing.T) {
	var registry *httptest.Server
	var tokenCalls atomic.Int32
	registry = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test-registry"`, registry.URL))
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/token":
			tokenCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
		case r.Method == http.MethodHead && r.URL.Path == "/v2/app/manifests/latest":
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			w.Header().Set(ContentDigestHeader, "sha256:remote")
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()
	data, err := NewImageCheckWithOptions(ImageCheckOptions{Client: registry.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := data.CheckUpdateContext(context.Background(), []types.Image{registryImage(registry, "sha256:local")}); err != nil {
		t.Fatalf("authenticated digest check: %v", err)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token endpoint calls=%d, want one call using the injected registry client", tokenCalls.Load())
	}
}

func TestImageCheckFailureKeepsLastSuccessAndRecovers(t *testing.T) {
	var fail atomic.Bool
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodHead && r.URL.Path == "/v2/app/manifests/latest" {
			if fail.Load() {
				http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set(ContentDigestHeader, "sha256:remote")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer registry.Close()
	data, err := NewImageCheckWithOptions(ImageCheckOptions{Client: registry.Client(), Freshness: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	img := registryImage(registry, "sha256:local")
	if err := data.CheckUpdateContext(context.Background(), []types.Image{img}); err != nil {
		t.Fatal(err)
	}
	first, ok := data.Get(img.ID)
	if !ok || !first.NeedUpdate || first.Stale || first.LastSuccessAt.IsZero() {
		t.Fatalf("initial successful result = ok:%v %+v", ok, first)
	}

	fail.Store(true)
	err = data.CheckUpdateContext(context.Background(), []types.Image{img})
	if err == nil {
		t.Fatal("registry failure was reported as success")
	}
	stale, ok := data.Get(img.ID)
	if !ok || !stale.Known || !stale.NeedUpdate || !stale.Stale || stale.Error == "" || !stale.LastSuccessAt.Equal(first.LastSuccessAt) {
		t.Fatalf("failed recheck discarded last success or omitted error: ok:%v before:%+v after:%+v", ok, first, stale)
	}

	fail.Store(false)
	if err := data.CheckUpdateContext(context.Background(), []types.Image{img}); err != nil {
		t.Fatalf("registry recovery check: %v", err)
	}
	recovered, _ := data.Get(img.ID)
	if !recovered.Known || !recovered.NeedUpdate || recovered.Stale || recovered.Error != "" || recovered.LastSuccessAt.Before(stale.LastSuccessAt) {
		t.Fatalf("successful retry did not clear stale failure: %+v", recovered)
	}
}

func TestImageCheckDoesNotOverlapAndHonorsContextCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
			}
			return
		}
		if r.Method == http.MethodHead && r.URL.Path == "/v2/app/manifests/latest" {
			w.Header().Set(ContentDigestHeader, "sha256:remote")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer registry.Close()
	data, err := NewImageCheckWithOptions(ImageCheckOptions{Client: registry.Client()})
	if err != nil {
		t.Fatal(err)
	}
	img := registryImage(registry, "sha256:local")
	firstDone := make(chan error, 1)
	go func() { firstDone <- data.CheckUpdateContext(context.Background(), []types.Image{img}) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("registry request did not start")
	}
	if err := data.CheckUpdateContext(context.Background(), []types.Image{img}); !errors.Is(err, ErrImageCheckInProgress) {
		t.Fatalf("overlapping check error=%v, want ErrImageCheckInProgress", err)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := data.CheckUpdateContext(cancelCtx, []types.Image{img}); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-canceled context error=%v, want context.Canceled", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("unblocked check failed: %v", err)
	}

	blocked := make(chan struct{})
	cancelRegistry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			close(blocked)
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	defer cancelRegistry.Close()
	cancelData, err := NewImageCheckWithOptions(ImageCheckOptions{Client: cancelRegistry.Client()})
	if err != nil {
		t.Fatal(err)
	}
	cancelImage := registryImage(cancelRegistry, "sha256:local")
	ctx, cancelRequest := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelRequest()
	err = cancelData.CheckUpdateContext(ctx, []types.Image{cancelImage})
	if err == nil {
		t.Fatal("deadline expiry was reported as a successful check")
	}
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("canceled request was not dispatched to registry")
	}
}

func TestImageCheckConstructionRejectsInvalidCA(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(caPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewImageCheckWithOptions(ImageCheckOptions{TrustedCAFile: caPath}); err == nil {
		t.Fatal("invalid configured CA was silently ignored")
	}
}

func TestBearerChallengePreservesRealmPathCase(t *testing.T) {
	imageRef, err := ref.ParseNormalizedNamed("example.com/team/app")
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetAuthURL(`bEaReR realm="https://auth.example.com/TokenCase",service="registry"`, imageRef)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/TokenCase" || got.Query().Get("service") != "registry" {
		t.Fatalf("parsed Bearer realm URL = %q, want path case preserved and service query", got.String())
	}
}

func TestManifestFallsBackFromHeadToGetAndHashesBody(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"config":{"digest":"sha256:config"}}`)
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(manifest)
			return
		}
		http.NotFound(w, r)
	}))
	defer registry.Close()
	got, err := getDigestWithClient(context.Background(), registry.Client(), registry.URL+"/manifest", "")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	if got != want {
		t.Fatalf("GET fallback digest=%q, want %q", got, want)
	}
}

func TestImageCheckGetMarksExpiredSuccessStale(t *testing.T) {
	data := NewImageCheck()
	data.mu.Lock()
	data.Data["sha256:expired"] = ImageCheckList{NeedUpdate: true, Known: true, LastSuccessAt: time.Now().Add(-3 * time.Hour)}
	data.mu.Unlock()
	result, ok := data.Get("sha256:expired")
	if !ok || !result.Known || !result.NeedUpdate || !result.Stale {
		t.Fatalf("expired result should preserve update fact and show stale: ok=%v result=%+v", ok, result)
	}
}

func registryImage(registry *httptest.Server, localDigest string) types.Image {
	host := strings.TrimPrefix(registry.URL, "https://")
	return types.Image{
		Summary:   dockerImage.Summary{ID: "sha256:registry-test", RepoDigests: []string{"app@" + localDigest}},
		ImageName: host + "/app", ImageTag: "latest",
	}
}

func writeServerCA(t *testing.T, registry *httptest.Server) string {
	t.Helper()
	cert := registry.Certificate()
	if _, err := x509.ParseCertificate(cert.Raw); err != nil {
		t.Fatalf("parse httptest CA cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "registry-ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

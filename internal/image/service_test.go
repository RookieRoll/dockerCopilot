package image

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dockerClient "github.com/docker/docker/client"
)

type fixedProtection struct {
	images ProtectedImages
	err    error
}

func (p fixedProtection) ProtectedImages(context.Context) (ProtectedImages, error) {
	return p.images, p.err
}

func newTestImageService(t *testing.T, handler http.HandlerFunc) (*Service, func()) {
	t.Helper()
	daemon := httptest.NewServer(handler)
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(daemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		daemon.Close()
		t.Fatalf("create fake Docker client: %v", err)
	}
	return NewService(docker), func() {
		_ = docker.Close()
		daemon.Close()
	}
}

func TestRemoveProtectsActiveUpdateImageIDsAndTagsEvenWithForce(t *testing.T) {
	var mu sync.Mutex
	var deletes []string
	service, closeService := newTestImageService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"sha256:resolved-image","RepoTags":["example/app:latest"],"RepoDigests":[]}`)
		case r.Method == http.MethodDelete:
			mu.Lock()
			deletes = append(deletes, r.URL.Path)
			mu.Unlock()
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeService()
	service.SetProtectionSource(fixedProtection{images: ProtectedImages{
		IDs:        map[string][]string{"old-image": {"update-task-old"}, "resolved-image": {"update-task-new"}},
		References: map[string][]string{NormalizeReference("example/app:latest"): {"update-task-tag"}},
	}})
	for _, id := range []string{"sha256:old-image", "sha256:resolved-image", "example/app:latest"} {
		if err := service.Remove(context.Background(), id, true); err == nil {
			t.Fatalf("Remove(%q, force=true) unexpectedly succeeded", id)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deletes) != 0 {
		t.Fatalf("protected images were sent to Docker delete: %v", deletes)
	}
}

func TestRemovePreservesExplicitForceAndDoesNotRetryAsForce(t *testing.T) {
	var mu sync.Mutex
	var forces []string
	service, closeService := newTestImageService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"sha256:ordinary","RepoTags":["example/ordinary:v1"]}`)
		case r.Method == http.MethodDelete:
			mu.Lock()
			forces = append(forces, r.URL.Query().Get("force"))
			mu.Unlock()
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeService()
	service.SetProtectionSource(fixedProtection{})
	if err := service.Remove(context.Background(), "sha256:ordinary", false); err != nil {
		t.Fatalf("ordinary delete: %v", err)
	}
	if err := service.Remove(context.Background(), "sha256:ordinary", true); err != nil {
		t.Fatalf("explicit force delete: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(forces) != 2 || forces[0] != "" || forces[1] != "1" {
		t.Fatalf("Docker force values = %v, want ordinary then explicit force", forces)
	}
}

func TestRemoveFailsClosedWhenTaskProtectionCannotBeRead(t *testing.T) {
	var deleteCount int
	service, closeService := newTestImageService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCount++
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	})
	defer closeService()
	service.SetProtectionSource(fixedProtection{err: fmt.Errorf("task records unreadable")})
	if err := service.Remove(context.Background(), "sha256:unknown", false); err == nil {
		t.Fatal("Remove succeeded with unavailable task protection data")
	}
	if deleteCount != 0 {
		t.Fatalf("Docker delete called %d times after protection lookup failed", deleteCount)
	}
}

func TestRemoveMapsDockerConflictToOrdinaryImageInUseError(t *testing.T) {
	var deleteCalls int
	service, closeService := newTestImageService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"sha256:ordinary","RepoTags":["example/ordinary:v1"]}`)
		case r.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"message":"image is being used by a container"}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeService()
	service.SetProtectionSource(fixedProtection{})

	err := service.Remove(context.Background(), "sha256:ordinary", false)
	var conflict interface {
		HTTPStatus() int
		ErrorCode() string
	}
	if !errors.As(err, &conflict) || conflict.HTTPStatus() != http.StatusConflict || conflict.ErrorCode() != "IMAGE_IN_USE" {
		t.Fatalf("ordinary Docker conflict = %v, want IMAGE_IN_USE HTTP 409", err)
	}
	if deleteCalls != 1 {
		t.Fatalf("Docker delete calls=%d, want one non-force attempt", deleteCalls)
	}
}

func TestCleanupRefreshesUsageAndContinuesAfterPerImageConflict(t *testing.T) {
	var mu sync.Mutex
	deleted := make(map[string]bool)
	service, closeService := newTestImageService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/images/json":
			fmt.Fprint(w, `[{"Id":"sha256:used","RepoTags":["example/used:v1"],"Created":1,"Size":20},{"Id":"sha256:blocked","RepoTags":["example/blocked:v1"],"Created":2,"Size":30},{"Id":"sha256:failed","RepoTags":["example/failed:v1"],"Created":3,"Size":35},{"Id":"sha256:free","RepoTags":["example/free:v1"],"Created":4,"Size":40}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[{"Id":"container","ImageID":"sha256:used"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/images/"), "/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"message":"no inspect for %s"}`, id)
		case r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/v1.45/images/")
			mu.Lock()
			defer mu.Unlock()
			if id == "sha256:blocked" {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"message":"image is being used"}`)
				return
			}
			if id == "sha256:failed" {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"message":"daemon failed to remove image"}`)
				return
			}
			deleted[id] = true
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeService()
	service.SetProtectionSource(fixedProtection{images: ProtectedImages{IDs: map[string][]string{"blocked": {"active-update"}}}})
	result, err := service.Cleanup(context.Background(), CleanupUnused)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if result.Total != 3 || result.Deleted != 1 || result.Skipped != 1 || result.Failed != 1 || len(result.Items) != 3 {
		t.Fatalf("unexpected cleanup result: %+v", result)
	}
	if result.Items[0].Status != "skipped" || result.Items[0].TaskID != "active-update" || result.Items[1].Status != "failed" || result.Items[2].Status != "succeeded" {
		t.Fatalf("cleanup item results lost conflict/success details: %+v", result.Items)
	}
	mu.Lock()
	defer mu.Unlock()
	if deleted["sha256:used"] || !deleted["sha256:free"] || deleted["sha256:blocked"] || deleted["sha256:failed"] {
		t.Fatalf("cleanup deletion set=%v", deleted)
	}
}

func TestDanglingCleanupDoesNotForceUsedImages(t *testing.T) {
	var force string
	service, closeService := newTestImageService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/images/json":
			fmt.Fprint(w, `[{"Id":"sha256:dangling","RepoTags":["<none>:<none>"],"Created":1,"Size":1}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[{"Id":"container","ImageID":"sha256:dangling"}]`)
		case r.Method == http.MethodDelete:
			force = r.URL.Query().Get("force")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"message":"image is being used"}`)
		default:
			http.NotFound(w, r)
		}
	})
	defer closeService()
	result, err := service.Cleanup(context.Background(), CleanupDangling)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if result.Skipped != 1 || result.Deleted != 0 || force != "" {
		t.Fatalf("cleanup=%+v Docker force=%q; want one skipped and no force", result, force)
	}
}

func TestNormalizeReferenceSuppliesDockerDefaults(t *testing.T) {
	if got, want := NormalizeReference("nginx"), "docker.io/library/nginx:latest"; got != want {
		t.Fatalf("NormalizeReference(nginx)=%q, want %q", got, want)
	}
}

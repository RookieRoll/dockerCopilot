package container

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"
)

func TestUpdateImageReferenceValidation(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, value := range []string{"tailscale/tailscale:latest", "registry.example:5000/app:v1", "tailscale/tailscale@sha256:" + digest, "nginx"} {
		if err := ValidateUpdateImageReference(value); err != nil {
			t.Errorf("valid reference %q: %v", value, err)
		}
	}
	for _, value := range []string{"", "sha256:" + digest, digest, "https://registry.example/app", "bad image:latest"} {
		if err := ValidateUpdateImageReference(value); err == nil {
			t.Errorf("invalid reference accepted: %q", value)
		}
	}
	config := &dockercontainer.Config{Image: "sha256:" + digest, Labels: map[string]string{updateImageRefLabel: "tailscale/tailscale:latest"}}
	if ref, err := UpdateImageReference(config); err != nil || ref != "tailscale/tailscale:latest" {
		t.Fatalf("lost recorded source: %q %v", ref, err)
	}
	config.Labels[updateImageRefLabel] = "sha256:" + digest
	if _, err := UpdateImageReference(config); err == nil {
		t.Fatal("accepted ID-only legacy configuration without a known registry reference")
	}
	config.Image = "nginx:stable"
	config.Labels[updateImageRefLabel] = "tailscale/tailscale:latest"
	if ref, err := UpdateImageReference(config); err != nil || ref != "nginx:stable" {
		t.Fatalf("stale label overrode current named configuration: %q %v", ref, err)
	}
}

// Exercise two actual replacements through Docker's HTTP API. Creation stays
// pinned, but the list and the next update must retain the registry source.
func TestRepeatedUpdateRetainsRegistryReferenceWhilePinningImageID(t *testing.T) {
	const source = "tailscale/tailscale:latest"
	var mu sync.Mutex
	currentID := "original"
	currentConfig := dockercontainer.Config{Image: source, Labels: map[string]string{"custom": "keep"}}
	currentImageID := "sha256:" + strings.Repeat("0", 64)
	var pullRefs, createImages []string
	service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": currentID, "Names": []string{"/tailscale"}, "Image": currentImageID, "ImageID": currentImageID, "State": "exited", "Status": "Exited"}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": currentID, "Name": "/tailscale", "Image": currentImageID, "Config": currentConfig, "HostConfig": map[string]any{}, "State": map[string]any{"Running": false}, "NetworkSettings": map[string]any{"Networks": map[string]any{}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			pullRefs = append(pullRefs, r.URL.Query().Get("fromImage")+":"+r.URL.Query().Get("tag"))
			fmt.Fprintln(w, `{"status":"pulled"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "sha256:" + strings.Repeat(fmt.Sprint(len(pullRefs)), 64)})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/create":
			if err := json.NewDecoder(r.Body).Decode(&currentConfig); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			createImages = append(createImages, currentConfig.Image)
			currentImageID = currentConfig.Image
			currentID = fmt.Sprintf("replacement-%d", len(createImages))
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": currentID, "Warnings": []string{}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1.45/containers/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker call: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	for round := 0; round < 2; round++ {
		items, err := service.ListDetails(context.Background(), nil)
		if err != nil || len(items) != 1 {
			t.Fatalf("list round %d: %v %+v", round, err, items)
		}
		if items[0].UsingImage != source {
			t.Fatalf("round %d exposed local ID as update source: %q", round, items[0].UsingImage)
		}
		if err := service.UpdateWithOptions(context.Background(), items[0].Id, "tailscale", items[0].UsingImage, true, fmt.Sprintf("task-%d", round), testUpdateCallbacks(func(string) error { return nil })); err != nil {
			t.Fatalf("update round %d: %v", round, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pullRefs) != 2 || len(createImages) != 2 {
		t.Fatalf("pulls=%v creates=%v", pullRefs, createImages)
	}
	for i := range pullRefs {
		if strings.TrimPrefix(pullRefs[i], "docker.io/") != source || createImages[i] != "sha256:"+strings.Repeat(fmt.Sprint(i+1), 64) {
			t.Fatalf("round %d pulled %q and created %q", i, pullRefs[i], createImages[i])
		}
	}
	if currentConfig.Labels["custom"] != "keep" || currentConfig.Labels[updateImageRefLabel] != source {
		t.Fatalf("labels not preserved: %v", currentConfig.Labels)
	}
}

func TestIDOnlyUpdateDoesNotContactDocker(t *testing.T) {
	service := newUpdateTestService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Docker request: %s", r.URL)
		w.WriteHeader(500)
	})
	err := service.UpdateWithOptions(context.Background(), "original", "tailscale", "sha256:"+strings.Repeat("a", 64), true, "task-invalid", testUpdateCallbacks(func(string) error { return nil }))
	if err == nil {
		t.Fatal("expected a useful validation error before pull")
	}
}

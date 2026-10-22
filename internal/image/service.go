package image

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	reference "github.com/distribution/reference"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

// Service owns image listing, usage annotation, and image removal.
type Service struct {
	docker    *client.Client
	gate      *AdmissionGate
	protectMu sync.RWMutex
	protect   ProtectionSource
}

func NewService(docker *client.Client) *Service {
	return &Service{docker: docker, gate: &AdmissionGate{}}
}

// AdmissionGate serializes update-task admission with destructive image
// operations. Callers hold it only while the task is durably accepted.
type AdmissionGate struct{ mu sync.Mutex }

func (g *AdmissionGate) Lock() {
	if g != nil {
		g.mu.Lock()
	}
}

func (g *AdmissionGate) Unlock() {
	if g != nil {
		g.mu.Unlock()
	}
}

func (s *Service) AdmissionGate() *AdmissionGate {
	if s == nil {
		return nil
	}
	return s.gate
}

type ProtectedImages struct {
	IDs        map[string][]string
	References map[string][]string
}

type ProtectionSource interface {
	ProtectedImages(context.Context) (ProtectedImages, error)
}

func (s *Service) SetProtectionSource(source ProtectionSource) {
	if s == nil {
		return
	}
	s.protectMu.Lock()
	s.protect = source
	s.protectMu.Unlock()
}

type ConflictError struct {
	TaskID string
	Reason string
}

func (e *ConflictError) Error() string {
	if e == nil {
		return "image is in use"
	}
	if e.TaskID != "" {
		return fmt.Sprintf("image is protected by active update task %s", e.TaskID)
	}
	if e.Reason != "" {
		return e.Reason
	}
	return "image is in use"
}

func (*ConflictError) HTTPStatus() int   { return http.StatusConflict }
func (*ConflictError) ErrorCode() string { return "IMAGE_IN_USE" }

func SplitImageNameAndTag(images []types.Image) []types.Image {
	for i, item := range images {
		if len(item.RepoTags) > 0 {
			tag := item.RepoTags[0]
			if idx := strings.LastIndex(tag, ":"); idx >= 0 && !strings.Contains(tag[idx+1:], "/") {
				images[i].ImageName, images[i].ImageTag = tag[:idx], tag[idx+1:]
			} else {
				images[i].ImageName, images[i].ImageTag = tag, "None"
			}
		} else if len(item.RepoDigests) > 0 {
			images[i].ImageName, images[i].ImageTag = strings.Split(item.RepoDigests[0], "@")[0], "None"
		} else {
			images[i].ImageName, images[i].ImageTag = "None", "None"
		}
	}
	return images
}

func CalculateImageSize(images []types.Image) []types.Image {
	for i := range images {
		if images[i].Size >= 1024*1024*1024 {
			images[i].SizeFormat = fmt.Sprintf("%d Gb", images[i].Size/1024/1024/1024)
		} else {
			images[i].SizeFormat = fmt.Sprintf("%d Mb", images[i].Size/1024/1024)
		}
	}
	return images
}

func (s *Service) List(ctx context.Context) ([]types.Image, error) {
	images, err := s.docker.ImageList(ctx, dockerimage.ListOptions{})
	if err != nil {
		return nil, err
	}
	result := make([]types.Image, 0, len(images))
	for _, item := range images {
		result = append(result, types.Image{Summary: item})
	}
	result = CalculateImageSize(SplitImageNameAndTag(result))
	containers, err := s.docker.ContainerList(ctx, dockercontainer.ListOptions{All: true})
	if err != nil {
		return result, err
	}
	used := make(map[string]struct{}, len(containers))
	for _, item := range containers {
		used[item.ImageID] = struct{}{}
	}
	for i := range result {
		_, result[i].InUsed = used[result[i].ID]
	}
	return result, nil
}

func (s *Service) Remove(ctx context.Context, id string, force bool) error {
	if s == nil || s.docker == nil {
		return fmt.Errorf("image Docker client is unavailable")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("image ID is required")
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	protection, err := s.protectionSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("check active image update protection: %w", err)
	}
	if protected, taskID := protection.match(id, nil); protected {
		return &ConflictError{TaskID: taskID}
	}
	inspected, _, inspectErr := s.docker.ImageInspectWithRaw(ctx, id)
	if inspectErr == nil {
		refs := append(append([]string(nil), inspected.RepoTags...), inspected.RepoDigests...)
		if protected, taskID := protection.match(inspected.ID, refs); protected {
			return &ConflictError{TaskID: taskID}
		}
	} else if len(protection.References) != 0 {
		return fmt.Errorf("resolve image identity while active update references are protected: %w", inspectErr)
	}
	_, err = s.docker.ImageRemove(ctx, id, dockerimage.RemoveOptions{Force: force})
	if err != nil && !force && cerrdefs.IsConflict(err) {
		return &ConflictError{Reason: "image is in use by a container; ordinary deletion did not force removal"}
	}
	return err
}

func (s *Service) protectionSnapshot(ctx context.Context) (ProtectedImages, error) {
	s.protectMu.RLock()
	source := s.protect
	s.protectMu.RUnlock()
	if source == nil {
		return ProtectedImages{}, nil
	}
	return source.ProtectedImages(ctx)
}

func (p ProtectedImages) match(id string, refs []string) (bool, string) {
	if taskIDs := p.IDs[normalizeImageID(id)]; len(taskIDs) != 0 {
		return true, taskIDs[0]
	}
	if taskIDs := p.References[NormalizeReference(id)]; len(taskIDs) != 0 {
		return true, taskIDs[0]
	}
	for _, ref := range refs {
		if taskIDs := p.References[NormalizeReference(ref)]; len(taskIDs) != 0 {
			return true, taskIDs[0]
		}
	}
	return false, ""
}

func normalizeImageID(id string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(id), "sha256:"))
}

// NormalizeReference canonicalizes Docker names and supplies the implicit
// latest tag where Docker would do so. Invalid references remain comparable
// as trimmed raw strings for compatibility with daemon-provided RepoTags.
func NormalizeReference(value string) string {
	value = strings.TrimSpace(value)
	if named, err := reference.ParseNormalizedNamed(value); err == nil {
		if _, digest := named.(reference.Digested); !digest {
			named = reference.TagNameOnly(named)
		}
		return named.String()
	}
	return value
}

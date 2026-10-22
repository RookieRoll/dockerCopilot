package svc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	dockercontainer "github.com/docker/docker/api/types/container"
	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/task"
)

// ImageService returns the shared image service and wires task-backed image
// protection. Literal ServiceContext values remain supported by constructing
// the service lazily, as they are in older logic tests.
func (ctx *ServiceContext) ImageService() *imageService.Service {
	if ctx == nil {
		return nil
	}
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.Images == nil {
		ctx.Images = imageService.NewService(ctx.DockerClient)
	}
	if ctx.TaskStore != nil {
		ctx.Images.SetProtectionSource(taskImageProtection{ctx: ctx})
	}
	return ctx.Images
}

type taskImageProtection struct{ ctx *ServiceContext }

func (p taskImageProtection) ProtectedImages(ctx context.Context) (imageService.ProtectedImages, error) {
	if p.ctx == nil {
		return imageService.ProtectedImages{}, nil
	}
	p.ctx.mu.Lock()
	store := p.ctx.TaskStore
	p.ctx.mu.Unlock()
	if store == nil {
		return imageService.ProtectedImages{}, nil
	}
	protected, err := store.ProtectedImages()
	if err != nil {
		return imageService.ProtectedImages{}, err
	}
	result := imageService.ProtectedImages{IDs: make(map[string][]string), References: make(map[string][]string)}
	addID := func(id, taskID string) {
		id = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(id), "sha256:"))
		if id == "" {
			return
		}
		result.IDs[id] = appendUnique(result.IDs[id], taskID)
	}
	for _, image := range protected {
		addID(image.ImageID, image.TaskID)
	}
	records, err := store.List()
	if err != nil {
		return imageService.ProtectedImages{}, err
	}
	for _, record := range records {
		if !protectsImageRecovery(record) {
			continue
		}
		for _, ref := range []string{record.Options.Image, record.ImageRef} {
			addImageReference(result.References, ref, record.TaskID)
		}
		if record.SnapshotRef != "" && record.Stage != "queued" && record.Stage != "" {
			snapshot, readErr := store.ReadSnapshot(record.TaskID)
			if readErr != nil {
				return imageService.ProtectedImages{}, fmt.Errorf("read update snapshot for task %s: %w", record.TaskID, readErr)
			}
			if err := addSnapshotProtection(result.IDs, record.TaskID, snapshot); err != nil {
				return imageService.ProtectedImages{}, err
			}
			var inspected dockercontainer.InspectResponse
			if err := json.Unmarshal(snapshot, &inspected); err != nil {
				return imageService.ProtectedImages{}, fmt.Errorf("decode private update snapshot for task %s: %w", record.TaskID, err)
			}
			if inspected.Config != nil {
				addImageReference(result.References, inspected.Config.Image, record.TaskID)
			}
		}
		for _, item := range record.Items {
			if !recordItemNeedsImageProtection(record, item) {
				continue
			}
			for _, ref := range []string{item.ImageRef} {
				addImageReference(result.References, ref, record.TaskID)
			}
			if item.SnapshotRef != "" && itemSnapshotIsDurable(item.Stage) {
				snapshot, readErr := store.ReadItemSnapshot(record.TaskID, item.SnapshotRef)
				if readErr != nil {
					return imageService.ProtectedImages{}, fmt.Errorf("read update item snapshot for task %s: %w", record.TaskID, readErr)
				}
				if err := addSnapshotProtection(result.IDs, record.TaskID, snapshot); err != nil {
					return imageService.ProtectedImages{}, err
				}
				var inspected dockercontainer.InspectResponse
				if err := json.Unmarshal(snapshot, &inspected); err != nil {
					return imageService.ProtectedImages{}, fmt.Errorf("decode private update item snapshot for task %s: %w", record.TaskID, err)
				}
				if inspected.Config != nil {
					addImageReference(result.References, inspected.Config.Image, record.TaskID)
				}
			}
		}
	}
	return result, nil
}

// Item snapshot references are assigned at admission, before the snapshot file
// is durably written. Only snapshot-saved and subsequent stages may read it;
// earlier stages still retain their admission-time ImageIDs and image refs.
func itemSnapshotIsDurable(stage string) bool {
	switch stage {
	case "", "queued", "starting", "preflight", "snapshot":
		return false
	default:
		return true
	}
}

func protectsImageRecovery(record task.Record) bool {
	if record.Status == task.StatusQueued || record.Status == task.StatusRunning || record.PendingInstall != nil || record.RequiresAttention {
		return true
	}
	return record.Recovery.Status != "" && record.Recovery.Status != "not-needed" && record.Recovery.Status != "restored"
}

func recordItemNeedsImageProtection(record task.Record, item task.Item) bool {
	if record.Status == task.StatusQueued || record.Status == task.StatusRunning {
		return item.Status == "queued" || item.Status == "running" || item.RequiresAttention || (item.Recovery.Status != "" && item.Recovery.Status != "not-needed" && item.Recovery.Status != "restored")
	}
	return item.RequiresAttention || (item.Recovery.Status != "" && item.Recovery.Status != "not-needed" && item.Recovery.Status != "restored")
}

func addImageReference(refs map[string][]string, ref, taskID string) {
	ref = imageService.NormalizeReference(ref)
	if ref == "" {
		return
	}
	refs[ref] = appendUnique(refs[ref], taskID)
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func addSnapshotProtection(ids map[string][]string, taskID string, data []byte) error {
	var inspected dockercontainer.InspectResponse
	if err := json.Unmarshal(data, &inspected); err != nil {
		return fmt.Errorf("decode private image protection snapshot for task %s: %w", taskID, err)
	}
	if inspected.ContainerJSONBase == nil || strings.TrimSpace(inspected.Image) == "" {
		return errors.New("private update snapshot does not contain the original image ID")
	}
	id := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(inspected.Image), "sha256:"))
	ids[id] = appendUnique(ids[id], taskID)
	return nil
}

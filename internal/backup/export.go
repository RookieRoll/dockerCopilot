package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockerBackend "github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/utiles/backup_compose"
)

const snapshotOperationKind = "container-config-snapshot"

type SnapshotItemResult struct {
	ContainerID string `json:"containerID,omitempty"`
	Name        string `json:"name,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

type SnapshotResult struct {
	Filename string               `json:"filename,omitempty"`
	Items    []SnapshotItemResult `json:"items"`
}

type SnapshotError struct {
	Items  []SnapshotItemResult
	causes []error
}

func (e *SnapshotError) Error() string {
	if e == nil || len(e.Items) == 0 {
		return "container configuration snapshot failed"
	}
	items := make([]string, 0, len(e.Items))
	for _, item := range e.Items {
		if item.Status == "failed" {
			label := item.Name
			if label == "" {
				label = item.ContainerID
			}
			items = append(items, fmt.Sprintf("%s: %s", label, item.Error))
		}
	}
	return "container configuration snapshot incomplete: " + strings.Join(items, "; ")
}

func (e *SnapshotError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return append([]error(nil), e.causes...)
}

// ExportJSON writes a complete legacy-compatible Docker create-config array.
// If any container cannot be captured in a stable state, no snapshot file is
// published and every affected container is identified in the returned error.
func (s *Service) ExportJSON(ctx context.Context) (SnapshotResult, error) {
	configs, _, result, err := s.capture(ctx)
	if err != nil {
		return result, err
	}
	if err := snapshotItemsError(result.Items); err != nil {
		return result, err
	}
	data, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		return result, fmt.Errorf("marshal JSON container snapshot: %w", err)
	}
	filename, err := s.writeSnapshot(".json", data)
	if err != nil {
		return result, err
	}
	result.Filename = filename
	return result, nil
}

// ExportComposeSnapshot writes a complete legacy Compose document through the
// same private, unique, atomic file store used by JSON snapshots.
func (s *Service) ExportComposeSnapshot(ctx context.Context) (SnapshotResult, error) {
	_, composeConfigs, result, err := s.capture(ctx)
	if err != nil {
		return result, err
	}
	if err := snapshotItemsError(result.Items); err != nil {
		return result, err
	}
	data, err := backupCompose.MarshalComposeYaml(composeConfigs)
	if err != nil {
		return result, err
	}
	filename, err := s.writeSnapshot(".yaml", data)
	if err != nil {
		return result, err
	}
	result.Filename = filename
	return result, nil
}

func (s *Service) capture(ctx context.Context) ([]dockerBackend.ContainerCreateConfig, []dockertypes.ContainerJSON, SnapshotResult, error) {
	result := SnapshotResult{Items: []SnapshotItemResult{}}
	if s.docker == nil {
		return nil, nil, result, errors.New("Docker client is unavailable")
	}
	items, err := s.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, nil, result, fmt.Errorf("list containers for snapshot: %w", err)
	}
	jsonConfigs := make([]dockerBackend.ContainerCreateConfig, 0, len(items))
	composeConfigs := make([]dockertypes.ContainerJSON, 0, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, nil, result, err
		}
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		itemResult := SnapshotItemResult{ContainerID: item.ID, Name: name, Status: "failed"}
		release, err := s.reserveSnapshot(ctx, item)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, result, ctx.Err()
			}
			itemResult.Error = err.Error()
			result.Items = append(result.Items, itemResult)
			continue
		}
		inspected, inspectErr := s.docker.ContainerInspect(ctx, item.ID)
		release()
		if inspectErr != nil {
			if ctx.Err() != nil {
				return nil, nil, result, ctx.Err()
			}
			itemResult.Error = inspectErr.Error()
			result.Items = append(result.Items, itemResult)
			continue
		}
		if inspected.ContainerJSONBase == nil {
			itemResult.Error = "inspect response is missing container identity"
			result.Items = append(result.Items, itemResult)
			continue
		}
		if inspected.Config == nil || inspected.HostConfig == nil || inspected.NetworkSettings == nil {
			itemResult.Error = "inspect response is missing container config, host config, or network settings"
			result.Items = append(result.Items, itemResult)
			continue
		}
		if inspected.ID != "" && item.ID != "" && !strings.EqualFold(inspected.ID, item.ID) {
			itemResult.Error = "container identity changed during snapshot"
			result.Items = append(result.Items, itemResult)
			continue
		}
		inspectName := strings.TrimPrefix(inspected.Name, "/")
		if itemResult.Name == "" || inspectName == "" || itemResult.Name != inspectName {
			itemResult.Error = "container name changed between list and inspect"
			result.Items = append(result.Items, itemResult)
			continue
		}
		inspected.Image = inspected.Config.Image
		composeConfigs = append(composeConfigs, inspected)
		jsonConfigs = append(jsonConfigs, dockerBackend.ContainerCreateConfig{
			Config: inspected.Config, HostConfig: inspected.HostConfig,
			NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: inspected.NetworkSettings.Networks},
			Name:             itemResult.Name,
		})
		itemResult.Status = "included"
		result.Items = append(result.Items, itemResult)
	}
	return jsonConfigs, composeConfigs, result, nil
}

func (s *Service) reserveSnapshot(ctx context.Context, item dockertypes.Container) (func(), error) {
	if s.coordinator == nil {
		return func() {}, nil
	}
	target := operation.Target{ID: item.ID}
	if len(item.Names) > 0 {
		target.Name = strings.TrimPrefix(item.Names[0], "/")
	}
	reservation, err := s.coordinator.ReserveContext(ctx, operation.Request{
		TaskID: uuid.NewString(), Kind: snapshotOperationKind, Target: target,
	})
	if err != nil {
		return nil, err
	}
	if reservation.Lease == nil {
		return nil, errors.New("container snapshot operation was not reserved")
	}
	if err := reservation.Lease.Confirm(); err != nil {
		reservation.Lease.Release()
		return nil, err
	}
	return reservation.Lease.Release, nil
}

func snapshotItemsError(items []SnapshotItemResult) error {
	failed := false
	causes := make([]error, 0)
	for _, item := range items {
		if item.Status != "failed" {
			continue
		}
		failed = true
		causes = append(causes, errors.New(item.Error))
	}
	if !failed {
		return nil
	}
	return &SnapshotError{Items: append([]SnapshotItemResult(nil), items...), causes: causes}
}

func (s *Service) writeSnapshot(extension string, data []byte) (string, error) {
	if strings.TrimSpace(s.dir) == "" {
		return "", errors.New("backup directory is empty")
	}
	if extension != ".json" && extension != ".yaml" {
		return "", errors.New("unsupported backup snapshot format")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	temp, err := os.CreateTemp(s.dir, ".backup-snapshot-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary backup snapshot: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	writeData := s.writeData
	if writeData == nil {
		writeData = func(writer io.Writer, data []byte) (int, error) { return writer.Write(data) }
	}
	written, err := writeData(temp, data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("write temporary backup snapshot: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("set backup snapshot permissions: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("sync temporary backup snapshot: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temporary backup snapshot: %w", err)
	}
	date := time.Now().Format("2006-01-02")
	for attempt := 0; attempt < 5; attempt++ {
		suffix := s.newSuffix()
		if suffix == "" || !filenamePattern.MatchString(suffix) {
			return "", errors.New("backup filename suffix is invalid")
		}
		filename := fmt.Sprintf("backup-%s-%s%s", date, suffix, extension)
		destination, err := s.Resolve(filename, extension)
		if err != nil {
			return "", err
		}
		if err := os.Link(tempName, destination); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", fmt.Errorf("atomically publish backup snapshot: %w", err)
		}
		return filename, nil
	}
	return "", errors.New("could not allocate a unique backup filename")
}

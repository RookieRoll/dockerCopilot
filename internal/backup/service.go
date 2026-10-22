package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	dockerBackend "github.com/docker/docker/api/types/backend"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	dockerMsgType "github.com/docker/docker/pkg/jsonmessage"
	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

var filenamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Service owns backup storage and Docker export/restore source files.
type Service struct {
	docker      *client.Client
	dir         string
	progress    ProgressCallbacks
	coordinator *operation.Coordinator
	newSuffix   func() string
	writeData   func(io.Writer, []byte) (int, error)
}

type ProgressCallbacks struct {
	Update func(string, types.TaskProgress)
	Get    func(string) (types.TaskProgress, bool)
}

func NewService(docker *client.Client, directory string, coordinators ...*operation.Coordinator) *Service {
	service := &Service{
		docker: docker, dir: strings.TrimSpace(directory),
		newSuffix: func() string { return strings.ReplaceAll(uuid.NewString(), "-", "") },
		writeData: func(writer io.Writer, data []byte) (int, error) { return writer.Write(data) },
	}
	if len(coordinators) > 0 {
		service.coordinator = coordinators[0]
	}
	return service
}

func (s *Service) Directory() string                                { return s.dir }
func (s *Service) SetProgressCallbacks(callbacks ProgressCallbacks) { s.progress = callbacks }

// SetCoordinator injects the shared container-operation coordinator so a
// snapshot cannot inspect a container while an in-process replacement owns it.
func (s *Service) SetCoordinator(coordinator *operation.Coordinator) { s.coordinator = coordinator }

func (s *Service) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		if !entry.IsDir() && (filepath.Ext(entry.Name()) == ".json" || filepath.Ext(entry.Name()) == ".yaml") {
			result = append(result, entry.Name())
		}
	}
	return result, nil
}

func (s *Service) Resolve(filename string, extensions ...string) (string, error) {
	name := strings.TrimSpace(filename)
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\\`) || !filenamePattern.MatchString(name) {
		return "", fmt.Errorf("非法文件名")
	}
	if len(extensions) > 0 {
		allowed := false
		for _, extension := range extensions {
			if strings.EqualFold(filepath.Ext(name), extension) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("非法文件名")
		}
	}
	base := filepath.Clean(s.dir)
	full := filepath.Clean(filepath.Join(base, name))
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("非法文件名")
	}
	return full, nil
}

func (s *Service) Delete(filename string, extensions ...string) error {
	path, err := s.Resolve(filename, extensions...)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (s *Service) Export(ctx context.Context) error {
	_, err := s.ExportJSON(ctx)
	return err
}

func (s *Service) ExportCompose(ctx context.Context) error {
	_, err := s.ExportComposeSnapshot(ctx)
	return err
}

func (s *Service) Restore(ctx context.Context, filename, taskID string) error {
	_, err := s.RestoreWithResults(ctx, filename, taskID)
	return err
}

type RestoreItemResult struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type RestoreResult struct {
	Items []RestoreItemResult `json:"items"`
}

// RestoreWithResults validates the entire legacy JSON document before making
// Docker calls, then pulls and creates each eligible container without starting it.
func (s *Service) RestoreWithResults(ctx context.Context, filename, taskID string) (RestoreResult, error) {
	result := RestoreResult{Items: []RestoreItemResult{}}
	progress := types.TaskProgress{TaskID: taskID, Name: "恢复容器"}
	update := func() {
		if s.progress.Update != nil {
			s.progress.Update(taskID, progress)
		}
	}
	fail := func(message string, err error) error {
		progress.Message, progress.DetailMsg, progress.IsDone = message, err.Error(), true
		update()
		return err
	}
	path, err := s.Resolve(filename, ".json")
	if err != nil {
		return result, fail("非法文件名", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return result, fail("读取文件失败或者未找到文件。请确认文件名仅由大小写字母、数字和短横线组成", err)
	}
	var configs []dockerBackend.ContainerCreateConfig
	if err := json.Unmarshal(content, &configs); err != nil {
		return result, fail("解析文件失败", err)
	}
	if len(configs) == 0 {
		return result, fail("备份文件没有可恢复的容器", errors.New("backup contains no containers"))
	}
	for i, item := range configs {
		if item.Config == nil || strings.TrimSpace(item.Config.Image) == "" || !validContainerName(item.Name) {
			return result, fail("备份文件结构无效", fmt.Errorf("container entry %d must include a valid name and image", i+1))
		}
	}
	if s.docker == nil {
		return result, fail("Docker 客户端不可用", errors.New("Docker client is unavailable"))
	}
	listed, err := s.docker.ContainerList(ctx, dockercontainer.ListOptions{All: true})
	if err != nil {
		return result, fail("读取现有容器失败", fmt.Errorf("list existing containers before restore: %w", err))
	}
	existingNames := make(map[string]struct{}, len(listed))
	for _, existing := range listed {
		for _, name := range existing.Names {
			existingNames[strings.TrimPrefix(name, "/")] = struct{}{}
		}
	}
	seenNames := make(map[string]struct{}, len(configs))
	for i, item := range configs {
		entry := RestoreItemResult{Index: i, Name: item.Name, Status: "running"}
		if _, exists := existingNames[item.Name]; exists {
			entry.Status, entry.Error = "skipped", "a container with this name already exists"
			result.Items = append(result.Items, entry)
			continue
		}
		if _, duplicate := seenNames[item.Name]; duplicate {
			entry.Status, entry.Error = "skipped", "duplicate container name in backup"
			result.Items = append(result.Items, entry)
			continue
		}
		seenNames[item.Name] = struct{}{}
		reservation, reserveErr := s.reserveRestore(ctx, item.Name)
		if reserveErr != nil {
			entry.Status, entry.Error = "skipped", reserveErr.Error()
			result.Items = append(result.Items, entry)
			continue
		}
		progress.Percentage = int(float64(i) / float64(len(configs)) * 100)
		progress.Message = fmt.Sprintf("正在恢复第%d个容器", i+1)
		progress.DetailMsg = progress.Message
		update()
		reader, err := s.docker.ImagePull(ctx, item.Config.Image, dockerimage.PullOptions{})
		if err != nil {
			if reservation != nil {
				reservation.Release()
			}
			entry.Status, entry.Error = "failed", fmt.Sprintf("pull image %s: %v", item.Config.Image, err)
			result.Items = append(result.Items, entry)
			continue
		}
		err = decodeRestorePull(reader)
		_ = reader.Close()
		if err != nil {
			if reservation != nil {
				reservation.Release()
			}
			entry.Status, entry.Error = "failed", fmt.Sprintf("pull image %s: %v", item.Config.Image, err)
			result.Items = append(result.Items, entry)
			continue
		}
		_, err = s.docker.ContainerCreate(ctx, item.Config, item.HostConfig, item.NetworkingConfig, nil, item.Name)
		if reservation != nil {
			reservation.Release()
		}
		if err != nil {
			entry.Status, entry.Error = "failed", fmt.Sprintf("create container: %v", err)
		} else {
			entry.Status = "succeeded"
		}
		result.Items = append(result.Items, entry)
	}
	var failures []string
	for _, item := range result.Items {
		if item.Status == "failed" || item.Status == "skipped" {
			failures = append(failures, fmt.Sprintf("%s: %s", item.Name, item.Error))
		}
	}
	progress.Percentage, progress.Message, progress.DetailMsg, progress.IsDone = 100, "恢复完成", formatRestoreResults(result.Items), true
	update()
	if len(failures) != 0 {
		return result, fmt.Errorf("restore completed with item failures: %s", strings.Join(failures, "; "))
	}
	return result, nil
}

func (s *Service) reserveRestore(ctx context.Context, name string) (*operation.Lease, error) {
	if s.coordinator == nil {
		return nil, nil
	}
	reservation, err := s.coordinator.ReserveContext(ctx, operation.Request{
		TaskID: uuid.NewString(), Kind: "container-restore", Target: operation.Target{Name: name},
	})
	if err != nil {
		return nil, err
	}
	if reservation.Lease == nil {
		return nil, errors.New("container restore operation was not reserved")
	}
	if err := reservation.Lease.Confirm(); err != nil {
		reservation.Lease.Release()
		return nil, err
	}
	return reservation.Lease, nil
}

func validContainerName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name != strings.TrimLeft(name, "/") || strings.ContainsAny(name, " \t\r\n") {
		return false
	}
	for _, r := range name {
		if !(r == '_' || r == '.' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func formatRestoreResults(items []RestoreItemResult) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		line := fmt.Sprintf("%s: %s", item.Name, item.Status)
		if item.Status == "succeeded" {
			line = item.Name + "恢复成功"
		}
		if item.Error != "" {
			line += " (" + item.Error + ")"
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, ",\n")
}

func decodeRestorePull(reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	for {
		var message dockerMsgType.JSONMessage
		if err := decoder.Decode(&message); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if message.Error != nil {
			return message.Error
		}
	}
}

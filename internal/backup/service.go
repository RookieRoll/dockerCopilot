package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	dockerBackend "github.com/docker/docker/api/types/backend"
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
		return fail("非法文件名", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fail("读取文件失败或者未找到文件。请确认文件名仅由大小写字母、数字和短横线组成", err)
	}
	var configs []dockerBackend.ContainerCreateConfig
	if err := json.Unmarshal(content, &configs); err != nil {
		return fail("解析文件失败", err)
	}
	results := make([]string, 0, len(configs))
	for i, item := range configs {
		progress.Percentage = int(float64(i) / float64(len(configs)) * 100)
		progress.Message = fmt.Sprintf("正在恢复第%d个容器", i+1)
		progress.DetailMsg = progress.Message
		update()
		reader, err := s.docker.ImagePull(ctx, item.Config.Image, dockerimage.PullOptions{})
		if err != nil {
			results = append(results, item.Config.Image+"拉取镜像出现错误"+err.Error())
			continue
		}
		err = decodeRestorePull(reader)
		_ = reader.Close()
		if err != nil {
			results = append(results, item.Config.Image+"拉取镜像出现错误"+err.Error())
			continue
		}
		_, err = s.docker.ContainerCreate(ctx, item.Config, item.HostConfig, item.NetworkingConfig, nil, item.Name)
		if err != nil {
			results = append(results, item.Name+"恢复失败"+err.Error())
		} else {
			results = append(results, item.Name+"恢复成功")
		}
	}
	progress.Percentage, progress.Message, progress.DetailMsg, progress.IsDone = 100, "恢复完成", strings.Join(results, ",\n"), true
	update()
	return nil
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

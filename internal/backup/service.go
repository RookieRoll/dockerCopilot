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
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockerBackend "github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	dockerMsgType "github.com/docker/docker/pkg/jsonmessage"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/onlyLTY/dockerCopilot/internal/utiles/backup_compose"
)

var filenamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Service owns backup storage and Docker export/restore source files.
type Service struct {
	docker   *client.Client
	dir      string
	progress ProgressCallbacks
}

type ProgressCallbacks struct {
	Update func(string, types.TaskProgress)
	Get    func(string) (types.TaskProgress, bool)
}

func NewService(docker *client.Client, directory string) *Service {
	return &Service{docker: docker, dir: strings.TrimSpace(directory)}
}

func (s *Service) Directory() string                                { return s.dir }
func (s *Service) SetProgressCallbacks(callbacks ProgressCallbacks) { s.progress = callbacks }

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
	items, err := s.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}
	list := make([]dockerBackend.ContainerCreateConfig, 0, len(items))
	for _, item := range items {
		inspected, err := s.docker.ContainerInspect(ctx, item.ID)
		if err != nil {
			return err
		}
		name := "get container name error"
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		inspected.Config.Hostname = ""
		inspected.Image = inspected.Config.Image
		list = append(list, dockerBackend.ContainerCreateConfig{Config: inspected.Config, HostConfig: inspected.HostConfig, NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: inspected.NetworkSettings.Networks}, Name: name})
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "backup-"+time.Now().Format("2006-01-02")+".json"), data, 0o644)
}

func (s *Service) ExportCompose(ctx context.Context) error {
	items, err := s.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}
	configs := make([]dockertypes.ContainerJSON, 0, len(items))
	for _, item := range items {
		inspected, err := s.docker.ContainerInspect(ctx, item.ID)
		if err != nil {
			return err
		}
		configs = append(configs, inspected)
	}
	return backupCompose.DockerConfig2ComposeYamlInDir(configs, s.dir)
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

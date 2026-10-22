package container

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	dockerMsgType "github.com/docker/docker/pkg/jsonmessage"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

// Service owns the direct container operations used by the HTTP application.
// It deliberately depends on the Docker client rather than the application
// composition context so it can be tested with a daemon or a narrow fake.
type Service struct {
	docker   *client.Client
	updates  *module.ImageUpdateData
	progress ProgressCallbacks
}

type ProgressCallbacks struct {
	Update func(string, types.TaskProgress)
	Get    func(string) (types.TaskProgress, bool)
}

func NewService(docker *client.Client, updates ...*module.ImageUpdateData) *Service {
	var checker *module.ImageUpdateData
	if len(updates) > 0 {
		checker = updates[0]
	}
	return &Service{docker: docker, updates: checker}
}

func (s *Service) SetProgressCallbacks(callbacks ProgressCallbacks) { s.progress = callbacks }

func (s *Service) Update(ctx context.Context, id, name, imageNameAndTag string, deleteOld bool, taskID string) error {
	progress := types.TaskProgress{TaskID: taskID, Name: name, Message: "正在连接Docker", DetailMsg: "正在连接Docker"}
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
	update()
	progress.Message, progress.DetailMsg, progress.Percentage = "正在拉取新镜像", "正在拉取新镜像", 10
	update()
	s.docker.NegotiateAPIVersion(ctx)
	reader, err := s.docker.ImagePull(ctx, imageNameAndTag, image.PullOptions{})
	if err != nil {
		return fail("拉取镜像失败", err)
	}
	err = decodeUpdatePull(reader, &progress, update)
	if closeErr := reader.Close(); closeErr != nil {
		logx.Error("关闭镜像拉取响应失败: " + closeErr.Error())
	}
	if err != nil {
		return fail("拉取镜像失败", err)
	}
	progress.Message, progress.DetailMsg = "拉取镜像成功", "拉取镜像成功"
	update()
	progress.Message, progress.DetailMsg, progress.Percentage = "正在停止容器", "正在停止容器", 30
	update()
	timeout := 10
	if err := s.docker.ContainerStop(ctx, id, container.StopOptions{Signal: "SIGINT", Timeout: &timeout}); err != nil {
		return fail("停止容器失败", err)
	}
	progress.Message, progress.DetailMsg = "容器停止成功", "容器停止成功"
	update()
	progress.Message, progress.DetailMsg, progress.Percentage = "正在重命名旧容器", "正在重命名旧容器", 40
	update()
	if err := s.docker.ContainerRename(ctx, id, name+"-"+time.Now().Format("2006-01-02-15-04-05")); err != nil {
		return fail("重命名旧容器失败", err)
	}
	progress.Message, progress.DetailMsg = "重命名旧容器成功", "重命名旧容器成功"
	update()
	progress.Message, progress.DetailMsg, progress.Percentage = "正在创建新容器", "正在创建新容器", 60
	update()
	inspected, err := s.docker.ContainerInspect(ctx, id)
	if err != nil {
		return fail("获取容器信息失败", err)
	}
	inspected.Config.Hostname, inspected.Config.Image, inspected.Image = "", imageNameAndTag, imageNameAndTag
	networking := &network.NetworkingConfig{EndpointsConfig: inspected.NetworkSettings.Networks}
	if _, err := s.docker.ContainerCreate(ctx, inspected.Config, inspected.HostConfig, networking, nil, name); err != nil {
		return fail("创建新容器失败", err)
	}
	progress.Message, progress.DetailMsg = "创建新容器成功", "创建新容器成功"
	update()
	progress.Message, progress.DetailMsg, progress.Percentage = "正在启动新容器以及删除旧容器(如果不保留旧容器)", "正在启动新容器以及删除旧容器(如果不保留旧容器)", 80
	update()
	if err := s.docker.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return fail("启动新容器失败", err)
	}
	if deleteOld {
		if err := s.docker.ContainerRemove(ctx, id, container.RemoveOptions{}); err != nil {
			return fail("删除旧容器失败", err)
		}
	}
	progress.Message, progress.DetailMsg, progress.Percentage, progress.IsDone = "更新成功", "更新成功", 100, true
	update()
	return nil
}

func decodeUpdatePull(reader io.Reader, progress *types.TaskProgress, update func()) error {
	decoder := json.NewDecoder(reader)
	for {
		var msg dockerMsgType.JSONMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			progress.Message, progress.DetailMsg, progress.Percentage, progress.IsDone = "拉取镜像失败", err.Error(), 25, true
			update()
			return fmt.Errorf("拉取镜像失败: %w", err)
		}
		if msg.Error != nil {
			progress.Message, progress.DetailMsg, progress.Percentage, progress.IsDone = "拉取镜像失败", msg.Error.Error(), 25, true
			update()
			return fmt.Errorf("拉取镜像失败: %w", msg.Error)
		}
		if msg.Progress != nil {
			progress.DetailMsg = fmt.Sprintf("进度%s: %s", msg.Status, msg.Progress.String())
		} else {
			progress.DetailMsg = fmt.Sprintf("进度%s", msg.Status)
		}
		progress.Percentage = 25
		update()
	}
}

func (s *Service) List(ctx context.Context) ([]types.Container, error) {
	items, err := s.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	result := make([]types.Container, 0, len(items))
	for _, item := range items {
		entry := types.Container{Container: item}
		result = append(result, entry)
	}
	return s.CheckImageUpdates(result), nil
}

func (s *Service) CheckImageUpdates(items []types.Container) []types.Container {
	if s.updates == nil {
		return items
	}
	for i := range items {
		if update, ok := s.updates.Get(items[i].ImageID); ok && update.NeedUpdate {
			items[i].Update = true
		}
	}
	return items
}

func (s *Service) Start(ctx context.Context, id string) error {
	return s.docker.ContainerStart(ctx, id, container.StartOptions{})
}

func (s *Service) Stop(ctx context.Context, id string) error {
	timeout := 10
	return s.docker.ContainerStop(ctx, id, container.StopOptions{Signal: "SIGINT", Timeout: &timeout})
}

func (s *Service) Restart(ctx context.Context, id string) error {
	timeout := 10
	return s.docker.ContainerRestart(ctx, id, container.StopOptions{Signal: "SIGINT", Timeout: &timeout})
}

func (s *Service) Rename(ctx context.Context, id, name string) error {
	return s.docker.ContainerRename(ctx, id, name)
}

func (s *Service) Inspect(ctx context.Context, id string) (dockertypes.ContainerJSON, error) {
	return s.docker.ContainerInspect(ctx, id)
}

package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	dockerMsgType "github.com/docker/docker/pkg/jsonmessage"
	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

// Service owns the direct container operations used by the HTTP application.
// It deliberately depends on the Docker client rather than the application
// composition context so it can be tested with a daemon or a narrow fake.
type Service struct {
	docker     *client.Client
	updates    *module.ImageUpdateData
	progress   ProgressCallbacks
	opMu       sync.Mutex
	operations *operation.Coordinator
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
	return &Service{docker: docker, updates: checker, operations: operation.NewCoordinator()}
}

// Operations returns the coordinator owned by this shared container service.
// The service composition should construct one Service and inject it into all
// handlers/adapters so every mutation path shares the same lease table.
func (s *Service) Operations() *operation.Coordinator {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.operations == nil {
		s.operations = operation.NewCoordinator()
	}
	return s.operations
}

// ReserveUpdate protects a container before a queued update is submitted.
// Callers must Confirm only after Runner.Submit has durably accepted the task;
// on submit failure they must Release. The equivalence key must encode every
// effective option that can change update behavior.
func (s *Service) ReserveUpdate(ctx context.Context, id, taskID, equivalenceKey string) (*operation.Lease, string, error) {
	inspected, err := s.Inspect(ctx, id)
	if err != nil {
		return nil, "", err
	}
	target := operation.Target{ID: inspected.ID, Name: strings.TrimLeft(strings.TrimSpace(inspected.Name), "/")}
	if target.ID == "" {
		target.ID = id
	}
	reservation, err := s.Operations().ReserveContext(ctx, operation.Request{
		TaskID: taskID, Kind: operation.ContainerUpdate, Target: target, EquivalenceKey: equivalenceKey,
	})
	if err != nil {
		return nil, "", err
	}
	return reservation.Lease, reservation.ExistingTaskID, nil
}

func (s *Service) reserveSync(ctx context.Context, id, kind string) (string, *operation.Lease, error) {
	inspected, err := s.Inspect(ctx, id)
	if err != nil {
		return "", nil, err
	}
	canonicalID := inspected.ID
	if canonicalID == "" {
		canonicalID = id
	}
	target := operation.Target{ID: canonicalID, Name: strings.TrimLeft(strings.TrimSpace(inspected.Name), "/")}
	reservation, err := s.Operations().ReserveContext(ctx, operation.Request{TaskID: uuid.NewString(), Kind: kind, Target: target})
	if err != nil {
		return "", nil, err
	}
	if reservation.Lease == nil {
		return "", nil, &operation.ConflictError{TaskID: reservation.ExistingTaskID, Kind: kind}
	}
	if err := reservation.Lease.Confirm(); err != nil {
		reservation.Lease.Release()
		return "", nil, err
	}
	return canonicalID, reservation.Lease, nil
}

func (s *Service) SetProgressCallbacks(callbacks ProgressCallbacks) { s.progress = callbacks }

func (s *Service) Update(ctx context.Context, id, name, imageNameAndTag string, deleteOld bool, taskID string) error {
	return s.UpdateWithCallbacks(ctx, id, name, imageNameAndTag, deleteOld, taskID, nil)
}

// UpdateWithCallbacks provides the created Docker ID immediately after the
// daemon responds, before a later mutating step is allowed to run. Task owners
// should durably checkpoint that fact in onCreated.
func (s *Service) UpdateWithCallbacks(ctx context.Context, id, name, imageNameAndTag string, deleteOld bool, taskID string, onCreated func(string) error) error {
	before, err := s.Inspect(ctx, id)
	if err != nil {
		return err
	}
	canonicalID := before.ID
	if canonicalID == "" {
		canonicalID = id
	}
	target := operation.Target{ID: canonicalID, Name: strings.TrimLeft(strings.TrimSpace(before.Name), "/")}
	ops := s.Operations()
	var ownLease *operation.Lease
	if !ops.Owns(taskID, target) {
		if taskID == "" {
			taskID = uuid.NewString()
		}
		lease, existingID, reserveErr := s.ReserveUpdate(ctx, id, taskID, FingerprintUpdate(name, imageNameAndTag, deleteOld))
		if reserveErr != nil {
			return reserveErr
		}
		if existingID != "" {
			return &operation.ConflictError{TaskID: existingID, Kind: operation.ContainerUpdate}
		}
		if lease == nil {
			return fmt.Errorf("update reservation did not return a lease")
		}
		if err := lease.Confirm(); err != nil {
			lease.Release()
			return err
		}
		ownLease = lease
	}
	if ownLease != nil {
		defer ownLease.Release()
	}
	if err := ops.AddAlias(taskID, target); err != nil {
		return err
	}
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
	backupName := name + "-" + time.Now().Format("2006-01-02-15-04-05")
	if err := ops.AddAlias(taskID, operation.Target{Name: backupName}); err != nil {
		return fail("重命名旧容器失败", err)
	}
	if err := s.docker.ContainerRename(ctx, id, backupName); err != nil {
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
	created, err := s.docker.ContainerCreate(ctx, inspected.Config, inspected.HostConfig, networking, nil, name)
	if err != nil {
		return fail("创建新容器失败", err)
	}
	aliasErr := ops.AddAlias(taskID, operation.Target{ID: created.ID})
	var checkpointErr error
	if onCreated != nil {
		checkpointErr = onCreated(created.ID)
	}
	nameAliasErr := ops.AddAlias(taskID, operation.Target{Name: name})
	if err := errors.Join(aliasErr, checkpointErr, nameAliasErr); err != nil {
		return fail("保护新容器身份失败", err)
	}
	progress.Message, progress.DetailMsg = "创建新容器成功", "创建新容器成功"
	update()
	progress.Message, progress.DetailMsg, progress.Percentage = "正在启动新容器以及删除旧容器(如果不保留旧容器)", "正在启动新容器以及删除旧容器(如果不保留旧容器)", 80
	update()
	if err := s.docker.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
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
	canonicalID, lease, err := s.reserveSync(ctx, id, "container-start")
	if err != nil {
		return err
	}
	defer lease.Release()
	return s.docker.ContainerStart(ctx, canonicalID, container.StartOptions{})
}

func (s *Service) Stop(ctx context.Context, id string) error {
	canonicalID, lease, err := s.reserveSync(ctx, id, "container-stop")
	if err != nil {
		return err
	}
	defer lease.Release()
	timeout := 10
	return s.docker.ContainerStop(ctx, canonicalID, container.StopOptions{Signal: "SIGINT", Timeout: &timeout})
}

func (s *Service) Restart(ctx context.Context, id string) error {
	canonicalID, lease, err := s.reserveSync(ctx, id, "container-restart")
	if err != nil {
		return err
	}
	defer lease.Release()
	timeout := 10
	return s.docker.ContainerRestart(ctx, canonicalID, container.StopOptions{Signal: "SIGINT", Timeout: &timeout})
}

func (s *Service) Rename(ctx context.Context, id, name string) error {
	canonicalID, lease, err := s.reserveSync(ctx, id, "container-rename")
	if err != nil {
		return err
	}
	defer lease.Release()
	if err := lease.AddAlias(operation.Target{Name: name}); err != nil {
		return err
	}
	return s.docker.ContainerRename(ctx, canonicalID, name)
}

// FingerprintUpdate builds the deterministic equivalence key used when
// reserving a queued update. Callers must include every effective option.
func FingerprintUpdate(name, image string, deleteOld bool) string {
	// Use an ordered typed value so the deduplication key is stable.
	encoded, _ := json.Marshal(struct {
		Name      string `json:"name"`
		Image     string `json:"image"`
		DeleteOld bool   `json:"deleteOld"`
	}{Name: strings.TrimLeft(strings.TrimSpace(name), "/"), Image: strings.TrimSpace(image), DeleteOld: deleteOld})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (s *Service) Inspect(ctx context.Context, id string) (dockertypes.ContainerJSON, error) {
	return s.docker.ContainerInspect(ctx, id)
}

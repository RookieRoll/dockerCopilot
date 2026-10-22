package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockermount "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	dockerMsgType "github.com/docker/docker/pkg/jsonmessage"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

// UpdateCheckpoint contains only non-secret facts that must be durably stored
// before the next update step. The complete inspected configuration is stored
// separately through SaveSnapshot.
type UpdateCheckpoint struct {
	Stage           string
	SnapshotRef     string
	OldContainerID  string
	PreviousImageID string
	ResolvedImageID string
	BackupName      string
}

// UpdateCallbacks are narrow persistence/progress hooks supplied by the task
// owner. SaveSnapshot and Checkpoint are mandatory: updates never begin a
// destructive step unless their required facts have been durably recorded.
type UpdateCallbacks struct {
	Progress     func(stage string, percentage int, message string) error
	SaveSnapshot func([]byte) error
	Checkpoint   func(UpdateCheckpoint) error
	OnCreated    func(string) error
}

// UpdateWithOptions performs update preparation and replacement with a frozen
// image ID. deleteOld controls whether the renamed original is removed after
// the replacement is started.
func (s *Service) UpdateWithOptions(ctx context.Context, id, name, imageRef string, deleteOld bool, taskID string, callbacks UpdateCallbacks) error {
	if s == nil || s.docker == nil {
		return errors.New("container Docker client is unavailable")
	}
	id, name, imageRef = strings.TrimSpace(id), strings.Trim(strings.TrimSpace(name), "/"), strings.TrimSpace(imageRef)
	if id == "" || name == "" || imageRef == "" {
		return errors.New("container ID, target name, and image reference are required")
	}
	if taskID == "" {
		return errors.New("update task ID is required")
	}
	if callbacks.SaveSnapshot == nil || callbacks.Checkpoint == nil || callbacks.OnCreated == nil {
		return errors.New("update requires private snapshot, critical checkpoint, and created-ID persistence callbacks")
	}

	before, err := s.docker.ContainerInspect(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect original container: %w", err)
	}
	if before.ContainerJSONBase == nil || before.Config == nil || before.HostConfig == nil {
		return errors.New("original container inspect is missing required configuration")
	}
	canonicalID := before.ID
	if canonicalID == "" {
		canonicalID = id
	}
	originalName := strings.TrimLeft(strings.TrimSpace(before.Name), "/")
	if originalName == "" {
		originalName = name
	}
	if originalName != name {
		return fmt.Errorf("target name %q does not match inspected container name %q", name, originalName)
	}
	backupName := fmt.Sprintf("%s-backup-%s", name, time.Now().UTC().Format("20060102T150405.000000000"))
	if len(backupName) > 255 {
		return errors.New("generated backup container name exceeds Docker's 255 character limit")
	}

	ops := s.Operations()
	target := operation.Target{ID: canonicalID, Name: originalName}
	var ownLease *operation.Lease
	if !ops.Owns(taskID, target) {
		lease, existingID, reserveErr := s.ReserveUpdate(ctx, canonicalID, taskID, FingerprintUpdate(name, imageRef, deleteOld))
		if reserveErr != nil {
			return reserveErr
		}
		if existingID != "" {
			return &operation.ConflictError{TaskID: existingID, Kind: operation.ContainerUpdate}
		}
		if lease == nil {
			return errors.New("update reservation did not return a lease")
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
	if err := ops.AddAlias(taskID, operation.Target{Name: backupName}); err != nil {
		return err
	}

	progress := types.TaskProgress{TaskID: taskID, Name: name}
	publish := func(stage string, percent int, message string) error {
		progress.Percentage = percent
		progress.Message, progress.DetailMsg = message, message
		if callbacks.Progress != nil {
			if err := callbacks.Progress(stage, percent, message); err != nil {
				return err
			}
		}
		if s.progress.Update != nil {
			s.progress.Update(taskID, progress)
		}
		return nil
	}
	fail := func(stage, message string, cause error) error {
		progress.Message, progress.DetailMsg, progress.IsDone = message, cause.Error(), true
		if s.progress.Update != nil {
			s.progress.Update(taskID, progress)
		}
		return cause
	}
	checkpoint := func(value UpdateCheckpoint) error {
		if value.Stage == "" {
			return errors.New("update checkpoint stage is required")
		}
		return callbacks.Checkpoint(value)
	}

	if err := publish("preflight", 2, "正在检查容器并保存更新快照"); err != nil {
		return fail("preflight", "更新准备失败", err)
	}
	snapshot, err := json.Marshal(before)
	if err != nil {
		return fail("snapshot", "序列化容器配置快照失败", err)
	}
	if err := callbacks.SaveSnapshot(snapshot); err != nil {
		return fail("snapshot", "保存私有容器配置快照失败", err)
	}
	snapshotRef := taskID + ".snapshot"
	if err := checkpoint(UpdateCheckpoint{Stage: "snapshot-saved", SnapshotRef: snapshotRef, OldContainerID: canonicalID, PreviousImageID: before.Image}); err != nil {
		return fail("snapshot", "持久化快照检查点失败，未修改旧容器", err)
	}

	if err := publish("pulling-image", 10, "正在拉取目标镜像"); err != nil {
		return fail("pulling-image", "更新准备失败", err)
	}
	s.docker.NegotiateAPIVersion(ctx)
	reader, err := s.docker.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return fail("pulling-image", "拉取目标镜像失败，旧容器未修改", err)
	}
	pullErr := decodeUpdatePull(reader, func(message string) error {
		progress.DetailMsg = message
		if callbacks.Progress != nil {
			return callbacks.Progress("pulling-image", 25, message)
		}
		return nil
	})
	closeErr := reader.Close()
	if closeErr != nil {
		logx.Errorf("close image pull response: %v", closeErr)
	}
	if pullErr != nil {
		return fail("pulling-image", "拉取目标镜像失败，旧容器未修改", errors.Join(pullErr, closeErr))
	}
	if closeErr != nil {
		return fail("pulling-image", "关闭镜像拉取响应失败，旧容器未修改", closeErr)
	}

	resolved, _, err := s.docker.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return fail("resolving-image", "解析拉取后的镜像身份失败，旧容器未修改", err)
	}
	if !strings.HasPrefix(resolved.ID, "sha256:") {
		return fail("resolving-image", "Docker未返回可固定的镜像ID，旧容器未修改", errors.New("resolved image ID is missing or not immutable"))
	}
	if err := checkpoint(UpdateCheckpoint{Stage: "image-resolved", SnapshotRef: snapshotRef, OldContainerID: canonicalID, PreviousImageID: before.Image, ResolvedImageID: resolved.ID}); err != nil {
		return fail("resolving-image", "持久化镜像身份失败，旧容器未修改", err)
	}

	wasRunning := before.State != nil && before.State.Running
	if wasRunning {
		if err := publish("stopping-old", 35, "正在按原停止配置停止旧容器"); err != nil {
			return fail("stopping-old", "更新准备失败", err)
		}
		if err := checkpoint(UpdateCheckpoint{Stage: "stopping-old", SnapshotRef: snapshotRef, OldContainerID: canonicalID, PreviousImageID: before.Image, ResolvedImageID: resolved.ID}); err != nil {
			return fail("stopping-old", "持久化停止检查点失败，旧容器未修改", err)
		}
		if err := s.docker.ContainerStop(ctx, canonicalID, container.StopOptions{Signal: before.Config.StopSignal, Timeout: before.Config.StopTimeout}); err != nil {
			return fail("stopping-old", "停止旧容器失败", err)
		}
		if err := checkpoint(UpdateCheckpoint{Stage: "old-stopped", SnapshotRef: snapshotRef, OldContainerID: canonicalID, PreviousImageID: before.Image, ResolvedImageID: resolved.ID}); err != nil {
			return fail("old-stopped", "旧容器已停止但保存阶段检查点失败", err)
		}
	}

	if err := publish("renaming-old", 50, "正在保留并重命名旧容器"); err != nil {
		return fail("renaming-old", "更新准备失败", err)
	}
	if err := checkpoint(UpdateCheckpoint{Stage: "renaming-old", SnapshotRef: snapshotRef, OldContainerID: canonicalID, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
		return fail("renaming-old", "持久化旧容器改名检查点失败，旧容器未改名", err)
	}
	if err := s.docker.ContainerRename(ctx, canonicalID, backupName); err != nil {
		return fail("renaming-old", "重命名旧容器失败", err)
	}
	if err := checkpoint(UpdateCheckpoint{Stage: "old-renamed", SnapshotRef: snapshotRef, OldContainerID: canonicalID, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
		return fail("old-renamed", "旧容器已改名但保存阶段检查点失败", err)
	}

	if err := publish("creating-new", 70, "正在使用固定镜像ID创建新容器"); err != nil {
		return fail("creating-new", "更新准备失败", err)
	}
	if err := checkpoint(UpdateCheckpoint{Stage: "creating-new", SnapshotRef: snapshotRef, OldContainerID: canonicalID, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
		return fail("creating-new", "持久化新容器创建检查点失败，未创建新容器", err)
	}
	config := before.Config
	config.Image = resolved.ID
	hostConfig := preserveObservedVolumes(before.HostConfig, before.Mounts)
	networks := copyCreateNetworks(before.NetworkSettings)
	created, err := s.docker.ContainerCreate(ctx, config, hostConfig, networks, nil, name)
	if err != nil {
		return fail("creating-new", "创建新容器失败", err)
	}
	aliasErr := ops.AddAlias(taskID, operation.Target{ID: created.ID})
	checkpointErr := callbacks.OnCreated(created.ID)
	nameAliasErr := ops.AddAlias(taskID, operation.Target{Name: name})
	if err := errors.Join(aliasErr, checkpointErr, nameAliasErr); err != nil {
		return fail("created-new", "保护新容器身份失败", err)
	}
	if err := checkpoint(UpdateCheckpoint{Stage: "created-new", SnapshotRef: snapshotRef, OldContainerID: canonicalID, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
		return fail("created-new", "已创建新容器但保存阶段检查点失败", err)
	}
	if wasRunning {
		if err := publish("starting-new", 85, "正在启动新容器"); err != nil {
			return fail("starting-new", "启动新容器前的进度保存失败", err)
		}
		if err := s.docker.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			return fail("starting-new", "启动新容器失败", err)
		}
		if err := checkpoint(UpdateCheckpoint{Stage: "new-started", SnapshotRef: snapshotRef, OldContainerID: canonicalID, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
			return fail("new-started", "新容器已启动但保存阶段检查点失败", err)
		}
	}
	if deleteOld {
		if err := checkpoint(UpdateCheckpoint{Stage: "removing-old", SnapshotRef: snapshotRef, OldContainerID: canonicalID, PreviousImageID: before.Image, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
			return fail("removing-old", "持久化旧容器清理检查点失败，旧容器仍保留", err)
		}
		if err := s.docker.ContainerRemove(ctx, canonicalID, container.RemoveOptions{}); err != nil {
			return fail("cleanup-old", "新容器已启动，但删除旧容器失败", err)
		}
		if err := checkpoint(UpdateCheckpoint{Stage: "old-removed", SnapshotRef: snapshotRef, OldContainerID: canonicalID, PreviousImageID: before.Image, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
			return fail("old-removed", "旧容器已删除但清理检查点未能保存", err)
		}
	}
	if err := publish("completed", 100, "容器更新完成"); err != nil {
		return fail("replacement-prepared", "更新阶段保存失败", err)
	}
	return nil
}

func preserveObservedVolumes(host *container.HostConfig, mounts []container.MountPoint) *container.HostConfig {
	result := new(container.HostConfig)
	if host != nil {
		*result = *host
		result.Binds = append([]string(nil), host.Binds...)
		result.Mounts = append([]dockermount.Mount(nil), host.Mounts...)
	}
	observedByTarget := make(map[string]container.MountPoint, len(mounts))
	for _, observed := range mounts {
		if observed.Type == dockermount.TypeVolume && observed.Name != "" && observed.Destination != "" {
			observedByTarget[observed.Destination] = observed
		}
	}
	known := make(map[string]bool, len(result.Mounts)+len(result.Binds))
	for i := range result.Mounts {
		existing := &result.Mounts[i]
		if existing.Type == dockermount.TypeVolume && existing.Source == "" {
			if observed, ok := observedByTarget[existing.Target]; ok {
				existing.Source = observed.Name
				existing.ReadOnly = !observed.RW
			}
		}
		known[existing.Target] = true
	}
	keptBinds := make([]string, 0, len(result.Binds))
	for _, bind := range result.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) == 1 {
			if observed, ok := observedByTarget[parts[0]]; ok && !known[parts[0]] {
				result.Mounts = append(result.Mounts, dockermount.Mount{Type: dockermount.TypeVolume, Source: observed.Name, Target: parts[0]})
				known[parts[0]] = true
				continue
			}
		} else if len(parts) >= 2 {
			known[parts[1]] = true
		}
		keptBinds = append(keptBinds, bind)
	}
	result.Binds = keptBinds
	for target, observed := range observedByTarget {
		if known[target] {
			continue
		}
		result.Mounts = append(result.Mounts, dockermount.Mount{
			Type: dockermount.TypeVolume, Source: observed.Name, Target: target, ReadOnly: !observed.RW,
		})
	}
	return result
}

func copyCreateNetworks(settings *container.NetworkSettings) *network.NetworkingConfig {
	result := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	if settings == nil {
		return result
	}
	for name, endpoint := range settings.Networks {
		if endpoint == nil {
			continue
		}
		copy := endpoint.Copy()
		copy.NetworkID = ""
		copy.EndpointID = ""
		copy.Gateway = ""
		copy.IPAddress = ""
		copy.IPPrefixLen = 0
		copy.IPv6Gateway = ""
		copy.GlobalIPv6Address = ""
		copy.GlobalIPv6PrefixLen = 0
		result.EndpointsConfig[name] = copy
	}
	return result
}

func decodeUpdatePull(reader io.Reader, onProgress func(string) error) error {
	decoder := json.NewDecoder(reader)
	for {
		var message dockerMsgType.JSONMessage
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode image pull response: %w", err)
		}
		if message.Error != nil {
			return fmt.Errorf("image pull failed: %w", message.Error)
		}
		if onProgress != nil {
			text := message.Status
			if message.Progress != nil {
				text = fmt.Sprintf("%s: %s", message.Status, message.Progress.String())
			}
			if err := onProgress(text); err != nil {
				return err
			}
		}
	}
}

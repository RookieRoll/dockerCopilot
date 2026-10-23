package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dockermount "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	cerrdefs "github.com/docker/docker/errdefs"
	dockerMsgType "github.com/docker/docker/pkg/jsonmessage"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

// UpdateCheckpoint contains only non-secret facts that must be added to the
// live task state before the next update step. The complete inspected
// configuration is held separately through SaveSnapshot.
type UpdateCheckpoint struct {
	Stage           string
	SnapshotRef     string
	OldContainerID  string
	NewContainerID  string
	PreviousImageID string
	ResolvedImageID string
	BackupName      string
}

type UpdateRecoveryState struct {
	Stage           string
	OldContainerID  string
	NewContainerID  string
	BackupName      string
	ResolvedImageID string
}

type UpdateRecoveryResult struct {
	Status string
	Detail string
}

// UpdateCallbacks are narrow task-state/progress hooks supplied by the task
// owner. SaveSnapshot and Checkpoint are mandatory: updates never begin a
// destructive step unless their recovery facts are recorded in the live task.
type UpdateCallbacks struct {
	Progress     func(stage string, percentage int, message string) error
	Warning      func(message string)
	SaveSnapshot func([]byte) error
	Checkpoint   func(UpdateCheckpoint) error
	OnCreated    func(string) error
	// CandidateOwnerID isolates replacement labels when a parent batch task
	// performs multiple serial updates. Empty uses the task ID.
	CandidateOwnerID string
	Validation       UpdateValidationOptions
}

type UpdateValidationOptions struct {
	HealthcheckWait    time.Duration
	RunningObservation time.Duration
	PollInterval       time.Duration
}

const updateTaskLabel = "io.dockercopilot.update-task"

// UpdateWithOptions performs update preparation and replacement with a frozen
// image ID. deleteOld controls whether the renamed original is removed after
// the replacement is committed and, when applicable, passes runtime validation.
func (s *Service) UpdateWithOptions(ctx context.Context, id, name, imageRef string, deleteOld bool, taskID string, callbacks UpdateCallbacks) error {
	if s == nil || s.docker == nil {
		return errors.New("container Docker client is unavailable")
	}
	id, name, imageRef = strings.TrimSpace(id), strings.Trim(strings.TrimSpace(name), "/"), strings.TrimSpace(imageRef)
	if id == "" || name == "" || imageRef == "" {
		return errors.New("container ID, target name, and image reference are required")
	}
	if err := ValidateUpdateImageReference(imageRef); err != nil {
		return err
	}
	if taskID == "" {
		return errors.New("update task ID is required")
	}
	if callbacks.SaveSnapshot == nil || callbacks.Checkpoint == nil || callbacks.OnCreated == nil {
		return errors.New("update requires a runtime snapshot, critical checkpoint, and created-ID callback")
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
	warn := func(message, oldID string, cause error) {
		s.updateWarning(callbacks.Warning, message, oldID, cause)
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
		return fail("snapshot", "记录快照检查点失败，未修改旧容器", err)
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
		return fail("resolving-image", "记录镜像身份失败，旧容器未修改", err)
	}

	wasRunning := before.State != nil && before.State.Running
	validationResult := "原容器已停止，新容器保持停止；未执行运行验证"
	if wasRunning {
		if err := publish("stopping-old", 35, "正在按原停止配置停止旧容器"); err != nil {
			return fail("stopping-old", "更新准备失败", err)
		}
		if err := checkpoint(UpdateCheckpoint{Stage: "stopping-old", SnapshotRef: snapshotRef, OldContainerID: canonicalID, PreviousImageID: before.Image, ResolvedImageID: resolved.ID}); err != nil {
			return fail("stopping-old", "记录停止检查点失败，旧容器未修改", err)
		}
		if err := s.docker.ContainerStop(ctx, canonicalID, container.StopOptions{Signal: before.Config.StopSignal, Timeout: before.Config.StopTimeout}); err != nil {
			return fail("stopping-old", "停止旧容器失败", err)
		}
		if err := checkpoint(UpdateCheckpoint{Stage: "old-stopped", SnapshotRef: snapshotRef, OldContainerID: canonicalID, PreviousImageID: before.Image, ResolvedImageID: resolved.ID}); err != nil {
			return fail("old-stopped", "旧容器已停止但记录阶段检查点失败", err)
		}
	}

	if err := publish("renaming-old", 50, "正在保留并重命名旧容器"); err != nil {
		return fail("renaming-old", "更新准备失败", err)
	}
	if err := checkpoint(UpdateCheckpoint{Stage: "renaming-old", SnapshotRef: snapshotRef, OldContainerID: canonicalID, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
		return fail("renaming-old", "记录旧容器改名检查点失败，旧容器未改名", err)
	}
	if err := s.docker.ContainerRename(ctx, canonicalID, backupName); err != nil {
		return fail("renaming-old", "重命名旧容器失败", err)
	}
	if err := checkpoint(UpdateCheckpoint{Stage: "old-renamed", SnapshotRef: snapshotRef, OldContainerID: canonicalID, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
		return fail("old-renamed", "旧容器已改名但记录阶段检查点失败", err)
	}

	if err := publish("creating-new", 70, "正在使用固定镜像ID创建新容器"); err != nil {
		return fail("creating-new", "更新准备失败", err)
	}
	if err := checkpoint(UpdateCheckpoint{Stage: "creating-new", SnapshotRef: snapshotRef, OldContainerID: canonicalID, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
		return fail("creating-new", "记录新容器创建检查点失败，未创建新容器", err)
	}
	createConfig := *before.Config
	createConfig.Image = resolved.ID
	createConfig.Labels = cloneDockerLabels(before.Config.Labels)
	if createConfig.Labels == nil {
		createConfig.Labels = make(map[string]string)
	}
	candidateOwnerID := taskID
	if callbacks.CandidateOwnerID != "" {
		candidateOwnerID = callbacks.CandidateOwnerID
	}
	createConfig.Labels[updateTaskLabel] = candidateOwnerID
	createConfig.Labels[updateImageRefLabel] = imageRef
	hostConfig := preserveObservedVolumes(before.HostConfig, before.Mounts)
	networks := copyCreateNetworks(before.NetworkSettings)
	created, err := s.docker.ContainerCreate(ctx, &createConfig, hostConfig, networks, nil, name)
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
		return fail("created-new", "已创建新容器但记录阶段检查点失败", err)
	}
	if wasRunning {
		if err := publish("starting-new", 85, "正在启动新容器"); err != nil {
			return fail("starting-new", "启动新容器前的进度保存失败", err)
		}
		if err := s.docker.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			return fail("starting-new", "启动新容器失败", err)
		}
		if err := checkpoint(UpdateCheckpoint{Stage: "new-started", SnapshotRef: snapshotRef, OldContainerID: canonicalID, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
			return fail("new-started", "新容器已启动但记录阶段检查点失败", err)
		}
		if err := publish("validating-new", 90, "正在验证新容器健康或持续运行状态"); err != nil {
			return fail("validating-new", "保存容器验证阶段失败", err)
		}
		validationResult, err = s.validateStartedContainer(ctx, created.ID, callbacks.Validation, hasConfiguredHealthcheck(&createConfig), publish)
		if err != nil {
			return fail("validating-new", "新容器未通过运行验证", err)
		}
	}
	if err := checkpoint(UpdateCheckpoint{Stage: "replacement-committed", SnapshotRef: snapshotRef, OldContainerID: canonicalID, NewContainerID: created.ID, PreviousImageID: before.Image, ResolvedImageID: resolved.ID, BackupName: backupName}); err != nil {
		return fail("replacement-commit", "新容器已验证，但提交检查点未记录；未清理旧容器", err)
	}
	if deleteOld {
		cleanupStarted := true
		if err := publish("removing-old", 96, "替换已提交，正在清理旧容器"); err != nil {
			cleanupStarted = false
			warn("更新已提交，但旧容器清理阶段未能保存；旧容器已保留", canonicalID, err)
		}
		if cleanupStarted {
			if err := s.docker.ContainerRemove(ctx, canonicalID, container.RemoveOptions{}); err != nil {
				cleanupState := s.inspectCleanupState(ctx, canonicalID)
				warn("更新已提交，但旧容器清理结果需核对（"+cleanupState+"）", canonicalID, err)
			} else if err := publish("old-removed", 99, "新容器已提交，旧容器已清理"); err != nil {
				warn("旧容器已删除，但清理结果未能写入任务进度", canonicalID, err)
			}
		}
	}
	if err := publish("completed", 100, validationResult); err != nil {
		warn("更新已提交，但最终进度未能写入任务记录", canonicalID, err)
	}
	return nil
}

func (s *Service) inspectCleanupState(ctx context.Context, oldID string) string {
	inspectCtx := ctx
	cancel := func() {}
	if ctx == nil || ctx.Err() != nil {
		inspectCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	}
	defer cancel()
	_, err := s.docker.ContainerInspect(inspectCtx, oldID)
	switch {
	case err == nil:
		return "旧容器仍存在"
	case cerrdefs.IsNotFound(err):
		return "旧容器已不存在"
	default:
		return "无法确认旧容器状态"
	}
}

func (s *Service) updateWarning(callback func(string), message, oldID string, cause error) {
	if callback == nil {
		logx.Errorf("container update cleanup warning oldContainerID=%s: %s: %v", oldID, message, cause)
		return
	}
	callback(fmt.Sprintf("%s（旧容器ID %s）：%v", message, oldID, cause))
}

// RecoverUpdate uses only the private inspect snapshot and live task identity
// facts. It removes only a uniquely verified replacement created by this task,
// then restores the original container name and running state when possible.
// Any ambiguity is reported as unknown without deleting or renaming anything.
func (s *Service) RecoverUpdate(ctx context.Context, taskID string, snapshotBytes []byte, state UpdateRecoveryState, checkpoint func(UpdateCheckpoint) error) (UpdateRecoveryResult, error) {
	return s.RecoverUpdateWithOwner(ctx, taskID, taskID, snapshotBytes, state, checkpoint)
}

// RecoverUpdateWithOwner separates the operation lease owner (the parent
// task) from the unique replacement label owner (a batch item ID).
func (s *Service) RecoverUpdateWithOwner(ctx context.Context, taskID, candidateOwnerID string, snapshotBytes []byte, state UpdateRecoveryState, checkpoint func(UpdateCheckpoint) error) (UpdateRecoveryResult, error) {
	unknown := func(detail string) UpdateRecoveryResult {
		return UpdateRecoveryResult{Status: "unknown", Detail: detail}
	}
	if s == nil || s.docker == nil || ctx == nil || taskID == "" || len(snapshotBytes) == 0 || checkpoint == nil {
		return unknown("更新恢复所需的Docker客户端、上下文、任务身份或私有快照不可用"), errors.New("update recovery dependencies are incomplete")
	}
	var before dockertypes.ContainerJSON
	if err := json.Unmarshal(snapshotBytes, &before); err != nil {
		return unknown("私有容器快照损坏；未执行自动恢复"), fmt.Errorf("decode update snapshot: %w", err)
	}
	if before.ContainerJSONBase == nil || before.Config == nil || before.HostConfig == nil || before.ID == "" {
		return unknown("私有容器快照缺少原容器身份或配置；未执行自动恢复"), errors.New("update snapshot is missing required original container facts")
	}
	if before.State == nil {
		return unknown("私有容器快照缺少原容器运行状态；未执行自动恢复"), errors.New("update snapshot is missing original container state")
	}
	oldID := state.OldContainerID
	if oldID == "" {
		oldID = before.ID
	}
	if oldID != before.ID {
		return unknown("任务记录中的旧容器ID与快照不一致；未执行自动恢复"), errors.New("update recovery old container ID does not match snapshot")
	}
	stage := state.Stage
	switch stage {
	case "", "preflight", "snapshot", "snapshot-saved", "pulling-image", "resolving-image", "image-resolved":
		return UpdateRecoveryResult{Status: "not-needed", Detail: "更新尚未进入旧容器修改阶段"}, nil
	case "replacement-committed", "removing-old", "old-removed", "cleanup-old":
		return UpdateRecoveryResult{Status: "not-needed", Detail: "新容器已持久提交；清理旧容器不会触发回滚"}, nil
	case "stopping-old", "old-stopped", "renaming-old", "old-renamed", "creating-new", "created-new", "starting-new", "new-started", "validating-new", "recovery-new-discovered", "recovery-removing-new", "recovery-restoring-name", "recovery-starting-old", "recovery-stopping-old":
	default:
		return unknown("更新阶段无法识别；未执行自动恢复"), nil
	}
	if state.ResolvedImageID == "" {
		return unknown("恢复所需的固定镜像ID缺失；未执行自动恢复"), nil
	}
	if state.BackupName == "" && stage != "stopping-old" && stage != "old-stopped" {
		return unknown("恢复所需的备份名称缺失；未执行自动恢复"), nil
	}

	old, err := s.docker.ContainerInspect(ctx, oldID)
	if err != nil {
		return unknown("无法按原容器ID核对旧容器；未执行自动恢复"), fmt.Errorf("inspect original container during recovery: %w", err)
	}
	if old.ID != oldID {
		return unknown("Docker返回的旧容器ID与快照不一致；未执行自动恢复"), nil
	}
	originalName := strings.TrimLeft(strings.TrimSpace(before.Name), "/")
	if originalName == "" {
		return unknown("快照缺少原容器名称；未执行自动恢复"), nil
	}
	oldName := strings.TrimLeft(strings.TrimSpace(old.Name), "/")
	if oldName != originalName && (state.BackupName == "" || oldName != state.BackupName) {
		return unknown("旧容器名称已被外部修改；未执行自动恢复"), nil
	}

	newID := state.NewContainerID
	var candidate *dockertypes.ContainerJSON
	if newID != "" {
		inspected, inspectErr := s.docker.ContainerInspect(ctx, newID)
		if inspectErr == nil {
			candidate = &inspected
		} else if !cerrdefs.IsNotFound(inspectErr) {
			return unknown("无法核对已记录的新容器ID；未执行自动恢复"), fmt.Errorf("inspect recorded replacement during recovery: %w", inspectErr)
		}
	}
	if strings.TrimSpace(candidateOwnerID) == "" {
		return unknown("任务更新项缺少候选容器所有权标记；未执行自动恢复"), errors.New("candidate owner ID is required")
	}
	args := filters.NewArgs(filters.Arg("label", updateTaskLabel+"="+candidateOwnerID))
	listed, listErr := s.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if listErr != nil {
		return unknown("无法按任务标签核对可能的新容器；未执行自动恢复"), fmt.Errorf("list task-labeled replacement containers: %w", listErr)
	}
	listedCandidate := false
	for _, item := range listed {
		inspected, inspectErr := s.docker.ContainerInspect(ctx, item.ID)
		if inspectErr != nil {
			return unknown("无法检查任务标签对应的新容器；未执行自动恢复"), fmt.Errorf("inspect task-labeled replacement: %w", inspectErr)
		}
		if !matchesUpdateCandidate(inspected, candidateOwnerID, state.ResolvedImageID, originalName) {
			return unknown("任务标签对应容器与记录的镜像或名称不匹配；未执行删除"), nil
		}
		if candidate != nil && candidate.ID != inspected.ID {
			return unknown("发现多个任务标签对应的新容器；未执行自动恢复"), nil
		}
		candidate = &inspected
		listedCandidate = true
	}
	if candidate != nil && !listedCandidate {
		return unknown("记录的新容器未出现在唯一任务标签结果中；未执行自动恢复"), nil
	}
	if candidate != nil {
		if !matchesUpdateCandidate(*candidate, candidateOwnerID, state.ResolvedImageID, originalName) {
			return unknown("记录的新容器身份与任务标签、固定镜像ID或目标名称不匹配；未执行删除"), nil
		}
		if newID == "" {
			newID = candidate.ID
			if err := checkpoint(UpdateCheckpoint{Stage: "recovery-new-discovered", OldContainerID: oldID, NewContainerID: newID, ResolvedImageID: state.ResolvedImageID, BackupName: state.BackupName}); err != nil {
				return unknown("已发现新容器，但任务身份记录失败；未执行删除"), fmt.Errorf("record discovered replacement identity: %w", err)
			}
		}
		if err := s.Operations().AddAlias(taskID, operation.Target{ID: newID}); err != nil {
			return unknown("已发现新容器，但无法保护其ID；未执行删除"), fmt.Errorf("protect replacement identity: %w", err)
		}
	}

	// Never remove by name. A name lookup must be either the original ID, the
	// verified task-owned replacement, or absent.
	nameOccupant, nameErr := s.docker.ContainerInspect(ctx, originalName)
	if nameErr == nil && nameOccupant.ID != oldID && (candidate == nil || nameOccupant.ID != candidate.ID) {
		return unknown("目标容器名称已被其他容器占用；未执行自动恢复"), nil
	} else if nameErr != nil && !cerrdefs.IsNotFound(nameErr) {
		return unknown("无法核对目标容器名称占用情况；未执行自动恢复"), fmt.Errorf("inspect update target name during recovery: %w", nameErr)
	}

	if candidate != nil {
		if err := checkpoint(UpdateCheckpoint{Stage: "recovery-removing-new", OldContainerID: oldID, NewContainerID: candidate.ID, ResolvedImageID: state.ResolvedImageID, BackupName: state.BackupName}); err != nil {
			return unknown("删除新容器前无法记录恢复检查点；未执行删除"), fmt.Errorf("record recovery removal checkpoint: %w", err)
		}
		if err := s.docker.ContainerRemove(ctx, candidate.ID, container.RemoveOptions{Force: true}); err != nil {
			remaining, inspectErr := s.docker.ContainerInspect(ctx, candidate.ID)
			if inspectErr == nil || !cerrdefs.IsNotFound(inspectErr) {
				return unknown("无法确认新容器已删除；旧容器保持原状"), errors.Join(fmt.Errorf("remove verified replacement: %w", err), inspectErr)
			}
			_ = remaining
		}
	}

	if oldName == state.BackupName {
		if err := checkpoint(UpdateCheckpoint{Stage: "recovery-restoring-name", OldContainerID: oldID, NewContainerID: newID, ResolvedImageID: state.ResolvedImageID, BackupName: state.BackupName}); err != nil {
			return unknown("恢复旧容器名称前无法记录检查点"), fmt.Errorf("record recovery rename checkpoint: %w", err)
		}
		if err := s.docker.ContainerRename(ctx, oldID, originalName); err != nil {
			return unknown("恢复旧容器名称失败；请人工处理"), fmt.Errorf("rename original container back: %w", err)
		}
	}
	old, err = s.docker.ContainerInspect(ctx, oldID)
	if err != nil {
		return unknown("恢复名称后无法核对旧容器；请人工处理"), fmt.Errorf("verify restored original container: %w", err)
	}
	if strings.TrimLeft(strings.TrimSpace(old.Name), "/") != originalName {
		return unknown("旧容器名称未恢复；请人工处理"), nil
	}
	originallyRunning := before.State != nil && before.State.Running
	currentlyRunning := old.State != nil && old.State.Running
	if originallyRunning && !currentlyRunning {
		if err := checkpoint(UpdateCheckpoint{Stage: "recovery-starting-old", OldContainerID: oldID, NewContainerID: newID, ResolvedImageID: state.ResolvedImageID, BackupName: state.BackupName}); err != nil {
			return unknown("启动旧容器前无法记录检查点"), fmt.Errorf("record recovery start checkpoint: %w", err)
		}
		if err := s.docker.ContainerStart(ctx, oldID, container.StartOptions{}); err != nil {
			return unknown("重新启动旧容器失败；请人工处理"), fmt.Errorf("restart original container after rollback: %w", err)
		}
	} else if !originallyRunning && currentlyRunning {
		if err := checkpoint(UpdateCheckpoint{Stage: "recovery-stopping-old", OldContainerID: oldID, NewContainerID: newID, ResolvedImageID: state.ResolvedImageID, BackupName: state.BackupName}); err != nil {
			return unknown("停止旧容器前无法记录检查点"), fmt.Errorf("record recovery stop checkpoint: %w", err)
		}
		if err := s.docker.ContainerStop(ctx, oldID, container.StopOptions{Signal: before.Config.StopSignal, Timeout: before.Config.StopTimeout}); err != nil {
			return unknown("恢复旧容器的停止状态失败；请人工处理"), fmt.Errorf("stop original container after rollback: %w", err)
		}
	}
	verified, err := s.docker.ContainerInspect(ctx, oldID)
	if err != nil {
		return unknown("恢复旧容器状态后无法核对；请人工处理"), fmt.Errorf("verify restored original container state: %w", err)
	}
	if verified.ID != oldID || strings.TrimLeft(strings.TrimSpace(verified.Name), "/") != originalName || verified.State == nil || verified.State.Running != originallyRunning {
		return unknown("Docker操作返回后，旧容器身份、名称或运行状态与快照不一致；请人工处理"), nil
	}
	return UpdateRecoveryResult{Status: "restored", Detail: "已移除经任务身份核验的新容器，并恢复旧容器名称与原运行状态"}, nil
}

func matchesUpdateCandidate(candidate dockertypes.ContainerJSON, taskID, imageID, name string) bool {
	if candidate.ContainerJSONBase == nil || candidate.Config == nil || candidate.ID == "" || candidate.Image != imageID {
		return false
	}
	if candidate.Config.Labels[updateTaskLabel] != taskID || strings.TrimLeft(strings.TrimSpace(candidate.Name), "/") != name {
		return false
	}
	return true
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

func cloneDockerLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func (s *Service) validateStartedContainer(ctx context.Context, id string, options UpdateValidationOptions, expectedHealthcheck bool, publish func(string, int, string) error) (string, error) {
	healthWait := options.HealthcheckWait
	if healthWait <= 0 {
		healthWait = 120 * time.Second
	}
	observation := options.RunningObservation
	if observation <= 0 {
		observation = 10 * time.Second
	}
	interval := options.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	started := time.Now()
	healthDeadline := started.Add(healthWait)
	observationDeadline := started.Add(observation)
	// The last read must happen after the observation window. Give that read
	// its own bounded budget, independent of the polling frequency.
	inspectGrace := time.Second
	validationDeadline := observationDeadline.Add(inspectGrace)
	if expectedHealthcheck {
		validationDeadline = healthDeadline
	}
	var initialRestartCount int
	var initialStartedAt string
	firstInspect := true
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		now := time.Now()
		if expectedHealthcheck && !now.Before(healthDeadline) {
			return "", fmt.Errorf("replacement container healthcheck did not become healthy within %s", healthWait)
		}
		if !expectedHealthcheck && !now.Before(validationDeadline) {
			return "", fmt.Errorf("replacement container did not remain running for %s; final inspect exceeded its bounded grace", observation)
		}
		inspectStartedAt := time.Now()
		inspectCtx, cancelInspect := context.WithDeadline(ctx, validationDeadline)
		inspected, err := s.docker.ContainerInspect(inspectCtx, id)
		cancelInspect()
		if err != nil {
			if ctx.Err() == nil && !time.Now().Before(validationDeadline) {
				return "", fmt.Errorf("replacement validation deadline elapsed while inspecting: %w", err)
			}
			return "", fmt.Errorf("inspect replacement during validation: %w", err)
		}
		observedAt := time.Now()
		// A new image may introduce a healthcheck that the old image lacked.
		if hasConfiguredHealthcheck(inspected.Config) || (inspected.State != nil && inspected.State.Health != nil) {
			expectedHealthcheck = true
			validationDeadline = healthDeadline
		}
		if expectedHealthcheck && !observedAt.Before(healthDeadline) {
			return "", fmt.Errorf("replacement container healthcheck did not become healthy within %s", healthWait)
		}
		if !expectedHealthcheck && !observedAt.Before(validationDeadline) {
			return "", fmt.Errorf("replacement container did not remain running for %s; final inspect exceeded its bounded grace", observation)
		}
		if inspected.State == nil || !inspected.State.Running {
			state := "unknown"
			if inspected.State != nil && inspected.State.Status != "" {
				state = inspected.State.Status
			}
			return "", fmt.Errorf("replacement container is not running (state=%s)", state)
		}
		if inspected.State.Paused || inspected.State.Dead || inspected.State.Restarting ||
			(inspected.State.Status != "" && inspected.State.Status != "running") {
			return "", fmt.Errorf("replacement container is not stably running (state=%s, paused=%t, restarting=%t, dead=%t)", inspected.State.Status, inspected.State.Paused, inspected.State.Restarting, inspected.State.Dead)
		}
		if firstInspect {
			initialRestartCount = inspected.RestartCount
			initialStartedAt = inspected.State.StartedAt
			firstInspect = false
			if !expectedHealthcheck && initialRestartCount > 0 {
				return "", errors.New("replacement container restarted before sustained-running validation began")
			}
			if !expectedHealthcheck && initialStartedAt == "" {
				return "", errors.New("replacement container inspect omitted StartedAt; sustained-running state cannot be verified")
			}
		} else if !expectedHealthcheck && (inspected.RestartCount != initialRestartCount || inspected.State.StartedAt != initialStartedAt) {
			return "", errors.New("replacement container restarted during the sustained-running validation window")
		}
		hasHealthcheck := hasConfiguredHealthcheck(inspected.Config)
		if expectedHealthcheck && (inspected.Config == nil || !hasHealthcheck || inspected.State.Health == nil) {
			return "", errors.New("replacement container inspect omitted configured healthcheck status")
		}
		if health := inspected.State.Health; health != nil {
			switch health.Status {
			case "healthy":
				if !time.Now().Before(healthDeadline) {
					return "", fmt.Errorf("replacement container healthcheck became healthy after deadline %s", healthWait)
				}
				return "新容器 healthcheck 已通过", nil
			case "unhealthy":
				return "", errors.New("replacement container healthcheck is unhealthy")
			case "starting":
				hasHealthcheck = true
			case "":
				return "", errors.New("replacement container returned an empty healthcheck status")
			default:
				return "", fmt.Errorf("replacement container returned unknown healthcheck status %q", health.Status)
			}
		}
		now = time.Now()
		if !hasHealthcheck {
			if !now.Before(observationDeadline) && !inspectStartedAt.Before(observationDeadline) {
				return "新容器已持续运行验证窗口；仅验证容器持续运行，不代表应用功能正常", nil
			}
		} else if !now.Before(healthDeadline) {
			return "", fmt.Errorf("replacement container healthcheck did not become healthy within %s", healthWait)
		}
		remaining := observationDeadline.Sub(now)
		if hasHealthcheck {
			remaining = healthDeadline.Sub(now)
		}
		wait := interval
		if remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", ctx.Err()
		case <-timer.C:
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		if err := publish("validating-new", 92, "新容器仍在运行，继续等待验证结果"); err != nil {
			return "", err
		}
	}
}

func hasConfiguredHealthcheck(config *container.Config) bool {
	if config == nil || config.Healthcheck == nil || len(config.Healthcheck.Test) == 0 {
		return false
	}
	return !strings.EqualFold(config.Healthcheck.Test[0], "NONE")
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

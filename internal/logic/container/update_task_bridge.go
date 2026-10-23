package container

import (
	"context"
	"errors"
	"fmt"

	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/task"
)

// updateSnapshotStore is the task runtime's private, process-local snapshot
// contract. It deliberately exposes no filesystem or startup-recovery API.
type updateSnapshotStore interface {
	SaveSnapshot(taskID string, content []byte) error
	ReadSnapshot(taskID string) ([]byte, error)
	SaveItemSnapshot(taskID, snapshotID string, content []byte) error
	ReadItemSnapshot(taskID, snapshotID string) ([]byte, error)
}

// updateTaskBridge maps the shared container update service's facts to either
// a single task or one item in a serial batch task.
type updateTaskBridge struct {
	taskID     string
	ownerID    string
	targetName string
	itemIndex  int // negative for a single-container task
	execution  *task.Execution
	store      updateSnapshotStore
	service    *containerService.Service
}

func (b updateTaskBridge) saveSnapshot(snapshot []byte) error {
	if b.store == nil {
		return errors.New("task runtime snapshot store is unavailable")
	}
	if b.itemIndex >= 0 {
		return b.store.SaveItemSnapshot(b.taskID, b.ownerID, snapshot)
	}
	return b.store.SaveSnapshot(b.taskID, snapshot)
}

func (b updateTaskBridge) checkpoint(value containerService.UpdateCheckpoint) error {
	if b.execution == nil {
		return errors.New("task execution is unavailable for update checkpoint")
	}
	stage := value.Stage
	if b.itemIndex >= 0 {
		stage = "batch-item-" + stage
	}
	mutate := func(record *task.Record) {
		if value.SnapshotRef != "" && b.itemIndex < 0 {
			record.SnapshotRef = value.SnapshotRef
		}
		if value.OldContainerID != "" {
			record.OldContainerID = value.OldContainerID
			record.ContainerIDs = appendTaskIdentity(record.ContainerIDs, value.OldContainerID)
		}
		if value.NewContainerID != "" {
			record.NewContainerID = value.NewContainerID
			record.ContainerIDs = appendTaskIdentity(record.ContainerIDs, value.NewContainerID)
		}
		if value.PreviousImageID != "" {
			record.ImageIDs = appendTaskIdentity(record.ImageIDs, value.PreviousImageID)
		}
		if value.ResolvedImageID != "" {
			record.ImageIDs = appendTaskIdentity(record.ImageIDs, value.ResolvedImageID)
		}
		if value.BackupName != "" {
			record.BackupContainerName = value.BackupName
		}
		if b.itemIndex < 0 {
			record.ProtectedNames = appendTaskIdentity(record.ProtectedNames, b.targetName)
			return
		}
		if b.itemIndex >= len(record.Items) {
			return
		}
		item := &record.Items[b.itemIndex]
		item.Stage = value.Stage
		if value.OldContainerID != "" {
			item.OldContainerID = value.OldContainerID
		}
		if value.NewContainerID != "" {
			item.NewContainerID = value.NewContainerID
		}
		if value.PreviousImageID != "" {
			item.ImageIDs = appendTaskIdentity(item.ImageIDs, value.PreviousImageID)
		}
		if value.ResolvedImageID != "" {
			item.ImageIDs = appendTaskIdentity(item.ImageIDs, value.ResolvedImageID)
		}
		if value.BackupName != "" {
			item.BackupContainerName = value.BackupName
		}
		if updateStageNeedsRecovery(value.Stage) {
			item.ProtectedNames = appendTaskIdentity(item.ProtectedNames, b.targetName)
		}
	}
	if value.Stage == "replacement-committed" {
		if b.itemIndex >= 0 {
			return b.execution.CriticalItemCommit(b.itemIndex, stage, mutate)
		}
		return b.execution.CriticalCommit(stage, mutate)
	}
	return b.execution.Critical(stage, mutate)
}

func (b updateTaskBridge) created(newID string, addAlias func(string) error) error {
	if b.execution == nil {
		return errors.New("task execution is unavailable for created-container checkpoint")
	}
	checkpointErr := b.execution.Critical("created-new-container", func(record *task.Record) {
		record.NewContainerID = newID
		record.ContainerIDs = appendTaskIdentity(record.ContainerIDs, newID)
		if b.itemIndex >= 0 {
			if b.itemIndex < len(record.Items) {
				record.Items[b.itemIndex].NewContainerID = newID
				record.Items[b.itemIndex].ProtectedNames = appendTaskIdentity(record.Items[b.itemIndex].ProtectedNames, b.targetName)
			}
		} else {
			record.ProtectedNames = appendTaskIdentity(record.ProtectedNames, b.targetName)
		}
	})
	if addAlias != nil {
		return errors.Join(checkpointErr, addAlias(newID))
	}
	return checkpointErr
}

func (b updateTaskBridge) recover(ctx context.Context, _ error) (task.Recovery, error) {
	if b.execution == nil || b.store == nil || b.service == nil {
		return task.Recovery{Status: "unknown", Detail: "本进程更新恢复所需的任务状态不可用"}, errors.New("update recovery dependencies are unavailable")
	}
	record := b.execution.Current()
	if b.itemIndex >= 0 {
		if b.itemIndex >= len(record.Items) {
			return task.Recovery{Status: "unknown", Detail: "更新项索引无效"}, errors.New("update recovery item index is invalid")
		}
		item := record.Items[b.itemIndex]
		if item.Committed {
			return task.Recovery{Status: "not-needed", Detail: "替换已提交；后续仅清理旧容器"}, nil
		}
		record.Stage, record.OldContainerID, record.NewContainerID = item.Stage, item.OldContainerID, item.NewContainerID
		record.BackupContainerName = item.BackupContainerName
		record.SnapshotRef = item.SnapshotRef
		record.ImageIDs = append([]string(nil), item.ImageIDs...)
	}
	if !updateStageNeedsRecovery(record.Stage) {
		return task.Recovery{Status: "not-needed", Detail: "更新尚未进入旧容器修改阶段"}, nil
	}
	if b.itemIndex >= 0 && record.Stage == "replacement-committed" {
		// The Docker replacement is not treated as committed until the current
		// task execution has recorded the item commit bit. A rejected commit
		// checkpoint must still take the pre-commit recovery path.
		record.Stage = "new-started"
	}
	snapshotID := record.SnapshotRef
	if b.itemIndex >= 0 {
		snapshotID = record.Items[b.itemIndex].SnapshotRef
	}
	if snapshotID == "" {
		return task.Recovery{Status: "unknown", Detail: "更新快照引用缺失；本进程未执行自动恢复"}, nil
	}
	var snapshot []byte
	var err error
	if b.itemIndex >= 0 {
		snapshot, err = b.store.ReadItemSnapshot(b.taskID, snapshotID)
	} else {
		snapshot, err = b.store.ReadSnapshot(b.taskID)
	}
	if err != nil {
		return task.Recovery{Status: "unknown", Detail: "无法读取本进程中的更新快照；未执行自动恢复"}, err
	}
	resolvedImageID := ""
	if len(record.ImageIDs) > 0 {
		resolvedImageID = record.ImageIDs[len(record.ImageIDs)-1]
	}
	ownerID := b.ownerID
	if ownerID == "" {
		ownerID = b.taskID
	}
	result, recoverErr := b.service.RecoverUpdateWithOwner(ctx, b.taskID, ownerID, snapshot, containerService.UpdateRecoveryState{
		Stage: record.Stage, OldContainerID: record.OldContainerID, NewContainerID: record.NewContainerID,
		BackupName: record.BackupContainerName, ResolvedImageID: resolvedImageID,
	}, func(checkpoint containerService.UpdateCheckpoint) error {
		return b.execution.RecoveryCheckpoint(ctx, checkpoint.Stage, func(current *task.Record) {
			if checkpoint.OldContainerID != "" {
				current.OldContainerID = checkpoint.OldContainerID
				current.ContainerIDs = appendTaskIdentity(current.ContainerIDs, checkpoint.OldContainerID)
			}
			if checkpoint.NewContainerID != "" {
				current.NewContainerID = checkpoint.NewContainerID
				current.ContainerIDs = appendTaskIdentity(current.ContainerIDs, checkpoint.NewContainerID)
			}
			if checkpoint.BackupName != "" {
				current.BackupContainerName = checkpoint.BackupName
			}
			if b.itemIndex >= 0 && b.itemIndex < len(current.Items) {
				item := &current.Items[b.itemIndex]
				item.Stage = checkpoint.Stage
				if checkpoint.OldContainerID != "" {
					item.OldContainerID = checkpoint.OldContainerID
				}
				if checkpoint.NewContainerID != "" {
					item.NewContainerID = checkpoint.NewContainerID
				}
				if checkpoint.BackupName != "" {
					item.BackupContainerName = checkpoint.BackupName
				}
			}
		})
	})
	if recoverErr != nil {
		return task.Recovery{Status: result.Status, Detail: result.Detail}, fmt.Errorf("recover container update: %w", recoverErr)
	}
	return task.Recovery{Status: result.Status, Detail: result.Detail}, nil
}

func updateStageNeedsRecovery(stage string) bool {
	switch stage {
	case "", "starting", "preflight", "snapshot", "snapshot-saved", "pulling-image", "resolving-image", "image-resolved":
		return false
	default:
		return true
	}
}

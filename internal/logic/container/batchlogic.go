package container

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

const MaxContainerBatchTargets = 100

type BatchRequest = types.ContainerBatchReq

type BatchLogic struct {
	logx.Logger
	ctx                context.Context
	service            *containerService.Service
	imageGate          *imageService.AdmissionGate
	runner             *task.Runner
	store              *task.Store
	mutationGate       *operation.MutationGate
	deleteOldDefault   bool
	healthcheckWait    time.Duration
	runningObservation time.Duration
	optionsErr         error
}

func NewBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchLogic {
	var service *containerService.Service
	var imageGate *imageService.AdmissionGate
	var runner *task.Runner
	var gate *operation.MutationGate
	var store *task.Store
	var deleteOldDefault bool
	var healthcheckWait, runningObservation time.Duration
	var optionsErr error
	if svcCtx != nil {
		service, runner, gate = svcCtx.ContainerService(), svcCtx.TaskRunner, svcCtx.MutationGate()
		imageGate = svcCtx.ImageService().AdmissionGate()
		store = svcCtx.TaskStore
		deleteOldDefault = svcCtx.Config.ResolveDeleteOldContainer(nil)
		if options, err := svcCtx.Config.TaskOptions(); err != nil {
			optionsErr = err
		} else {
			healthcheckWait, runningObservation = options.HealthcheckWait, options.RunningObservation
		}
	}
	return &BatchLogic{Logger: logx.WithContext(ctx), ctx: ctx, service: service, imageGate: imageGate, runner: runner, store: store, mutationGate: gate, deleteOldDefault: deleteOldDefault, healthcheckWait: healthcheckWait, runningObservation: runningObservation, optionsErr: optionsErr}
}

type batchRequestError string

func (e batchRequestError) Error() string     { return string(e) }
func (e batchRequestError) HTTPStatus() int   { return http.StatusBadRequest }
func (e batchRequestError) ErrorCode() string { return "INVALID_BATCH_REQUEST" }

type batchUnavailableError string

func (e batchUnavailableError) Error() string     { return string(e) }
func (e batchUnavailableError) HTTPStatus() int   { return http.StatusServiceUnavailable }
func (e batchUnavailableError) ErrorCode() string { return "TASK_SERVICE_UNAVAILABLE" }

// Submit accepts a durable batch task. Each target is inspected before
// reservation, exact targets are de-duplicated, and operation conflicts are
// retained as skipped item results while independent targets continue.
func (l *BatchLogic) Submit(req *BatchRequest) (string, error) {
	if req == nil {
		return "", batchRequestError("batch request is required")
	}
	action := containerService.Action(strings.TrimSpace(req.Action))
	if action != containerService.ActionStart && action != containerService.ActionStop && action != containerService.ActionRestart && action != "update" {
		return "", batchRequestError("batch action must be start, stop, restart, or update")
	}
	if len(req.ContainerIDs) == 0 || len(req.ContainerIDs) > MaxContainerBatchTargets {
		return "", batchRequestError(fmt.Sprintf("batch must include between 1 and %d container IDs", MaxContainerBatchTargets))
	}
	ids := make([]string, 0, len(req.ContainerIDs))
	seenIDs := make(map[string]struct{}, len(req.ContainerIDs))
	for _, rawID := range req.ContainerIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return "", batchRequestError("container IDs cannot be empty")
		}
		// Docker names are case-sensitive; only exact duplicate input tokens
		// are collapsed here. Canonical inspected IDs are normalized below.
		key := id
		if _, exists := seenIDs[key]; exists {
			continue
		}
		seenIDs[key] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "", batchRequestError("batch must include at least one unique container ID")
	}
	if l.runner == nil || l.service == nil || l.mutationGate == nil || (action == "update" && (l.store == nil || l.optionsErr != nil)) {
		return "", batchUnavailableError("task-backed container batch operations are unavailable")
	}

	taskID := uuid.NewString()
	operations := make([]task.BatchOperation, 0, len(ids))
	targets := make([]operation.Target, 0, len(ids))
	seenCanonical := make(map[string]struct{}, len(ids))
	var executionMu sync.Mutex
	var executionRef *task.Execution
	for _, id := range ids {
		item := task.BatchOperation{Item: task.Item{ID: id}}
		inspect, err := l.service.Inspect(l.ctx, id)
		if err != nil {
			item.InitialError = "inspect container before batch operation: " + err.Error()
			operations = append(operations, item)
			continue
		}
		if inspect.ContainerJSONBase == nil {
			item.InitialError = "inspect container before batch operation: Docker returned no container identity"
			operations = append(operations, item)
			continue
		}
		canonicalID := strings.TrimSpace(inspect.ID)
		if canonicalID == "" {
			item.InitialError = "inspect container before batch operation: Docker returned no container ID"
			operations = append(operations, item)
			continue
		}
		name := strings.TrimLeft(strings.TrimSpace(inspect.Name), "/")
		if name == "" {
			item.InitialError = "inspect container before batch operation: Docker returned no container name"
			operations = append(operations, item)
			continue
		}
		if action == "update" {
			if inspect.Config == nil || strings.TrimSpace(inspect.Config.Image) == "" {
				item.InitialError = "inspect container before batch update: Docker returned no configured image reference"
				operations = append(operations, item)
				continue
			}
			if inspect.ContainerJSONBase == nil || strings.TrimSpace(inspect.Image) == "" {
				item.InitialError = "inspect container before batch update: Docker returned no immutable image ID"
				operations = append(operations, item)
				continue
			}
			item.Item.ImageRef = strings.TrimSpace(inspect.Config.Image)
			item.Item.ImageIDs = []string{inspect.Image}
			item.Item.SnapshotRef = uuid.NewString()
		}
		if _, duplicate := seenCanonical[strings.ToLower(canonicalID)]; duplicate {
			continue
		}
		seenCanonical[strings.ToLower(canonicalID)] = struct{}{}
		item.Item.ID, item.Item.Name = canonicalID, name
		targets = append(targets, operation.Target{ID: canonicalID, Name: name})
		operations = append(operations, item)
	}
	reservation := operation.Reservation{}
	if len(targets) > 0 {
		var err error
		reservation, err = l.service.Operations().ReserveAvailableBatchContext(l.ctx, operation.Request{
			TaskID: taskID, Kind: "container-batch-" + string(action),
		}, targets)
		if err != nil {
			return "", err
		}
	}
	conflicts := make(map[string]operation.TargetConflict, len(reservation.Conflicts))
	for _, conflict := range reservation.Conflicts {
		conflicts[strings.ToLower(conflict.Target.ID)] = conflict
	}
	for index := range operations {
		if operations[index].InitialError != "" {
			continue
		}
		if conflict, exists := conflicts[strings.ToLower(operations[index].Item.ID)]; exists {
			operations[index].SkipReason = fmt.Sprintf("container is held by %s task %s", conflict.Kind, conflict.TaskID)
			continue
		}
		itemTarget := operation.Target{ID: operations[index].Item.ID, Name: operations[index].Item.Name}
		operations[index].PreserveFinalStage = action == "update"
		batchTotal := len(operations)
		if action == "update" {
			itemIndex := index
			itemSnapshot := operations[itemIndex].Item
			operations[itemIndex].Recover = func(recoveryCtx context.Context, cause error) (task.Recovery, error) {
				executionMu.Lock()
				execution := executionRef
				executionMu.Unlock()
				return l.recoverBatchUpdateItem(recoveryCtx, taskID, execution, itemIndex, itemSnapshot, cause)
			}
		}
		operations[index].Run = func(ctx context.Context, execution *task.Execution) error {
			executionMu.Lock()
			executionRef = execution
			executionMu.Unlock()
			if action == "update" {
				deleteOld := l.deleteOldDefault
				if req.DeleteOldContainer != nil {
					deleteOld = *req.DeleteOldContainer
				}
				return l.runBatchUpdate(ctx, taskID, execution, index, batchTotal, itemTarget, operations[index].Item, deleteOld)
			}
			return l.service.OperateOwned(ctx, taskID, itemTarget, containerService.Action(action))
		}
	}
	items := make([]task.Item, len(operations))
	for i := range operations {
		items[i] = operations[i].Item
	}
	record := task.Record{
		TaskID: taskID, Type: "container-batch-" + string(action), Status: task.StatusQueued,
		Name: fmt.Sprintf("%d containers", len(operations)), Items: items, ContainerIDs: make([]string, 0, len(items)),
		RequestID: observability.RequestIDFromContext(l.ctx),
	}
	for _, item := range items {
		if item.ID != "" {
			record.ContainerIDs = appendTaskIdentity(record.ContainerIDs, item.ID)
		}
	}
	if action == "update" {
		deleteOld := l.deleteOldDefault
		if req.DeleteOldContainer != nil {
			deleteOld = *req.DeleteOldContainer
		}
		record.Options.DeleteOldContainer = &deleteOld
		if l.healthcheckWait > 0 {
			record.Options.HealthWaitSeconds = durationSeconds(l.healthcheckWait)
		}
	}
	var leaseMu sync.Mutex
	var gateLease *operation.MutationLease
	var settled sync.Once
	var recoverFn task.RecoveryFunc
	if action == "update" {
		recoverFn = func(recoveryCtx context.Context, cause error) (task.Recovery, error) {
			executionMu.Lock()
			execution := executionRef
			executionMu.Unlock()
			return l.recoverBatchUpdates(recoveryCtx, taskID, execution, cause)
		}
	}
	if action == "update" {
		l.imageGate.Lock()
		defer l.imageGate.Unlock()
	}
	acceptedID, err := l.runner.SubmitBatchWithHooks(record, operations, recoverFn, func() error {
		gate, acquireErr := l.mutationGate.Acquire(context.Background(), taskID)
		if acquireErr != nil {
			return acquireErr
		}
		if reservation.Lease != nil {
			if confirmErr := reservation.Lease.Confirm(); confirmErr != nil {
				gate.Release()
				return confirmErr
			}
		}
		leaseMu.Lock()
		gateLease = gate
		leaseMu.Unlock()
		return nil
	}, func() {
		settled.Do(func() {
			leaseMu.Lock()
			gate := gateLease
			gateLease = nil
			leaseMu.Unlock()
			executionMu.Lock()
			executionRef = nil
			executionMu.Unlock()
			if gate != nil {
				gate.Release()
			}
			if reservation.Lease != nil {
				reservation.Lease.Release()
			}
		})
	})
	if err != nil {
		if reservation.Lease != nil {
			reservation.Lease.Release()
		}
		return "", err
	}
	return acceptedID, nil
}

func (l *BatchLogic) runBatchUpdate(ctx context.Context, taskID string, execution *task.Execution, itemIndex, totalItems int, target operation.Target, item task.Item, deleteOld bool) error {
	if l.store == nil {
		return errors.New("private batch snapshot store is unavailable")
	}
	pullProgress := newPullProgressThrottle(time.Second)
	callbacks := containerService.UpdateCallbacks{
		CandidateOwnerID: item.SnapshotRef,
		Progress: func(stage string, percentage int, message string) error {
			if stage == "pulling-image" {
				if !pullProgress.Allow(time.Now()) {
					return nil
				}
				record := execution.Current()
				observability.LogTaskPullProgress(ctx, record.TaskID, record.RequestID, stage, percentage, message)
			}
			batchPercentage := (itemIndex*100 + percentage) / totalItems
			if batchPercentage > 99 {
				batchPercentage = 99
			}
			return execution.Progress(fmt.Sprintf("item-%d-%s", itemIndex+1, stage), batchPercentage, fmt.Sprintf("%s: %s", item.Name, message))
		},
		Warning: execution.Warning,
		SaveSnapshot: func(snapshot []byte) error {
			return l.store.SaveItemSnapshot(taskID, item.SnapshotRef, snapshot)
		},
		Checkpoint: func(checkpoint containerService.UpdateCheckpoint) error {
			mutate := func(record *task.Record) {
				current := &record.Items[itemIndex]
				current.Stage = checkpoint.Stage
				if checkpoint.OldContainerID != "" {
					current.OldContainerID = checkpoint.OldContainerID
					record.OldContainerID = checkpoint.OldContainerID
					record.ContainerIDs = appendTaskIdentity(record.ContainerIDs, checkpoint.OldContainerID)
				}
				if checkpoint.NewContainerID != "" {
					current.NewContainerID = checkpoint.NewContainerID
					record.NewContainerID = checkpoint.NewContainerID
					record.ContainerIDs = appendTaskIdentity(record.ContainerIDs, checkpoint.NewContainerID)
				}
				if checkpoint.BackupName != "" {
					current.BackupContainerName = checkpoint.BackupName
					record.BackupContainerName = checkpoint.BackupName
				}
				if checkpoint.PreviousImageID != "" {
					current.ImageIDs = appendTaskIdentity(current.ImageIDs, checkpoint.PreviousImageID)
					record.ImageIDs = appendTaskIdentity(record.ImageIDs, checkpoint.PreviousImageID)
				}
				if checkpoint.ResolvedImageID != "" {
					current.ImageIDs = appendTaskIdentity(current.ImageIDs, checkpoint.ResolvedImageID)
					record.ImageIDs = appendTaskIdentity(record.ImageIDs, checkpoint.ResolvedImageID)
				}
				if checkpoint.Stage == "stopping-old" || checkpoint.Stage == "old-stopped" || strings.HasPrefix(checkpoint.Stage, "renaming-old") || strings.HasPrefix(checkpoint.Stage, "old-renamed") || strings.HasPrefix(checkpoint.Stage, "creating-new") || strings.HasPrefix(checkpoint.Stage, "created-new") || checkpoint.Stage == "new-started" || checkpoint.Stage == "replacement-committed" {
					current.ProtectedNames = appendTaskIdentity(current.ProtectedNames, target.Name)
				}
			}
			stage := "batch-item-" + checkpoint.Stage
			if checkpoint.Stage == "replacement-committed" {
				return execution.CriticalItemCommit(itemIndex, stage, mutate)
			}
			return execution.Critical(stage, mutate)
		},
		OnCreated: func(newID string) error {
			persistErr := execution.Critical("batch-item-created-new-container", func(record *task.Record) {
				record.Items[itemIndex].NewContainerID = newID
				record.NewContainerID = newID
				record.ContainerIDs = appendTaskIdentity(record.ContainerIDs, target.ID, newID)
				record.Items[itemIndex].ProtectedNames = appendTaskIdentity(record.Items[itemIndex].ProtectedNames, target.Name)
			})
			aliasErr := error(nil)
			if lease := l.service.Operations(); lease != nil {
				aliasErr = lease.AddAlias(taskID, operation.Target{ID: newID, Name: target.Name})
			}
			return errors.Join(persistErr, aliasErr)
		},
		Validation: containerService.UpdateValidationOptions{HealthcheckWait: l.healthcheckWait, RunningObservation: l.runningObservation},
	}
	if err := l.service.UpdateWithOptions(ctx, target.ID, target.Name, item.ImageRef, deleteOld, taskID, callbacks); err != nil {
		return err
	}
	if err := execution.CriticalItemCommit(itemIndex, "batch-item-committed", func(record *task.Record) {
		current := &record.Items[itemIndex]
		current.Stage = "committed"
		current.Recovery = task.Recovery{Status: "not-needed"}
		current.RequiresAttention = false
		current.ProtectedNames = nil
	}); err != nil {
		return err
	}
	return nil
}

func (l *BatchLogic) recoverBatchUpdates(ctx context.Context, taskID string, execution *task.Execution, cause error) (task.Recovery, error) {
	if execution == nil {
		return task.Recovery{Status: "not-needed", Detail: "no batch update callback entered the replacement phase"}, nil
	}
	if l.store == nil {
		return task.Recovery{Status: "unknown", Detail: "batch update recovery context is unavailable"}, errors.New("batch update recovery context is unavailable")
	}
	record := execution.Current()
	statuses := make([]string, 0, len(record.Items))
	var errs []error
	for index := range record.Items {
		item := record.Items[index]
		if item.Status != "failed" && item.Status != "running" {
			continue
		}
		if item.Recovery.Status != "" && item.Recovery.Detail != "item recovery did not complete before the task ended" {
			statuses = append(statuses, item.Recovery.Status)
			continue
		}
		if item.SnapshotRef == "" || !batchUpdateStageNeedsRecovery(item.Stage) {
			status := task.Recovery{Status: "not-needed", Detail: "update item did not enter the replacement phase"}
			if err := execution.RecoveryCheckpoint(ctx, "batch-item-not-needed", func(current *task.Record) {
				current.Items[index].Recovery = status
				current.Items[index].RequiresAttention = false
				current.Items[index].ProtectedNames = nil
			}); err != nil {
				errs = append(errs, err)
			}
			statuses = append(statuses, status.Status)
			continue
		}
		resultRecord, recoverErr := l.recoverBatchUpdateItem(ctx, taskID, execution, index, item, cause)
		attention := recoverErr != nil || resultRecord.Status == "unknown" || resultRecord.Status == "failed"
		checkpointErr := execution.RecoveryCheckpoint(ctx, "batch-item-recovery-result", func(current *task.Record) {
			current.Items[index].Recovery = resultRecord
			current.Items[index].RequiresAttention = attention
			if !attention {
				current.Items[index].ProtectedNames = nil
			}
		})
		statuses = append(statuses, resultRecord.Status)
		if recoverErr != nil || checkpointErr != nil {
			errs = append(errs, errors.Join(recoverErr, checkpointErr))
		}
	}
	if len(errs) > 0 {
		return task.Recovery{Status: "failed", Detail: "one or more batch update items need recovery review"}, errors.Join(errs...)
	}
	for _, status := range statuses {
		if status == "unknown" || status == "failed" {
			return task.Recovery{Status: "unknown", Detail: "one or more batch update items remain uncertain"}, nil
		}
	}
	for _, status := range statuses {
		if status == "restored" {
			return task.Recovery{Status: "restored", Detail: "failed update items were restored where possible"}, nil
		}
	}
	return task.Recovery{Status: "not-needed", Detail: "no batch update item required Docker recovery"}, nil
}

func (l *BatchLogic) recoverBatchUpdateItem(ctx context.Context, taskID string, execution *task.Execution, index int, item task.Item, _ error) (task.Recovery, error) {
	if execution == nil {
		return task.Recovery{Status: "unknown", Detail: "batch update execution is unavailable"}, errors.New("batch update execution is unavailable")
	}
	if l.store == nil {
		return task.Recovery{Status: "unknown", Detail: "batch update recovery context is unavailable"}, errors.New("batch update recovery context is unavailable")
	}
	record := execution.Current()
	if index < 0 || index >= len(record.Items) {
		return task.Recovery{Status: "unknown", Detail: "batch update item index is invalid"}, errors.New("batch update item index is invalid")
	}
	item = record.Items[index]
	if item.Committed {
		return task.Recovery{Status: "not-needed", Detail: "replacement commit was durably recorded; cleanup only"}, nil
	}
	if item.SnapshotRef == "" || !batchUpdateStageNeedsRecovery(item.Stage) {
		return task.Recovery{Status: "not-needed", Detail: "update item did not enter the replacement phase"}, nil
	}
	snapshot, snapshotErr := l.store.ReadItemSnapshot(taskID, item.SnapshotRef)
	if snapshotErr != nil {
		return task.Recovery{Status: "unknown", Detail: "private item snapshot could not be read; no automatic recovery performed"}, snapshotErr
	}
	resolvedImageID := ""
	if len(item.ImageIDs) > 0 {
		resolvedImageID = item.ImageIDs[len(item.ImageIDs)-1]
	}
	recoveryStage := item.Stage
	if recoveryStage == "replacement-committed" {
		// This stage can survive in memory after its commit write fails. The
		// item commit bit is the durable authority; allow pre-commit rollback.
		recoveryStage = "new-started"
	}
	result, recoverErr := l.service.RecoverUpdateWithOwner(ctx, taskID, item.SnapshotRef, snapshot, containerService.UpdateRecoveryState{
		Stage: recoveryStage, OldContainerID: item.OldContainerID, NewContainerID: item.NewContainerID,
		BackupName: item.BackupContainerName, ResolvedImageID: resolvedImageID,
	}, func(checkpoint containerService.UpdateCheckpoint) error {
		return execution.RecoveryCheckpoint(ctx, "batch-item-"+checkpoint.Stage, func(current *task.Record) {
			fact := &current.Items[index]
			fact.Stage = checkpoint.Stage
			if checkpoint.OldContainerID != "" {
				fact.OldContainerID = checkpoint.OldContainerID
				current.ContainerIDs = appendTaskIdentity(current.ContainerIDs, checkpoint.OldContainerID)
			}
			if checkpoint.NewContainerID != "" {
				fact.NewContainerID = checkpoint.NewContainerID
				current.ContainerIDs = appendTaskIdentity(current.ContainerIDs, checkpoint.NewContainerID)
			}
			if checkpoint.BackupName != "" {
				fact.BackupContainerName = checkpoint.BackupName
			}
		})
	})
	return task.Recovery{Status: result.Status, Detail: result.Detail}, recoverErr
}

func batchUpdateStageNeedsRecovery(stage string) bool {
	switch stage {
	case "", "starting", "preflight", "snapshot-saved", "pulling-image", "resolving-image", "image-resolved":
		return false
	default:
		return true
	}
}

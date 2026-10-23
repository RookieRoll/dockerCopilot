package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

type UpdateLogic struct {
	logx.Logger
	ctx                context.Context
	taskRunner         *task.Runner
	taskStore          *task.Store
	service            *containerService.Service
	imageGate          *imageService.AdmissionGate
	mutationGate       *operation.MutationGate
	deleteOldDefault   bool
	healthcheckWait    time.Duration
	runningObservation time.Duration
	optionsErr         error
}

func NewUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateLogic {
	options, optionsErr := svcCtx.Config.TaskOptions()
	return &UpdateLogic{
		Logger:           logx.WithContext(ctx),
		ctx:              ctx,
		taskRunner:       svcCtx.TaskRunner,
		taskStore:        svcCtx.TaskStore,
		service:          serviceFromContext(svcCtx),
		imageGate:        svcCtx.ImageService().AdmissionGate(),
		mutationGate:     svcCtx.MutationGate(),
		deleteOldDefault: svcCtx.Config.ResolveDeleteOldContainer(nil),
		healthcheckWait:  options.HealthcheckWait, runningObservation: options.RunningObservation, optionsErr: optionsErr,
	}
}

type requestError string

func (e requestError) Error() string     { return string(e) }
func (e requestError) HTTPStatus() int   { return http.StatusBadRequest }
func (e requestError) ErrorCode() string { return "INVALID_REQUEST" }

type taskUnavailableError string

func (e taskUnavailableError) Error() string     { return string(e) }
func (e taskUnavailableError) HTTPStatus() int   { return http.StatusServiceUnavailable }
func (e taskUnavailableError) ErrorCode() string { return "TASK_SERVICE_UNAVAILABLE" }

type taskSubmitError struct{ cause error }

func (e taskSubmitError) Error() string { return e.cause.Error() }
func (e taskSubmitError) Unwrap() error { return e.cause }
func (e taskSubmitError) HTTPStatus() int {
	if errors.Is(e.cause, task.ErrQueueFull) {
		return http.StatusConflict
	}
	return http.StatusServiceUnavailable
}
func (e taskSubmitError) ErrorCode() string {
	if errors.Is(e.cause, task.ErrQueueFull) {
		return "TASK_QUEUE_FULL"
	}
	return "TASK_ACCEPTANCE_FAILED"
}

func (l *UpdateLogic) Update(req *types.ContainerUpdateReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	if l.optionsErr != nil {
		return nil, taskUnavailableError("task validation options are unavailable")
	}
	if l.taskRunner == nil {
		return nil, taskUnavailableError("task runner is unavailable")
	}
	if req == nil || req.Id == "" || req.ContainerName == "" || req.ImageNameAndTag == "" {
		return &types.Resp{Code: http.StatusBadRequest, Msg: "container id, name, and image are required", Data: map[string]any{}}, requestError("container id, name, and image are required")
	}
	delOldContainer := l.effectiveDeleteOld(req.DelOldContainer)
	deleteOption := delOldContainer
	imageNameAndTag := req.ImageNameAndTag
	l.imageGate.Lock()
	defer l.imageGate.Unlock()
	taskID := uuid.NewString()
	lease, existingID, reserveErr := l.service.ReserveUpdate(l.ctx, req.Id, taskID, updateEquivalenceKey(req.ContainerName, imageNameAndTag, delOldContainer))
	if reserveErr != nil {
		resp.Code, resp.Msg, resp.Data = 0, reserveErr.Error(), map[string]any{}
		return resp, reserveErr
	}
	if existingID != "" {
		observability.LogTaskEvent(l.ctx, "task.deduplicated", existingID, observability.RequestIDFromContext(l.ctx))
		resp.Code, resp.Msg, resp.Data = http.StatusOK, "success", map[string]string{"taskID": existingID}
		return resp, nil
	}
	if lease == nil {
		return nil, errors.New("container update reservation did not return a lease")
	}
	inspected, inspectErr := l.service.Inspect(l.ctx, req.Id)
	if inspectErr != nil {
		lease.Release()
		return resp, fmt.Errorf("inspect original image before update admission: %w", inspectErr)
	}
	if inspected.ContainerJSONBase == nil || strings.TrimSpace(inspected.Image) == "" {
		lease.Release()
		return resp, errors.New("inspect original image before update admission: Docker returned no immutable image ID")
	}
	var gateMu sync.Mutex
	var gateLease *operation.MutationLease
	var executionMu sync.Mutex
	var executionRef *task.Execution
	var settled sync.Once
	acceptedTaskID, err := l.taskRunner.Submit(task.TaskSpec{
		Record: task.Record{
			Type: "container-update", Name: req.ContainerName, ImageRef: imageNameAndTag,
			OldContainerID: req.Id, ContainerIDs: []string{req.Id}, ImageIDs: []string{inspected.Image},
			TaskID: taskID, RequestID: observability.RequestIDFromContext(l.ctx),
			Options: task.TaskOptions{Image: imageNameAndTag, DeleteOldContainer: &deleteOption, HealthWaitSeconds: durationSeconds(l.healthcheckWait)},
		},
		OnAccepted: func() error {
			gate, acquireErr := l.mutationGate.Acquire(context.Background(), taskID)
			if acquireErr != nil {
				return acquireErr
			}
			if confirmErr := lease.Confirm(); confirmErr != nil {
				gate.Release()
				return confirmErr
			}
			gateMu.Lock()
			gateLease = gate
			gateMu.Unlock()
			return nil
		},
		OnSettled: func() {
			settled.Do(func() {
				gateMu.Lock()
				gate := gateLease
				gateLease = nil
				gateMu.Unlock()
				if gate != nil {
					gate.Release()
				}
				lease.Release()
			})
		},
		Run: func(ctx context.Context, execution *task.Execution) error {
			executionMu.Lock()
			executionRef = execution
			executionMu.Unlock()
			if err := execution.Progress("updating", 0, "容器更新已开始"); err != nil {
				return err
			}
			pullProgress := newPullProgressThrottle(time.Second)
			bridge := updateTaskBridge{
				taskID: execution.TaskID(), ownerID: execution.TaskID(), targetName: req.ContainerName,
				itemIndex: -1, execution: execution, store: l.taskStore, service: l.service,
			}
			err := l.service.UpdateWithOptions(ctx, req.Id, req.ContainerName, imageNameAndTag, delOldContainer, execution.TaskID(), containerService.UpdateCallbacks{
				Progress: func(stage string, percentage int, message string) error {
					if stage == "pulling-image" {
						if !pullProgress.Allow(time.Now()) {
							return nil
						}
						record := execution.Current()
						observability.LogTaskPullProgress(ctx, record.TaskID, record.RequestID, stage, percentage, message)
					}
					return execution.Progress(stage, percentage, message)
				},
				Warning:      execution.Warning,
				SaveSnapshot: bridge.saveSnapshot,
				Checkpoint:   bridge.checkpoint,
				OnCreated:    func(newID string) error { return bridge.created(newID, nil) },
				Validation:   containerService.UpdateValidationOptions{HealthcheckWait: l.healthcheckWait, RunningObservation: l.runningObservation},
			})
			if err != nil {
				return err
			}
			// UpdateWithOptions already published its final progress. The
			// replacement checkpoint is the commit boundary for recovery.
			return nil
		},
		Recover: func(recoveryCtx context.Context, cause error) (task.Recovery, error) {
			executionMu.Lock()
			recoveryExecution := executionRef
			executionMu.Unlock()
			if recoveryExecution == nil {
				return task.Recovery{Status: "unknown", Detail: "更新执行上下文不可用；未执行自动恢复"}, errors.New("task execution unavailable during update recovery")
			}
			return updateTaskBridge{
				taskID: taskID, ownerID: taskID, targetName: req.ContainerName,
				itemIndex: -1, execution: recoveryExecution, store: l.taskStore, service: l.service,
			}.recover(recoveryCtx, cause)
		},
	})
	if err != nil {
		lease.Release()
		resp.Code, resp.Msg, resp.Data = 0, err.Error(), map[string]any{}
		return resp, taskSubmitError{cause: err}
	}
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]string{"taskID": acceptedTaskID}
	return resp, nil
}

func durationSeconds(value time.Duration) int {
	if value <= 0 {
		return 0
	}
	seconds := int(value / time.Second)
	if seconds == 0 {
		return 1
	}
	return seconds
}

func (l *UpdateLogic) effectiveDeleteOld(requestValue *bool) bool {
	if requestValue != nil {
		return *requestValue
	}
	return l.deleteOldDefault
}

func appendTaskIdentity(existing []string, values ...string) []string {
	out := append([]string(nil), existing...)
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		found := false
		for _, current := range out {
			if current == value {
				found = true
				break
			}
		}
		if !found {
			out = append(out, value)
		}
	}
	return out
}

func updateEquivalenceKey(name, image string, deleteOld bool) string {
	encoded, _ := json.Marshal(struct {
		Name      string `json:"name"`
		Image     string `json:"image"`
		DeleteOld bool   `json:"deleteOld"`
	}{Name: strings.TrimLeft(strings.TrimSpace(name), "/"), Image: strings.TrimSpace(image), DeleteOld: deleteOld})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

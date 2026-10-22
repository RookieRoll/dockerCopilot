package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateLogic struct {
	logx.Logger
	ctx              context.Context
	taskRunner       *task.Runner
	service          *containerService.Service
	deleteOldDefault bool
}

func NewUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateLogic {
	return &UpdateLogic{
		Logger:           logx.WithContext(ctx),
		ctx:              ctx,
		taskRunner:       svcCtx.TaskRunner,
		service:          serviceFromContext(svcCtx),
		deleteOldDefault: svcCtx.Config.ResolveDeleteOldContainer(nil),
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
	if l.taskRunner == nil {
		return nil, taskUnavailableError("task runner is unavailable")
	}
	if req == nil || req.Id == "" || req.ContainerName == "" || req.ImageNameAndTag == "" {
		return &types.Resp{Code: http.StatusBadRequest, Msg: "container id, name, and image are required", Data: map[string]any{}}, requestError("container id, name, and image are required")
	}
	delOldContainer := l.deleteOldDefault
	if req.DelOldContainer != nil {
		delOldContainer = *req.DelOldContainer
	}
	deleteOption := delOldContainer
	imageNameAndTag := req.ImageNameAndTag
	taskID := uuid.NewString()
	lease, existingID, reserveErr := l.service.ReserveUpdate(l.ctx, req.Id, taskID, updateEquivalenceKey(req.ContainerName, imageNameAndTag, delOldContainer))
	if reserveErr != nil {
		resp.Code, resp.Msg, resp.Data = 0, reserveErr.Error(), map[string]any{}
		return resp, reserveErr
	}
	if existingID != "" {
		resp.Code, resp.Msg, resp.Data = http.StatusOK, "success", map[string]string{"taskID": existingID}
		return resp, nil
	}
	if lease == nil {
		return nil, errors.New("container update reservation did not return a lease")
	}
	taskID, err = l.taskRunner.Submit(task.TaskSpec{
		Record: task.Record{
			Type: "container-update", Name: req.ContainerName, ImageRef: imageNameAndTag,
			OldContainerID: req.Id, ContainerIDs: []string{req.Id},
			TaskID: taskID, Options: task.TaskOptions{Image: imageNameAndTag, DeleteOldContainer: &deleteOption},
		},
		OnAccepted: lease.Confirm,
		OnSettled:  lease.Release,
		Run: func(ctx context.Context, execution *task.Execution) error {
			if err := execution.Progress("updating", 0, "容器更新已开始"); err != nil {
				return err
			}
			err := l.service.UpdateWithCallbacks(ctx, req.Id, req.ContainerName, imageNameAndTag, delOldContainer, execution.TaskID(), func(newID string) error {
				return execution.Critical("created-new-container", func(record *task.Record) {
					record.NewContainerID = newID
					record.ContainerIDs = appendTaskIdentity(record.ContainerIDs, req.Id, newID)
					record.ProtectedNames = appendTaskIdentity(record.ProtectedNames, req.ContainerName)
				})
			})
			if err != nil {
				return err
			}
			return execution.Progress("completed", 100, "容器更新完成")
		},
	})
	if err != nil {
		lease.Release()
		resp.Code, resp.Msg, resp.Data = 0, err.Error(), map[string]any{}
		return resp, taskSubmitError{cause: err}
	}
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]string{"taskID": taskID}
	return resp, nil
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

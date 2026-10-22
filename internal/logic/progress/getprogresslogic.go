package progress

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetProgressLogic struct {
	logx.Logger
	ctx         context.Context
	getProgress func(string) (svc.TaskProgress, bool)
	taskQuery   *task.QueryService
}

func NewGetProgressLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetProgressLogic {
	return &GetProgressLogic{
		Logger:      logx.WithContext(ctx),
		ctx:         ctx,
		getProgress: svcCtx.ProgressService().Get,
		taskQuery:   svcCtx.TaskQuery,
	}
}

type taskNotFoundError struct{ cause error }

func (e taskNotFoundError) Error() string     { return "taskID 未找到" }
func (e taskNotFoundError) Unwrap() error     { return e.cause }
func (e taskNotFoundError) HTTPStatus() int   { return http.StatusNotFound }
func (e taskNotFoundError) ErrorCode() string { return "TASK_NOT_FOUND" }

func (l *GetProgressLogic) GetProgress(req *types.GetProgressReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	if l.taskQuery != nil {
		record, queryErr := l.taskQuery.GetProgress(req.TaskId)
		if queryErr != nil {
			resp.Code = http.StatusNotFound
			resp.Msg = "taskID 未找到"
			resp.Data = map[string]interface{}{}
			if !errors.Is(queryErr, os.ErrNotExist) {
				return resp, queryErr
			}
			return resp, taskNotFoundError{cause: queryErr}
		}
		resp.Code = http.StatusOK
		resp.Msg = record.Message
		resp.Data = record
		return resp, nil
	}
	progress, exists := l.getProgress(req.TaskId)
	if !exists {
		resp.Code = http.StatusNotFound
		resp.Msg = "taskID 未找到"
		resp.Data = map[string]interface{}{}
		return resp, taskNotFoundError{}
	}
	resp.Code = 200
	resp.Msg = progress.Message
	resp.Data = map[string]interface{}{
		"taskID":     progress.TaskID,
		"percentage": progress.Percentage,
		"message":    progress.Message,
		"name":       progress.Name,
		"detailMsg":  progress.DetailMsg,
		"isDone":     progress.IsDone,
	}
	return resp, nil
}

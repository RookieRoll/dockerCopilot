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
	logic := &GetProgressLogic{
		Logger:    logx.WithContext(ctx),
		ctx:       ctx,
		taskQuery: svcCtx.TaskQuery,
	}
	if logic.taskQuery == nil {
		logic.getProgress = svcCtx.ProgressService().Get
	}
	return logic
}

type taskNotFoundError struct{ cause error }

func (e taskNotFoundError) Error() string {
	return "进度已失效，服务可能已重启；操作结果未知，请核对容器状态和日志"
}
func (e taskNotFoundError) Unwrap() error     { return e.cause }
func (e taskNotFoundError) HTTPStatus() int   { return http.StatusGone }
func (e taskNotFoundError) ErrorCode() string { return "OPERATION_RESULT_EXPIRED" }

func (l *GetProgressLogic) GetProgress(req *types.GetProgressReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	if l.taskQuery != nil {
		record, queryErr := l.taskQuery.GetProgress(req.TaskId)
		if queryErr != nil {
			resp.Code = http.StatusGone
			resp.Msg = taskNotFoundError{}.Error()
			resp.Data = map[string]interface{}{}
			if !errors.Is(queryErr, os.ErrNotExist) {
				resp.Code = 0
				resp.Msg = queryErr.Error()
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
		resp.Code = http.StatusGone
		resp.Msg = taskNotFoundError{}.Error()
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

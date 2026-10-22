package progress

import (
	"context"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetProgressLogic struct {
	logx.Logger
	ctx         context.Context
	getProgress func(string) (svc.TaskProgress, bool)
}

func NewGetProgressLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetProgressLogic {
	return &GetProgressLogic{
		Logger:      logx.WithContext(ctx),
		ctx:         ctx,
		getProgress: svcCtx.ProgressService().Get,
	}
}

func (l *GetProgressLogic) GetProgress(req *types.GetProgressReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	progress, exists := l.getProgress(req.TaskId)
	if !exists {
		resp.Code = 400
		resp.Msg = "taskID 未找到"
		resp.Data = map[string]interface{}{}
		return
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

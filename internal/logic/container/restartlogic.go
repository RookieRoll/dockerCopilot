package container

import (
	"context"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type RestartLogic struct {
	logx.Logger
	ctx     context.Context
	service *containerService.Service
}

func NewRestartLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RestartLogic {
	return &RestartLogic{
		Logger:  logx.WithContext(ctx),
		ctx:     ctx,
		service: serviceFromContext(svcCtx),
	}
}

func (l *RestartLogic) Restart(req *types.IdReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	err = l.service.Restart(l.ctx, req.Id)
	if err != nil {
		resp.Code = 400
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]interface{}{}
	return resp, nil
}

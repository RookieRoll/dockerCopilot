package container

import (
	"context"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type StartLogic struct {
	logx.Logger
	ctx     context.Context
	service *containerService.Service
}

func serviceFromContext(svcCtx *svc.ServiceContext) *containerService.Service {
	if svcCtx == nil {
		return nil
	}
	return svcCtx.ContainerService()
}

func NewStartLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StartLogic {
	return &StartLogic{
		Logger:  logx.WithContext(ctx),
		ctx:     ctx,
		service: serviceFromContext(svcCtx),
	}
}

func (l *StartLogic) Start(req *types.IdReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	err = l.service.Start(l.ctx, req.Id)
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

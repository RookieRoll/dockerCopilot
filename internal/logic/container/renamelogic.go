package container

import (
	"context"
	"strings"

	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type RenameLogic struct {
	logx.Logger
	ctx     context.Context
	service *containerService.Service
	ports   *settings.PortOverridesService
}

func NewRenameLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenameLogic {
	return &RenameLogic{
		Logger:  logx.WithContext(ctx),
		ctx:     ctx,
		service: serviceFromContext(svcCtx),
		ports:   portServiceFromContext(svcCtx),
	}
}

func (l *RenameLogic) Rename(req *types.ContainerRenameReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	containerInspect, err := l.service.Inspect(l.ctx, req.Id)
	if err != nil {
		resp.Code = 400
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	oldName := strings.TrimPrefix(containerInspect.Name, "/")

	err = l.service.Rename(l.ctx, req.Id, req.NewName)
	if err != nil {
		resp.Code = 400
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	if err := l.ports.Migrate(oldName, req.NewName); err != nil {
		l.Errorf("migrate configured ports after renaming container %q: %v", oldName, err)
	}
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]interface{}{}
	return resp, nil
}

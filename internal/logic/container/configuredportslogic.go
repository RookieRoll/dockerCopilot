package container

import (
	"context"
	"fmt"
	"strings"

	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type ConfiguredPortsLogic struct {
	logx.Logger
	ctx        context.Context
	containers *containerService.Service
	ports      *settings.PortOverridesService
}

func NewConfiguredPortsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ConfiguredPortsLogic {
	return &ConfiguredPortsLogic{
		Logger:     logx.WithContext(ctx),
		ctx:        ctx,
		containers: serviceFromContext(svcCtx),
		ports:      portServiceFromContext(svcCtx),
	}
}

func (l *ConfiguredPortsLogic) Save(req *types.ContainerConfiguredPortsReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	containerInspect, err := l.containers.Inspect(l.ctx, req.Id)
	if err != nil {
		resp.Code = 404
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	if containerInspect.HostConfig == nil || string(containerInspect.HostConfig.NetworkMode) != "host" {
		err = fmt.Errorf("configured ports are only supported for host network containers")
		resp.Code = 400
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}

	containerName := strings.TrimPrefix(containerInspect.Name, "/")
	if err := l.ports.Save(containerName, req.Ports); err != nil {
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

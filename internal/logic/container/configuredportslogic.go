package container

import (
	"context"
	"fmt"
	"strings"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/onlyLTY/dockerCopilot/internal/utiles"
	"github.com/zeromicro/go-zero/core/logx"
)

type ConfiguredPortsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewConfiguredPortsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ConfiguredPortsLogic {
	return &ConfiguredPortsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ConfiguredPortsLogic) Save(req *types.ContainerConfiguredPortsReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	containerInspect, err := utiles.GetContainerInspect(l.svcCtx, req.Id)
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
	if err := utiles.SaveContainerPortOverrides(containerName, req.Ports); err != nil {
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

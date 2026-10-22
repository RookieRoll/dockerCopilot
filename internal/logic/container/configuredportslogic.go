package container

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
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
	containerInspect, lease, err := l.containers.ReserveContainerMutation(l.ctx, req.Id, "container-configured-ports")
	if err != nil {
		resp.Code = http.StatusInternalServerError
		var classified observability.ClassifiedError
		if errors.As(err, &classified) {
			resp.Code = classified.HTTPStatus()
		} else if cerrdefs.IsNotFound(err) {
			resp.Code = http.StatusNotFound
		}
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	defer lease.Release()
	if containerInspect.HostConfig == nil || string(containerInspect.HostConfig.NetworkMode) != "host" {
		err = observability.BadRequest(fmt.Errorf("configured ports are only supported for host network containers"))
		resp.Code = http.StatusBadRequest
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}

	containerName := strings.TrimPrefix(containerInspect.Name, "/")
	normalizedPorts, err := settings.NormalizeConfiguredPorts(req.Ports)
	if err != nil {
		err = observability.BadRequest(err)
		resp.Code = http.StatusBadRequest
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	if err := l.ports.Save(containerName, normalizedPorts); err != nil {
		resp.Code = http.StatusInternalServerError
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}

	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]interface{}{}
	return resp, nil
}

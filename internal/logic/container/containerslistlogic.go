package container

import (
	"context"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/go-connections/nat"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type ContainersListLogic struct {
	logx.Logger
	ctx     context.Context
	service *containerService.Service
	ports   *settings.PortOverridesService
}

type PortInfo = containerService.PortInfo
type Info = containerService.Info

func deduplicateAndSortPorts(ports []PortInfo) []PortInfo {
	return containerService.DeduplicateAndSortPorts(ports)
}
func mapContainerPorts(ports []dockerTypes.Port) []PortInfo {
	return containerService.MapContainerPorts(ports)
}
func mapHostNetworkPorts(ports nat.PortSet) []PortInfo {
	return containerService.MapHostNetworkPorts(ports)
}

func NewContainersListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ContainersListLogic {
	return &ContainersListLogic{Logger: logx.WithContext(ctx), ctx: ctx, service: serviceFromContext(svcCtx), ports: portServiceFromContext(svcCtx)}
}

func (l *ContainersListLogic) ContainersList() (*types.Resp, error) {
	items, err := l.service.ListDetails(l.ctx, l.ports)
	if err != nil {
		return &types.Resp{Code: 500, Msg: err.Error(), Data: map[string]interface{}{}}, err
	}
	return &types.Resp{Code: 200, Msg: "success", Data: items}, nil
}

package utiles

import (
	"context"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	MyType "github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

func GetContainerList(ctx *svc.ServiceContext) ([]MyType.Container, error) {
	// 获取所有容器（包括停止的容器）
	service := ctx.Containers
	if service == nil {
		service = containerService.NewService(ctx.DockerClient, ctx.HubImageInfo)
	}
	dockerContainerList, err := service.List(context.Background())
	if err != nil {
		logx.Errorf("get container list error: %v", err)
		return nil, err
	}
	return dockerContainerList, nil
}

func CheckImageUpdate(ctx *svc.ServiceContext, containerListData []MyType.Container) []MyType.Container {
	service := ctx.Containers
	if service == nil {
		service = containerService.NewService(ctx.DockerClient, ctx.HubImageInfo)
	}
	return service.CheckImageUpdates(containerListData)
}

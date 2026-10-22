package utiles

import (
	"context"
	"github.com/docker/docker/api/types"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func GetContainerInspect(ctx *svc.ServiceContext, id string) (types.ContainerJSON, error) {
	service := ctx.Containers
	if service == nil {
		service = containerService.NewService(ctx.DockerClient)
	}
	return service.Inspect(context.TODO(), id)
}

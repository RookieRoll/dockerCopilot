package utiles

import (
	"context"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func StopContainer(ctx *svc.ServiceContext, id string) error {
	service := ctx.Containers
	if service == nil {
		service = containerService.NewService(ctx.DockerClient)
	}
	return service.Stop(context.Background(), id)
}

package utiles

import (
	"context"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func StartContainer(ctx *svc.ServiceContext, id string) error {
	service := ctx.Containers
	if service == nil {
		service = containerService.NewService(ctx.DockerClient)
	}
	return service.Start(context.Background(), id)
}

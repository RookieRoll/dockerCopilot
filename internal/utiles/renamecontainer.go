package utiles

import (
	"context"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func RenameContainer(ctx *svc.ServiceContext, id string, newName string) error {
	service := ctx.Containers
	if service == nil {
		service = containerService.NewService(ctx.DockerClient)
	}
	return service.Rename(context.TODO(), id, newName)
}

package utiles

import (
	"context"
	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func RemoveImage(ctx *svc.ServiceContext, imageID string, force bool) error {
	service := ctx.Images
	if service == nil {
		service = imageService.NewService(ctx.DockerClient)
	}
	return service.Remove(context.Background(), imageID, force)
}

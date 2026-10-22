package utiles

import (
	"context"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

// UpdateContainer is a compatibility adapter for callers of the original
// utility API. Production logic uses the container service directly.
func UpdateContainer(ctx *svc.ServiceContext, id, name, imageNameAndTag string, deleteOld bool, taskID string) error {
	service := ctx.Containers
	if service == nil {
		service = containerService.NewService(ctx.DockerClient, ctx.HubImageInfo)
		progress := ctx.ProgressService()
		service.SetProgressCallbacks(containerService.ProgressCallbacks{Update: progress.Update, Get: progress.Get})
	}
	return service.Update(context.Background(), id, name, imageNameAndTag, deleteOld, taskID)
}

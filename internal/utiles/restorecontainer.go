package utiles

import (
	"context"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

// RestoreContainer is the compatibility adapter for older utility callers.
func RestoreContainer(ctx *svc.ServiceContext, filename, taskID string) error {
	service := ctx.Backups
	if service == nil {
		service = backupService.NewService(ctx.DockerClient, ctx.Config.BackupDirectory())
		progress := ctx.ProgressService()
		service.SetProgressCallbacks(backupService.ProgressCallbacks{Update: progress.Update, Get: progress.Get})
	}
	return service.Restore(context.Background(), filename, taskID)
}

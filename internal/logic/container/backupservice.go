package container

import (
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func backupServiceFromContext(ctx *svc.ServiceContext) *backupService.Service {
	if ctx.Backups != nil {
		return ctx.Backups
	}
	service := backupService.NewService(ctx.DockerClient, ctx.Config.BackupDirectory())
	progress := ctx.ProgressService()
	service.SetProgressCallbacks(backupService.ProgressCallbacks{Update: progress.Update, Get: progress.Get})
	return service
}

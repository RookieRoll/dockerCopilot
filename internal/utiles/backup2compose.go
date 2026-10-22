package utiles

import (
	"context"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func Backup2Compose(ctx *svc.ServiceContext) (err error) {
	service := ctx.Backups
	if service == nil {
		service = backupService.NewService(ctx.DockerClient, ctx.Config.BackupDirectory())
	}
	return service.ExportCompose(context.Background())
}

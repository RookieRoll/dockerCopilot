package utiles

import (
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func BackupList(ctx *svc.ServiceContext) ([]string, error) {
	dir := BackupDir()
	if ctx != nil {
		dir = ctx.Config.BackupDirectory()
	}
	if ctx != nil && ctx.Backups != nil {
		return ctx.Backups.List()
	}
	return backupService.NewService(nil, dir).List()
}

func BackupListIn(dir string) ([]string, error) {
	return backupService.NewService(nil, dir).List()
}

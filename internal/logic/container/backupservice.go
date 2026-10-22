package container

import (
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func backupServiceFromContext(ctx *svc.ServiceContext) *backupService.Service {
	if ctx == nil {
		return nil
	}
	return ctx.BackupService()
}

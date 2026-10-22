package container

import (
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func portServiceFromContext(ctx *svc.ServiceContext) *settings.PortOverridesService {
	if ctx.Ports != nil {
		return ctx.Ports
	}
	return settings.NewPortOverridesService(ctx.Config.PortOverridesFilePath())
}

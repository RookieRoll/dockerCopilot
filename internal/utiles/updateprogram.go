package utiles

import (
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
)

// UpdateProgram is the compatibility adapter for callers that still pass the
// legacy service context. New logic supplies a resolved value source.
func UpdateProgram(ctx *svc.ServiceContext) error {
	source := config.LegacyUpdateSource()
	if ctx != nil && ctx.Config.Updates.Repository != "" {
		source = ctx.Config.UpdateSource()
	}
	return updater.UpdateProgramWithSource(source)
}

func UpdateProgramWithSource(source config.UpdateConfig) error {
	return updater.UpdateProgramWithSource(source)
}

func downloadFile(url, destination string) error { return updater.DownloadFile(url, destination) }
func decompressTarGz(archive, destination string) error {
	return updater.DecompressTarGz(archive, destination)
}

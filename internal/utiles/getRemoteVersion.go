package utiles

import (
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
)

func GetRemoteVersion() (remoteVersion string, err error) {
	return updater.GetRemoteVersion()
}

func GetRemoteVersionWithConfig(source config.UpdateConfig) (remoteVersion string, err error) {
	return updater.GetRemoteVersionWithConfig(source)
}

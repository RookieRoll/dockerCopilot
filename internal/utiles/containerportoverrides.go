package utiles

import (
	"path/filepath"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

const (
	containerPortOverridesFileName = "container-port-overrides.json"
	maxConfiguredPorts             = 32 // retained for compatibility tests
)

// These variables remain only for callers/tests of the legacy package API.
var (
	timeNow                    = time.Now
	containerPortOverridesPath = filepath.Join("/data", "config", containerPortOverridesFileName)
)

func NormalizeConfiguredPorts(ports []types.ConfiguredPort) ([]types.ConfiguredPort, error) {
	return settings.NormalizeConfiguredPorts(ports)
}

func configuredPortOverridesFilePath() string {
	return config.LegacyPortOverridesPath(containerPortOverridesPath)
}
func LoadContainerPortOverrides() (map[string][]types.ConfiguredPort, error) {
	return settings.LoadContainerPortOverridesAt(configuredPortOverridesFilePath())
}
func LoadContainerPortOverridesAt(path string) (map[string][]types.ConfiguredPort, error) {
	return settings.LoadContainerPortOverridesAt(path)
}
func RemoveContainerPortOverrides(name string) error {
	return settings.RemoveContainerPortOverridesAt(configuredPortOverridesFilePath(), name)
}
func RemoveContainerPortOverridesAt(path, name string) error {
	return settings.RemoveContainerPortOverridesAt(path, name)
}
func CleanupContainerPortOverrides(names []string) (int, map[string][]types.ConfiguredPort, error) {
	return settings.CleanupContainerPortOverridesAt(configuredPortOverridesFilePath(), names)
}
func CleanupContainerPortOverridesAt(path string, names []string) (int, map[string][]types.ConfiguredPort, error) {
	return settings.CleanupContainerPortOverridesAt(path, names)
}
func SaveContainerPortOverrides(name string, ports []types.ConfiguredPort) error {
	return SaveContainerPortOverridesAt(configuredPortOverridesFilePath(), name, ports)
}
func SaveContainerPortOverridesAt(path, name string, ports []types.ConfiguredPort) error {
	return settings.SaveContainerPortOverridesAtTimestamp(path, name, ports, timeNow().UTC().Format(time.RFC3339))
}
func MigrateContainerPortOverrides(oldName, newName string) error {
	return settings.MigrateContainerPortOverridesAt(configuredPortOverridesFilePath(), oldName, newName)
}
func MigrateContainerPortOverridesAt(path, oldName, newName string) error {
	return settings.MigrateContainerPortOverridesAt(path, oldName, newName)
}

func nowRFC3339() string { return timeNow().UTC().Format(time.RFC3339) }

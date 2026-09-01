package utiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/onlyLTY/dockerCopilot/internal/types"
)

const (
	containerPortOverridesFileName = "container-port-overrides.json"
	containerPortOverridesPathEnv  = "CONTAINER_PORT_OVERRIDES_PATH"
	maxConfiguredPorts             = 32
	maxConfiguredPortLabelRunes    = 64
)

var (
	timeNow = time.Now

	containerPortOverridesPath = filepath.Join("/data", "config", containerPortOverridesFileName)
	containerPortOverridesMu   sync.Mutex
)

type containerPortOverridesFile struct {
	SchemaVersion int                                        `json:"schemaVersion"`
	Containers    map[string]containerPortOverridesContainer `json:"containers"`
}

type containerPortOverridesContainer struct {
	UpdatedAt string                 `json:"updatedAt"`
	Ports     []types.ConfiguredPort `json:"ports"`
}

func NormalizeConfiguredPorts(ports []types.ConfiguredPort) ([]types.ConfiguredPort, error) {
	if len(ports) > maxConfiguredPorts {
		return nil, fmt.Errorf("a container can have at most %d configured ports", maxConfiguredPorts)
	}

	normalized := make([]types.ConfiguredPort, 0, len(ports))
	seen := make(map[string]struct{}, len(ports))
	for _, port := range ports {
		if port.Port < 1 || port.Port > 65535 {
			return nil, fmt.Errorf("configured port must be between 1 and 65535")
		}

		protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
		if protocol != "tcp" && protocol != "udp" {
			return nil, fmt.Errorf("configured port protocol must be tcp or udp")
		}

		label := strings.TrimSpace(port.Label)
		if utf8.RuneCountInString(label) > maxConfiguredPortLabelRunes {
			return nil, fmt.Errorf("configured port label must be at most %d characters", maxConfiguredPortLabelRunes)
		}

		key := fmt.Sprintf("%d/%s", port.Port, protocol)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("configured ports must not contain duplicates")
		}
		seen[key] = struct{}{}
		normalized = append(normalized, types.ConfiguredPort{
			Port:     port.Port,
			Protocol: protocol,
			Label:    label,
		})
	}

	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Port != normalized[j].Port {
			return normalized[i].Port < normalized[j].Port
		}
		return normalized[i].Protocol < normalized[j].Protocol
	})
	return normalized, nil
}

func configuredPortOverridesFilePath() string {
	if configuredPath := strings.TrimSpace(os.Getenv(containerPortOverridesPathEnv)); configuredPath != "" {
		return configuredPath
	}
	return containerPortOverridesPath
}

func LoadContainerPortOverrides() (map[string][]types.ConfiguredPort, error) {
	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()
	return loadContainerPortOverridesLocked()
}

// RemoveContainerPortOverrides removes the saved ports for one container.
// It is safe to call when the container has no saved configuration.
func RemoveContainerPortOverrides(containerName string) error {
	containerName = normalizeContainerName(containerName)
	if containerName == "" {
		return nil
	}

	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()

	config, err := readContainerPortOverridesFileLocked()
	if err != nil {
		return err
	}
	if _, found := config.Containers[containerName]; !found {
		return nil
	}
	delete(config.Containers, containerName)
	return writeContainerPortOverridesFileLocked(config)
}

// CleanupContainerPortOverrides removes entries whose container names are not
// present in the latest successful Docker container list. The returned count
// is the number of removed stale entries.
func CleanupContainerPortOverrides(activeContainerNames []string) (int, error) {
	activeNames := make(map[string]struct{}, len(activeContainerNames))
	for _, name := range activeContainerNames {
		if normalizedName := normalizeContainerName(name); normalizedName != "" {
			activeNames[normalizedName] = struct{}{}
		}
	}

	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()

	config, err := readContainerPortOverridesFileLocked()
	if err != nil {
		return 0, err
	}
	removed := 0
	for name := range config.Containers {
		if _, found := activeNames[normalizeContainerName(name)]; found {
			continue
		}
		delete(config.Containers, name)
		removed++
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, writeContainerPortOverridesFileLocked(config)
}

func SaveContainerPortOverrides(containerName string, ports []types.ConfiguredPort) error {
	containerName = normalizeContainerName(containerName)
	if containerName == "" {
		return fmt.Errorf("container name is required")
	}

	normalized, err := NormalizeConfiguredPorts(ports)
	if err != nil {
		return err
	}

	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()

	config, err := readContainerPortOverridesFileLocked()
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		delete(config.Containers, containerName)
	} else {
		config.Containers[containerName] = containerPortOverridesContainer{
			UpdatedAt: nowRFC3339(),
			Ports:     normalized,
		}
	}
	return writeContainerPortOverridesFileLocked(config)
}

func MigrateContainerPortOverrides(oldName, newName string) error {
	oldName = normalizeContainerName(oldName)
	newName = normalizeContainerName(newName)
	if oldName == "" || newName == "" || oldName == newName {
		return nil
	}

	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()

	config, err := readContainerPortOverridesFileLocked()
	if err != nil {
		return err
	}
	entry, found := config.Containers[oldName]
	if !found {
		return nil
	}
	delete(config.Containers, oldName)
	entry.UpdatedAt = nowRFC3339()
	config.Containers[newName] = entry
	return writeContainerPortOverridesFileLocked(config)
}

func normalizeContainerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func loadContainerPortOverridesLocked() (map[string][]types.ConfiguredPort, error) {
	config, err := readContainerPortOverridesFileLocked()
	if err != nil {
		return nil, err
	}
	overrides := make(map[string][]types.ConfiguredPort, len(config.Containers))
	for name, entry := range config.Containers {
		ports, err := NormalizeConfiguredPorts(entry.Ports)
		if err != nil {
			return nil, fmt.Errorf("invalid saved ports for container %q: %w", name, err)
		}
		overrides[name] = ports
	}
	return overrides, nil
}

func readContainerPortOverridesFileLocked() (containerPortOverridesFile, error) {
	config := containerPortOverridesFile{
		SchemaVersion: 1,
		Containers:    make(map[string]containerPortOverridesContainer),
	}
	content, err := os.ReadFile(configuredPortOverridesFilePath())
	if os.IsNotExist(err) {
		return config, nil
	}
	if err != nil {
		return containerPortOverridesFile{}, err
	}
	if err := json.Unmarshal(content, &config); err != nil {
		return containerPortOverridesFile{}, fmt.Errorf("read configured ports: %w", err)
	}
	if config.SchemaVersion != 1 {
		return containerPortOverridesFile{}, fmt.Errorf("unsupported configured port schema version %d", config.SchemaVersion)
	}
	if config.Containers == nil {
		config.Containers = make(map[string]containerPortOverridesContainer)
	}
	return config, nil
}

func writeContainerPortOverridesFileLocked(config containerPortOverridesFile) error {
	path := configuredPortOverridesFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	config.SchemaVersion = 1
	if config.Containers == nil {
		config.Containers = make(map[string]containerPortOverridesContainer)
	}

	content, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')

	temporaryFile, err := os.CreateTemp(filepath.Dir(path), ".container-port-overrides-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporaryFile.Name()
	defer os.Remove(temporaryPath)

	if err := temporaryFile.Chmod(0o644); err != nil {
		temporaryFile.Close()
		return err
	}
	if _, err := temporaryFile.Write(content); err != nil {
		temporaryFile.Close()
		return err
	}
	if err := temporaryFile.Sync(); err != nil {
		temporaryFile.Close()
		return err
	}
	if err := temporaryFile.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func nowRFC3339() string {
	return timeNow().Format(time.RFC3339)
}

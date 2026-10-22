package settings

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

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

const (
	containerPortOverridesFileName = "container-port-overrides.json"
	maxConfiguredPorts             = 32
	maxConfiguredPortLabelRunes    = 64
)

var (
	timeNow = time.Now

	containerPortOverridesPath       = filepath.Join("/data", "config", containerPortOverridesFileName)
	containerPortOverridesMu         sync.Mutex
	containerPortOverridesGeneration = make(map[string]uint64)
)

// PortOverridesService scopes persisted host-network port settings to the
// configured file path. The legacy package-level functions remain for callers
// that have not yet been migrated.
type PortOverridesService struct {
	path      string
	writeFile func(string, containerPortOverridesFile) error
}

func NewPortOverridesService(path string) *PortOverridesService {
	return &PortOverridesService{path: path, writeFile: writeContainerPortOverridesFileLocked}
}
func (s *PortOverridesService) Load() (map[string][]types.ConfiguredPort, error) {
	return LoadContainerPortOverridesAt(s.path)
}
func (s *PortOverridesService) Save(name string, ports []types.ConfiguredPort) error {
	return SaveContainerPortOverridesAt(s.path, name, ports)
}
func (s *PortOverridesService) Remove(name string) error {
	return RemoveContainerPortOverridesAt(s.path, name)
}
func (s *PortOverridesService) Cleanup(names []string) (int, map[string][]types.ConfiguredPort, error) {
	return CleanupContainerPortOverridesAt(s.path, names)
}
func (s *PortOverridesService) CleanupProtected(names, protected []string) (int, map[string][]types.ConfiguredPort, error) {
	return CleanupContainerPortOverridesAtProtected(s.path, names, protected)
}
func (s *PortOverridesService) Generation() uint64 {
	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()
	return containerPortOverridesGeneration[portOverridesPathKey(s.path)]
}
func (s *PortOverridesService) CleanupStable(names, protected []string, expected uint64) (int, map[string][]types.ConfiguredPort, bool, error) {
	return CleanupContainerPortOverridesAtStable(s.path, names, protected, expected)
}
func (s *PortOverridesService) CleanupStableWithContainers(containers []ContainerPortIdentity, protected []string, expected uint64) (int, map[string][]types.ConfiguredPort, bool, error) {
	return CleanupContainerPortOverridesAtStableWithContainers(s.path, containers, protected, expected)
}
func (s *PortOverridesService) Migrate(oldName, newName string) error {
	return MigrateContainerPortOverridesAt(s.path, oldName, newName)
}
func (s *PortOverridesService) BeginRename(oldName, newName, containerID string) error {
	return beginContainerPortRenameAt(s.path, oldName, newName, containerID, s.writeFile)
}
func (s *PortOverridesService) CompleteRename(oldName, newName string) error {
	return completeContainerPortRenameAt(s.path, oldName, newName, s.writeFile)
}

type containerPortOverridesFile struct {
	SchemaVersion int                                        `json:"schemaVersion"`
	Containers    map[string]containerPortOverridesContainer `json:"containers"`
	RenameIntents map[string]ContainerPortRenameIntent       `json:"renameIntents,omitempty"`
}

// ContainerPortRenameIntent is a durable marker that protects both names
// until a later successful Docker list can resolve an interrupted rename.
type ContainerPortRenameIntent struct {
	NewName     string `json:"newName"`
	ContainerID string `json:"containerID,omitempty"`
	StartedAt   string `json:"startedAt"`
}

type ContainerPortIdentity struct {
	ID   string
	Name string
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
	return config.LegacyPortOverridesPath(containerPortOverridesPath)
}

func LoadContainerPortOverrides() (map[string][]types.ConfiguredPort, error) {
	return LoadContainerPortOverridesAt(configuredPortOverridesFilePath())
}

func LoadContainerPortOverridesAt(path string) (map[string][]types.ConfiguredPort, error) {
	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()
	return loadContainerPortOverridesLocked(path)
}

// RemoveContainerPortOverrides removes the saved ports for one container.
// It is safe to call when the container has no saved configuration.
func RemoveContainerPortOverrides(containerName string) error {
	return RemoveContainerPortOverridesAt(configuredPortOverridesFilePath(), containerName)
}

func RemoveContainerPortOverridesAt(path, containerName string) error {
	containerName = normalizeContainerName(containerName)
	if containerName == "" {
		return nil
	}

	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()

	config, err := readContainerPortOverridesFileLocked(path)
	if err != nil {
		return err
	}
	if _, found := config.Containers[containerName]; !found {
		return nil
	}
	delete(config.Containers, containerName)
	if err := writeContainerPortOverridesFileLocked(path, config); err != nil {
		return err
	}
	incrementPortOverridesGenerationLocked(path)
	return nil
}

// CleanupContainerPortOverrides removes entries whose container names are not
// present in the latest successful Docker container list and returns the
// cleaned, normalized overrides from the same single file read.
func CleanupContainerPortOverrides(activeContainerNames []string) (int, map[string][]types.ConfiguredPort, error) {
	return CleanupContainerPortOverridesAt(configuredPortOverridesFilePath(), activeContainerNames)
}

func CleanupContainerPortOverridesAt(path string, activeContainerNames []string) (int, map[string][]types.ConfiguredPort, error) {
	return cleanupContainerPortOverridesAt(path, activeContainerNames, nil)
}

func CleanupContainerPortOverridesAtProtected(path string, activeContainerNames, protectedNames []string) (int, map[string][]types.ConfiguredPort, error) {
	return cleanupContainerPortOverridesAt(path, activeContainerNames, protectedNames)
}

// CleanupContainerPortOverridesAtStable atomically checks the port-file
// generation and removes only entries absent from both Docker and durable
// operation protection. A generation mismatch means the caller's list
// snapshot is stale and must be retried.
func CleanupContainerPortOverridesAtStable(path string, activeContainerNames, protectedNames []string, expectedGeneration uint64) (int, map[string][]types.ConfiguredPort, bool, error) {
	return cleanupContainerPortOverridesAtStable(path, activeContainerNames, nil, protectedNames, expectedGeneration)
}

func CleanupContainerPortOverridesAtStableWithContainers(path string, containers []ContainerPortIdentity, protectedNames []string, expectedGeneration uint64) (int, map[string][]types.ConfiguredPort, bool, error) {
	activeNames := make([]string, 0, len(containers))
	for _, container := range containers {
		activeNames = append(activeNames, container.Name)
	}
	return cleanupContainerPortOverridesAtStable(path, activeNames, containers, protectedNames, expectedGeneration)
}

func cleanupContainerPortOverridesAtStable(path string, activeContainerNames []string, identities []ContainerPortIdentity, protectedNames []string, expectedGeneration uint64) (int, map[string][]types.ConfiguredPort, bool, error) {
	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()
	if containerPortOverridesGeneration[portOverridesPathKey(path)] != expectedGeneration {
		return 0, nil, false, nil
	}
	removed, overrides, err := cleanupContainerPortOverridesLockedWithIdentity(path, activeContainerNames, identities, protectedNames)
	return removed, overrides, true, err
}

func cleanupContainerPortOverridesAt(path string, activeContainerNames, protectedNames []string) (int, map[string][]types.ConfiguredPort, error) {
	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()
	return cleanupContainerPortOverridesLockedWithIdentity(path, activeContainerNames, nil, protectedNames)
}

func cleanupContainerPortOverridesLockedWithIdentity(path string, activeContainerNames []string, identities []ContainerPortIdentity, protectedNames []string) (int, map[string][]types.ConfiguredPort, error) {
	config, err := readContainerPortOverridesFileLocked(path)
	if err != nil {
		return 0, nil, err
	}
	activeSet := make(map[string]struct{}, len(activeContainerNames))
	for _, name := range activeContainerNames {
		if normalized := normalizeContainerName(name); normalized != "" {
			activeSet[normalized] = struct{}{}
		}
	}
	intentChanged, pendingNames := reconcilePortRenameIntents(&config, activeSet, identities)
	activeNames := make(map[string]struct{}, len(activeContainerNames))
	for _, name := range append(append(append([]string(nil), activeContainerNames...), protectedNames...), pendingNames...) {
		if normalizedName := normalizeContainerName(name); normalizedName != "" {
			activeNames[normalizedName] = struct{}{}
		}
	}
	removed := 0
	for name := range config.Containers {
		if _, found := activeNames[normalizeContainerName(name)]; found {
			continue
		}
		delete(config.Containers, name)
		removed++
	}
	if removed > 0 || intentChanged {
		if err := writeContainerPortOverridesFileLocked(path, config); err != nil {
			return removed, nil, err
		}
		incrementPortOverridesGenerationLocked(path)
	}

	overrides := make(map[string][]types.ConfiguredPort, len(config.Containers))
	for name, entry := range config.Containers {
		ports, err := NormalizeConfiguredPorts(entry.Ports)
		if err != nil {
			return removed, nil, fmt.Errorf("invalid saved ports for container %q: %w", name, err)
		}
		overrides[name] = ports
	}
	return removed, overrides, nil
}

func SaveContainerPortOverrides(containerName string, ports []types.ConfiguredPort) error {
	return SaveContainerPortOverridesAt(configuredPortOverridesFilePath(), containerName, ports)
}

func SaveContainerPortOverridesAt(path, containerName string, ports []types.ConfiguredPort) error {
	return SaveContainerPortOverridesAtTimestamp(path, containerName, ports, nowRFC3339())
}

func SaveContainerPortOverridesAtTimestamp(path, containerName string, ports []types.ConfiguredPort, updatedAt string) error {
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

	config, err := readContainerPortOverridesFileLocked(path)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		delete(config.Containers, containerName)
	} else {
		config.Containers[containerName] = containerPortOverridesContainer{
			UpdatedAt: updatedAt,
			Ports:     normalized,
		}
	}
	if err := writeContainerPortOverridesFileLocked(path, config); err != nil {
		return err
	}
	incrementPortOverridesGenerationLocked(path)
	return nil
}

func MigrateContainerPortOverrides(oldName, newName string) error {
	return MigrateContainerPortOverridesAt(configuredPortOverridesFilePath(), oldName, newName)
}

func MigrateContainerPortOverridesAt(path, oldName, newName string) error {
	oldName = normalizeContainerName(oldName)
	newName = normalizeContainerName(newName)
	if oldName == "" || newName == "" || oldName == newName {
		return nil
	}

	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()

	config, err := readContainerPortOverridesFileLocked(path)
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
	if err := writeContainerPortOverridesFileLocked(path, config); err != nil {
		return err
	}
	incrementPortOverridesGenerationLocked(path)
	return nil
}

// BeginContainerPortRenameAt persists a marker before Docker is renamed. The
// old mapping remains untouched until CompleteContainerPortRenameAt succeeds.
func BeginContainerPortRenameAt(path, oldName, newName, containerID string) error {
	return beginContainerPortRenameAt(path, oldName, newName, containerID, writeContainerPortOverridesFileLocked)
}

func beginContainerPortRenameAt(path, oldName, newName, containerID string, writeFile func(string, containerPortOverridesFile) error) error {
	if writeFile == nil {
		writeFile = writeContainerPortOverridesFileLocked
	}
	oldName = normalizeContainerName(oldName)
	newName = normalizeContainerName(newName)
	containerID = strings.TrimSpace(containerID)
	if oldName == "" || newName == "" || containerID == "" {
		return fmt.Errorf("old name, new name, and canonical container ID are required")
	}
	if oldName == newName {
		return nil
	}
	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()
	config, err := readContainerPortOverridesFileLocked(path)
	if err != nil {
		return err
	}
	if config.RenameIntents == nil {
		config.RenameIntents = make(map[string]ContainerPortRenameIntent)
	}
	if current, exists := config.RenameIntents[oldName]; exists {
		if current.NewName != newName || (current.ContainerID != "" && containerID != "" && current.ContainerID != containerID) {
			return fmt.Errorf("a different port configuration rename is still pending for %q", oldName)
		}
		return nil
	}
	config.RenameIntents[oldName] = ContainerPortRenameIntent{NewName: newName, ContainerID: containerID, StartedAt: nowRFC3339()}
	if err := writeFile(path, config); err != nil {
		return err
	}
	incrementPortOverridesGenerationLocked(path)
	return nil
}

// CompleteContainerPortRenameAt migrates the old entry and clears its durable
// intent in a single atomic file replacement.
func CompleteContainerPortRenameAt(path, oldName, newName string) error {
	return completeContainerPortRenameAt(path, oldName, newName, writeContainerPortOverridesFileLocked)
}

func completeContainerPortRenameAt(path, oldName, newName string, writeFile func(string, containerPortOverridesFile) error) error {
	if writeFile == nil {
		writeFile = writeContainerPortOverridesFileLocked
	}
	oldName = normalizeContainerName(oldName)
	newName = normalizeContainerName(newName)
	if oldName == "" || newName == "" || oldName == newName {
		return nil
	}
	containerPortOverridesMu.Lock()
	defer containerPortOverridesMu.Unlock()
	config, err := readContainerPortOverridesFileLocked(path)
	if err != nil {
		return err
	}
	intent, exists := config.RenameIntents[oldName]
	if !exists || intent.NewName != newName {
		return fmt.Errorf("matching port configuration rename intent is missing")
	}
	oldEntry, hasOld := config.Containers[oldName]
	if _, hasNew := config.Containers[newName]; hasOld && hasNew {
		return fmt.Errorf("port configuration already exists for the new container name")
	}
	if hasOld {
		delete(config.Containers, oldName)
		oldEntry.UpdatedAt = nowRFC3339()
		config.Containers[newName] = oldEntry
	}
	delete(config.RenameIntents, oldName)
	if err := writeFile(path, config); err != nil {
		return err
	}
	incrementPortOverridesGenerationLocked(path)
	return nil
}

// reconcilePortRenameIntents resolves only facts established by a complete
// Docker list. Ambiguous presence preserves both names and the intent.
func reconcilePortRenameIntents(config *containerPortOverridesFile, active map[string]struct{}, identities []ContainerPortIdentity) (bool, []string) {
	if config.RenameIntents == nil {
		return false, nil
	}
	changed := false
	nameByID := make(map[string]string, len(identities))
	for _, identity := range identities {
		id := strings.ToLower(strings.TrimSpace(identity.ID))
		name := normalizeContainerName(identity.Name)
		if id != "" && name != "" {
			nameByID[id] = name
		}
	}
	protected := make([]string, 0, len(config.RenameIntents)*2)
	for oldName, intent := range config.RenameIntents {
		oldName = normalizeContainerName(oldName)
		newName := normalizeContainerName(intent.NewName)
		_, oldPresent := active[oldName]
		_, newPresent := active[newName]
		currentName := nameByID[strings.ToLower(strings.TrimSpace(intent.ContainerID))]
		switch {
		case intent.ContainerID != "" && currentName == newName && newPresent:
			oldEntry, hasOld := config.Containers[oldName]
			_, hasNew := config.Containers[newName]
			if hasOld && hasNew {
				protected = append(protected, oldName, newName)
				continue
			}
			if hasOld {
				delete(config.Containers, oldName)
				oldEntry.UpdatedAt = nowRFC3339()
				config.Containers[newName] = oldEntry
			}
			delete(config.RenameIntents, oldName)
			changed = true
		case intent.ContainerID != "" && currentName == oldName && oldPresent:
			delete(config.RenameIntents, oldName)
			changed = true
		default:
			protected = append(protected, oldName, newName)
		}
	}
	return changed, protected
}

func portOverridesPathKey(path string) string {
	return filepath.Clean(path)
}

func incrementPortOverridesGenerationLocked(path string) {
	containerPortOverridesGeneration[portOverridesPathKey(path)]++
}

func normalizeContainerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func loadContainerPortOverridesLocked(path string) (map[string][]types.ConfiguredPort, error) {
	config, err := readContainerPortOverridesFileLocked(path)
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

func readContainerPortOverridesFileLocked(path string) (containerPortOverridesFile, error) {
	config := containerPortOverridesFile{
		SchemaVersion: 1,
		Containers:    make(map[string]containerPortOverridesContainer),
	}
	content, err := os.ReadFile(path)
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

func writeContainerPortOverridesFileLocked(path string, config containerPortOverridesFile) error {
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

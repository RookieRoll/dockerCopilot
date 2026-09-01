package utiles

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestSaveLoadAndMigrateContainerPortOverrides(t *testing.T) {
	originalPath := containerPortOverridesPath
	originalTimeNow := timeNow
	containerPortOverridesPath = filepath.Join(t.TempDir(), "config", containerPortOverridesFileName)
	timeNow = func() time.Time { return time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC) }
	t.Cleanup(func() {
		containerPortOverridesPath = originalPath
		timeNow = originalTimeNow
	})

	input := []types.ConfiguredPort{
		{Port: 18080, Protocol: "TCP", Label: " API "},
		{Port: 12712, Protocol: "udp", Label: "Discovery"},
	}
	if err := SaveContainerPortOverrides("host-service", input); err != nil {
		t.Fatalf("save configured ports: %v", err)
	}

	overrides, err := LoadContainerPortOverrides()
	if err != nil {
		t.Fatalf("load configured ports: %v", err)
	}
	expected := []types.ConfiguredPort{
		{Port: 12712, Protocol: "udp", Label: "Discovery"},
		{Port: 18080, Protocol: "tcp", Label: "API"},
	}
	if !reflect.DeepEqual(overrides["host-service"], expected) {
		t.Fatalf("unexpected overrides: %#v", overrides)
	}

	if err := MigrateContainerPortOverrides("host-service", "renamed-service"); err != nil {
		t.Fatalf("migrate configured ports: %v", err)
	}
	overrides, err = LoadContainerPortOverrides()
	if err != nil {
		t.Fatalf("load migrated configured ports: %v", err)
	}
	if _, found := overrides["host-service"]; found {
		t.Fatalf("old container name remains after migration: %#v", overrides)
	}
	if !reflect.DeepEqual(overrides["renamed-service"], expected) {
		t.Fatalf("unexpected migrated overrides: %#v", overrides)
	}

	content, err := os.ReadFile(containerPortOverridesPath)
	if err != nil {
		t.Fatalf("read stored configuration: %v", err)
	}
	if !containsAll(string(content), []string{`"schemaVersion": 1`, `"renamed-service"`, `"updatedAt"`}) {
		t.Fatalf("unexpected stored config: %s", content)
	}
}

func TestCleanupContainerPortOverridesRemovesStaleEntries(t *testing.T) {
	originalPath := containerPortOverridesPath
	containerPortOverridesPath = filepath.Join(t.TempDir(), "config", containerPortOverridesFileName)
	t.Cleanup(func() { containerPortOverridesPath = originalPath })

	if err := SaveContainerPortOverrides("active-service", []types.ConfiguredPort{{Port: 8080, Protocol: "tcp"}}); err != nil {
		t.Fatalf("save active configured ports: %v", err)
	}
	if err := SaveContainerPortOverrides("deleted-service", []types.ConfiguredPort{{Port: 9090, Protocol: "tcp"}}); err != nil {
		t.Fatalf("save stale configured ports: %v", err)
	}

	removed, err := CleanupContainerPortOverrides([]string{"/active-service"})
	if err != nil {
		t.Fatalf("cleanup stale configured ports: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected one stale entry to be removed, got %d", removed)
	}

	overrides, err := LoadContainerPortOverrides()
	if err != nil {
		t.Fatalf("load cleaned configured ports: %v", err)
	}
	if _, found := overrides["deleted-service"]; found {
		t.Fatalf("stale container override remains: %#v", overrides)
	}
	if _, found := overrides["active-service"]; !found {
		t.Fatalf("active container override was removed: %#v", overrides)
	}
}

func TestRemoveContainerPortOverrides(t *testing.T) {
	originalPath := containerPortOverridesPath
	containerPortOverridesPath = filepath.Join(t.TempDir(), "config", containerPortOverridesFileName)
	t.Cleanup(func() { containerPortOverridesPath = originalPath })

	if err := SaveContainerPortOverrides("host-service", []types.ConfiguredPort{{Port: 18080, Protocol: "tcp"}}); err != nil {
		t.Fatalf("save configured ports: %v", err)
	}
	if err := RemoveContainerPortOverrides("/host-service"); err != nil {
		t.Fatalf("remove configured ports: %v", err)
	}
	overrides, err := LoadContainerPortOverrides()
	if err != nil {
		t.Fatalf("load configured ports after remove: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("expected no overrides after removal, got %#v", overrides)
	}
}

func TestConfiguredPortValidation(t *testing.T) {
	tests := []struct {
		name  string
		ports []types.ConfiguredPort
	}{
		{name: "port zero", ports: []types.ConfiguredPort{{Port: 0, Protocol: "tcp"}}},
		{name: "port too high", ports: []types.ConfiguredPort{{Port: 65536, Protocol: "tcp"}}},
		{name: "unsupported protocol", ports: []types.ConfiguredPort{{Port: 8080, Protocol: "sctp"}}},
		{name: "duplicate mapping", ports: []types.ConfiguredPort{{Port: 8080, Protocol: "tcp"}, {Port: 8080, Protocol: "TCP"}}},
		{name: "too many ports", ports: makeConfiguredPorts(maxConfiguredPorts + 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NormalizeConfiguredPorts(tt.ports); err == nil {
				t.Fatalf("expected validation error for %#v", tt.ports)
			}
		})
	}
}

func TestSaveEmptyConfiguredPortsRemovesContainerEntry(t *testing.T) {
	originalPath := containerPortOverridesPath
	containerPortOverridesPath = filepath.Join(t.TempDir(), "config", containerPortOverridesFileName)
	t.Cleanup(func() { containerPortOverridesPath = originalPath })

	if err := SaveContainerPortOverrides("host-service", []types.ConfiguredPort{{Port: 8080, Protocol: "tcp"}}); err != nil {
		t.Fatalf("save configured ports: %v", err)
	}
	if err := SaveContainerPortOverrides("host-service", nil); err != nil {
		t.Fatalf("clear configured ports: %v", err)
	}
	overrides, err := LoadContainerPortOverrides()
	if err != nil {
		t.Fatalf("load configured ports: %v", err)
	}
	if _, found := overrides["host-service"]; found {
		t.Fatalf("container override was not removed: %#v", overrides)
	}
}

func makeConfiguredPorts(count int) []types.ConfiguredPort {
	ports := make([]types.ConfiguredPort, 0, count)
	for port := 1; port <= count; port++ {
		ports = append(ports, types.ConfiguredPort{Port: port, Protocol: "tcp"})
	}
	return ports
}

func containsAll(content string, values []string) bool {
	for _, value := range values {
		if !strings.Contains(content, value) {
			return false
		}
	}
	return true
}

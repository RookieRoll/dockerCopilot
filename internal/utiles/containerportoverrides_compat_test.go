package utiles

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestLoadLegacyConfiguredPortsFileSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "container-port-overrides.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create temp config directory: %v", err)
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "legacy-container-port-overrides.json"))
	if err != nil {
		t.Fatalf("read legacy port fixture: %v", err)
	}
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		t.Fatalf("seed legacy port fixture: %v", err)
	}
	t.Setenv("CONTAINER_PORT_OVERRIDES_PATH", path)

	got, err := LoadContainerPortOverrides()
	if err != nil {
		t.Fatalf("load legacy port file: %v", err)
	}
	want := []types.ConfiguredPort{{Port: 5353, Protocol: "udp", Label: "Discovery"}, {Port: 12712, Protocol: "tcp", Label: "Web"}}
	if !reflect.DeepEqual(got["host-service"], want) {
		t.Fatalf("legacy file parsed as %#v, want %#v", got["host-service"], want)
	}
}

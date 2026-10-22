package settings

import (
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestPortOverridesServiceUsesConfiguredPath(t *testing.T) {
	service := NewPortOverridesService(t.TempDir() + "/ports.json")
	if err := service.Save("web", []types.ConfiguredPort{{Port: 8080, Protocol: "TCP", Label: "http"}}); err != nil {
		t.Fatal(err)
	}
	ports, err := service.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := ports["web"]; len(got) != 1 || got[0].Protocol != "tcp" {
		t.Fatalf("unexpected port settings: %#v", ports)
	}
	removed, _, err := service.Cleanup(nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expected stale entry cleanup, got %d", removed)
	}
}

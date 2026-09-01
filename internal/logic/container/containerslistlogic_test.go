package container

import (
	"encoding/json"
	"reflect"
	"testing"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/go-connections/nat"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestMapContainerPorts(t *testing.T) {
	ports := mapContainerPorts([]dockerTypes.Port{
		{IP: "127.0.0.1", PublicPort: 5353, PrivatePort: 53, Type: "udp"},
		{IP: "0.0.0.0", PublicPort: 8080, PrivatePort: 80, Type: "tcp"},
		{IP: "", PublicPort: 0, PrivatePort: 443, Type: "tcp"},
		{IP: "0.0.0.0", PublicPort: 8080, PrivatePort: 8080, Type: "tcp"},
		{IP: "::", PublicPort: 8080, PrivatePort: 80, Type: "TCP"},
	})

	expected := []PortInfo{
		{HostIP: "", HostPort: 0, ContainerPort: 443, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
		{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 8080, Protocol: "tcp"},
	}
	if !reflect.DeepEqual(ports, expected) {
		t.Fatalf("unexpected mapped ports: %#v", ports)
	}
}

func TestMapHostNetworkPortsUsesDeclaredExposedPorts(t *testing.T) {
	webPort, err := nat.NewPort("tcp", "8080")
	if err != nil {
		t.Fatalf("failed to create exposed port: %v", err)
	}
	udpPort, err := nat.NewPort("udp", "5353")
	if err != nil {
		t.Fatalf("failed to create exposed port: %v", err)
	}

	ports := mapHostNetworkPorts(nat.PortSet{webPort: {}, udpPort: {}})
	expected := []PortInfo{
		{HostPort: 0, ContainerPort: 5353, Protocol: "udp"},
		{HostPort: 0, ContainerPort: 8080, Protocol: "tcp"},
	}
	if !reflect.DeepEqual(ports, expected) {
		t.Fatalf("unexpected host network ports: %#v", ports)
	}
}

func TestMapContainerPortsReturnsEmptyArray(t *testing.T) {
	ports := mapContainerPorts(nil)
	if ports == nil {
		t.Fatal("expected non-nil empty port array")
	}
	if len(ports) != 0 {
		t.Fatalf("expected no ports, got %d", len(ports))
	}
}

func TestInfoPortsUseLowerCamelCaseJSON(t *testing.T) {
	encoded, err := json.Marshal(Info{Ports: []PortInfo{{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}}, ConfiguredPorts: []types.ConfiguredPort{}})
	if err != nil {
		t.Fatalf("failed to marshal container info: %v", err)
	}
	expected := `{"id":"","status":"","name":"","usingImage":"","createImage":"","createTime":"","runningTime":"","haveUpdate":false,"ports":[{"hostIp":"0.0.0.0","hostPort":8080,"containerPort":80,"protocol":"tcp"}],"configuredPorts":[]}`
	if string(encoded) != expected {
		t.Fatalf("unexpected JSON: %s", encoded)
	}
}

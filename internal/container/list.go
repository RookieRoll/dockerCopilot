package container

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/go-connections/nat"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type PortInfo struct {
	HostIP        string `json:"hostIp"`
	HostPort      uint16 `json:"hostPort"`
	ContainerPort uint16 `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

type Info struct {
	Id              string                 `json:"id"`
	Status          string                 `json:"status"`
	Name            string                 `json:"name"`
	UsingImage      string                 `json:"usingImage"`
	CreateImage     string                 `json:"createImage"`
	CreateTime      string                 `json:"createTime"`
	RunningTime     string                 `json:"runningTime"`
	HaveUpdate      bool                   `json:"haveUpdate"`
	NetworkMode     string                 `json:"networkMode,omitempty"`
	Ports           []PortInfo             `json:"ports"`
	ConfiguredPorts []types.ConfiguredPort `json:"configuredPorts"`
}

type PortSettings interface {
	Cleanup([]string) (int, map[string][]types.ConfiguredPort, error)
}

type stablePortSettings interface {
	PortSettings
	Generation() uint64
	CleanupStableWithContainers([]settings.ContainerPortIdentity, []string, uint64) (int, map[string][]types.ConfiguredPort, bool, error)
	Load() (map[string][]types.ConfiguredPort, error)
}

var errStalePortSettingsSnapshot = errors.New("port settings snapshot changed during container list")

func DeduplicateAndSortPorts(ports []PortInfo) []PortInfo {
	unique := make(map[string]PortInfo, len(ports))
	for _, port := range ports {
		port.Protocol = strings.ToLower(strings.TrimSpace(port.Protocol))
		if port.Protocol == "" {
			port.Protocol = "unknown"
		}
		key := fmt.Sprintf("%d:%d/%s", port.HostPort, port.ContainerPort, port.Protocol)
		if _, exists := unique[key]; !exists {
			unique[key] = port
		}
	}
	mapped := make([]PortInfo, 0, len(unique))
	for _, port := range unique {
		mapped = append(mapped, port)
	}
	sort.SliceStable(mapped, func(i, j int) bool {
		if mapped[i].HostPort != mapped[j].HostPort {
			return mapped[i].HostPort < mapped[j].HostPort
		}
		if mapped[i].ContainerPort != mapped[j].ContainerPort {
			return mapped[i].ContainerPort < mapped[j].ContainerPort
		}
		return mapped[i].Protocol < mapped[j].Protocol
	})
	return mapped
}

func MapContainerPorts(ports []dockerTypes.Port) []PortInfo {
	mapped := make([]PortInfo, 0, len(ports))
	for _, port := range ports {
		mapped = append(mapped, PortInfo{HostIP: port.IP, HostPort: port.PublicPort, ContainerPort: port.PrivatePort, Protocol: port.Type})
	}
	return DeduplicateAndSortPorts(mapped)
}

func MapHostNetworkPorts(exposedPorts nat.PortSet) []PortInfo {
	mapped := make([]PortInfo, 0, len(exposedPorts))
	for port := range exposedPorts {
		containerPort := port.Int()
		if containerPort <= 0 || containerPort > 65535 {
			continue
		}
		mapped = append(mapped, PortInfo{ContainerPort: uint16(containerPort), Protocol: port.Proto()})
	}
	return DeduplicateAndSortPorts(mapped)
}

func (s *Service) ListDetails(ctx context.Context, portSettings PortSettings) ([]Info, error) {
	return s.ListDetailsWithProtectedNames(ctx, portSettings, nil)
}

// ListDetailsWithProtectedNames only removes stale port settings when both
// the Docker list snapshot and operation coordinator remained stable while it
// was collected. Protected names come from active in-memory task records.
func (s *Service) ListDetailsWithProtectedNames(ctx context.Context, portSettings PortSettings, protectedNames []string) ([]Info, error) {
	return s.ListDetailsWithOperationVersion(ctx, portSettings, protectedNames, s.Operations().CaptureVersion())
}

// ListDetailsWithOperationVersion accepts a revision captured before the
// caller reads active task protections, closing the window between those
// facts and the Docker list snapshot.
func (s *Service) ListDetailsWithOperationVersion(ctx context.Context, portSettings PortSettings, protectedNames []string, opVersion uint64) ([]Info, error) {
	return s.listDetails(ctx, portSettings, protectedNames, opVersion, true)
}

// ListDetailsWithoutCleanupWithOperationVersion serves a list when task-state
// recovery protections could not be read. Existing mappings are loaded for
// display, but cleanup is skipped entirely.
func (s *Service) ListDetailsWithoutCleanupWithOperationVersion(ctx context.Context, portSettings PortSettings, opVersion uint64) ([]Info, error) {
	return s.listDetails(ctx, portSettings, nil, opVersion, false)
}

func (s *Service) listDetails(ctx context.Context, portSettings PortSettings, protectedNames []string, opVersion uint64, cleanupAllowed bool) ([]Info, error) {
	var portGeneration uint64
	stableSettings, hasStableSettings := portSettings.(stablePortSettings)
	if hasStableSettings {
		portGeneration = stableSettings.Generation()
	}
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	activeNames := make([]string, 0, len(list))
	activeContainers := make([]settings.ContainerPortIdentity, 0, len(list))
	for _, item := range list {
		if len(item.Names) > 0 {
			name := strings.TrimPrefix(item.Names[0], "/")
			activeNames = append(activeNames, name)
			activeContainers = append(activeContainers, settings.ContainerPortIdentity{ID: item.ID, Name: name})
		}
	}
	configured := map[string][]types.ConfiguredPort{}
	if portSettings != nil {
		var removed int
		var entries map[string][]types.ConfiguredPort
		var cleanupErr error
		if !cleanupAllowed {
			loader, ok := portSettings.(interface {
				Load() (map[string][]types.ConfiguredPort, error)
			})
			if ok {
				entries, cleanupErr = loader.Load()
			} else {
				cleanupErr = errors.New("port settings cannot be loaded without cleanup")
			}
		} else if hasStableSettings {
			committed, stableErr := s.Operations().WithStableVersion(opVersion, func() error {
				var stable bool
				removed, entries, stable, cleanupErr = stableSettings.CleanupStableWithContainers(activeContainers, protectedNames, portGeneration)
				if !stable {
					return errStalePortSettingsSnapshot
				}
				return cleanupErr
			})
			if stableErr != nil && stableErr != errStalePortSettingsSnapshot {
				cleanupErr = stableErr
			}
			if !committed || stableErr == errStalePortSettingsSnapshot {
				entries, cleanupErr = stableSettings.Load()
			}
		} else {
			removed, entries, cleanupErr = portSettings.Cleanup(activeNames)
		}
		if cleanupErr != nil {
			logx.Errorf("cleanup configured container ports: %v", cleanupErr)
		} else {
			configured = entries
			if removed > 0 {
				logx.Infof("removed %d stale configured container port entries", removed)
			}
		}
	}
	inspects := make([]dockerTypes.ContainerJSON, len(list))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for idx := range list {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			inspected, inspectErr := s.Inspect(ctx, list[idx].ID)
			if inspectErr != nil {
				logx.Error("get container inspect error: " + list[idx].ID)
				return
			}
			inspects[idx] = inspected
		}(idx)
	}
	wg.Wait()
	result := make([]Info, 0, len(list))
	for i, item := range list {
		info := Info{ConfiguredPorts: make([]types.ConfiguredPort, 0), Id: item.ID, Status: item.State, UsingImage: item.Image, RunningTime: item.Status, HaveUpdate: item.Update, Ports: MapContainerPorts(item.Ports)}
		if len(item.Names) > 0 {
			info.Name = strings.TrimPrefix(item.Names[0], "/")
		} else {
			info.Name = "get container name error"
			logx.Error("get container name error" + item.ID)
		}
		if info.UsingImage == "" {
			info.UsingImage = item.ImageID
			logx.Error("image dont have name" + item.ID)
		}
		inspected := inspects[i]
		if inspected.Config != nil {
			info.CreateImage = inspected.Config.Image
			if imageRef, refErr := UpdateImageReference(inspected.Config); refErr == nil {
				info.UsingImage = imageRef
			}
		} else {
			logx.Error("get image name error" + item.ID)
		}
		if inspected.HostConfig != nil {
			info.NetworkMode = string(inspected.HostConfig.NetworkMode)
		}
		info.CreateTime = time.Unix(item.Created, 0).Format("2006-01-02 15:04:05")
		if info.NetworkMode == "host" {
			info.ConfiguredPorts = configured[info.Name]
			if info.ConfiguredPorts == nil {
				info.ConfiguredPorts = make([]types.ConfiguredPort, 0)
			}
		}
		if info.NetworkMode == "host" && len(info.Ports) == 0 && inspected.Config != nil {
			info.Ports = MapHostNetworkPorts(inspected.Config.ExposedPorts)
		}
		result = append(result, info)
	}
	return result, nil
}

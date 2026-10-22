package container

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/go-connections/nat"
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
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	activeNames := make([]string, 0, len(list))
	for _, item := range list {
		if len(item.Names) > 0 {
			activeNames = append(activeNames, strings.TrimPrefix(item.Names[0], "/"))
		}
	}
	configured := map[string][]types.ConfiguredPort{}
	if portSettings != nil {
		removed, entries, cleanupErr := portSettings.Cleanup(activeNames)
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

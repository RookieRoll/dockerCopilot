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
	"github.com/onlyLTY/dockerCopilot/internal/utiles"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ContainersListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

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

func deduplicateAndSortPorts(ports []PortInfo) []PortInfo {
	// Docker may return the same logical mapping once per bound host address
	// (for example, both IPv4 and IPv6). HostIP is not part of the display
	// mapping, so collapse those entries to avoid showing duplicate ports.
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

func mapContainerPorts(ports []dockerTypes.Port) []PortInfo {
	mapped := make([]PortInfo, 0, len(ports))
	for _, port := range ports {
		mapped = append(mapped, PortInfo{
			HostIP:        port.IP,
			HostPort:      port.PublicPort,
			ContainerPort: port.PrivatePort,
			Protocol:      port.Type,
		})
	}
	return deduplicateAndSortPorts(mapped)
}

func mapHostNetworkPorts(exposedPorts nat.PortSet) []PortInfo {
	mapped := make([]PortInfo, 0, len(exposedPorts))
	for port := range exposedPorts {
		containerPort := port.Int()
		if containerPort <= 0 || containerPort > 65535 {
			continue
		}
		mapped = append(mapped, PortInfo{
			ContainerPort: uint16(containerPort),
			Protocol:      port.Proto(),
		})
	}
	return deduplicateAndSortPorts(mapped)
}

func NewContainersListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ContainersListLogic {
	return &ContainersListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ContainersListLogic) ContainersList() (resp *types.Resp, err error) {
	// 获取所有容器（包括停止的容器）
	resp = &types.Resp{}
	list, err := utiles.GetContainerList(l.svcCtx)
	if err != nil {
		resp.Code = 500
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	resp.Code = 200
	resp.Msg = "success"
	activeContainerNames := make([]string, 0, len(list))
	for _, container := range list {
		if len(container.Names) > 0 {
			activeContainerNames = append(activeContainerNames, strings.TrimPrefix(container.Names[0], "/"))
		}
	}
	removed, configuredPortOverrides, cleanupErr := utiles.CleanupContainerPortOverrides(activeContainerNames)
	if cleanupErr != nil {
		l.Errorf("cleanup configured container ports: %v", cleanupErr)
		configuredPortOverrides = map[string][]types.ConfiguredPort{}
	} else if removed > 0 {
		l.Infof("removed %d stale configured container port entries", removed)
	}
	var containerInfoList []Info
	list = utiles.CheckImageUpdate(l.svcCtx, list)

	// 并行预取 inspect 结果(8 路限流),消除逐容器串行往返。
	inspects := make([]dockerTypes.ContainerJSON, len(list))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for idx := range list {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			inspected, inspectErr := utiles.GetContainerInspect(l.svcCtx, list[idx].ID)
			if inspectErr != nil {
				l.Error("get container inspect error: " + list[idx].ID)
				return
			}
			inspects[idx] = inspected
		}(idx)
	}
	wg.Wait()

	for i, v := range list {
		containerInfo := Info{ConfiguredPorts: make([]types.ConfiguredPort, 0)}
		containerInfo.Id = v.ID
		containerInfo.Status = v.State
		if len(v.Names) > 0 {
			ContainerName := v.Names[0][1:]
			containerInfo.Name = ContainerName
		} else {
			containerInfo.Name = "get container name error"
			l.Error("get container name error" + v.ID)
		}
		if v.Image != "" {
			containerInfo.UsingImage = v.Image
		} else {
			containerInfo.UsingImage = v.ImageID
			l.Error("image dont have name" + v.ID)
		}
		containerInspect := inspects[i]
		if containerInspect.Config != nil {
			containerInfo.CreateImage = containerInspect.Config.Image
		} else {
			containerInfo.CreateImage = ""
			l.Error("get image name error" + v.ID)
		}
		if containerInspect.HostConfig != nil {
			containerInfo.NetworkMode = string(containerInspect.HostConfig.NetworkMode)
		}
		t := time.Unix(v.Created, 0)
		containerInfo.CreateTime = t.Format("2006-01-02 15:04:05")
		containerInfo.RunningTime = v.Status
		containerInfo.HaveUpdate = v.Update
		containerInfo.Ports = mapContainerPorts(v.Ports)
		if containerInfo.NetworkMode == "host" {
			containerInfo.ConfiguredPorts = configuredPortOverrides[containerInfo.Name]
			if containerInfo.ConfiguredPorts == nil {
				containerInfo.ConfiguredPorts = make([]types.ConfiguredPort, 0)
			}
		}
		if containerInfo.NetworkMode == "host" && len(containerInfo.Ports) == 0 && containerInspect.Config != nil {
			// Host networking has no Docker port-publishing records. The only
			// port metadata available from inspect is the image's exposed-port
			// declaration; it is not a guarantee that a process is listening.
			containerInfo.Ports = mapHostNetworkPorts(containerInspect.Config.ExposedPorts)
		}
		containerInfoList = append(containerInfoList, containerInfo)
	}
	resp.Data = containerInfoList
	return resp, nil
}

package container

import (
	"context"
	"strings"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/go-connections/nat"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type ContainersListLogic struct {
	logx.Logger
	ctx     context.Context
	service *containerService.Service
	ports   *settings.PortOverridesService
	svcCtx  *svc.ServiceContext
}

type PortInfo = containerService.PortInfo
type Info = containerService.Info

func deduplicateAndSortPorts(ports []PortInfo) []PortInfo {
	return containerService.DeduplicateAndSortPorts(ports)
}
func mapContainerPorts(ports []dockerTypes.Port) []PortInfo {
	return containerService.MapContainerPorts(ports)
}
func mapHostNetworkPorts(ports nat.PortSet) []PortInfo {
	return containerService.MapHostNetworkPorts(ports)
}

func NewContainersListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ContainersListLogic {
	return &ContainersListLogic{Logger: logx.WithContext(ctx), ctx: ctx, service: serviceFromContext(svcCtx), ports: portServiceFromContext(svcCtx), svcCtx: svcCtx}
}

func (l *ContainersListLogic) ContainersList() (*types.Resp, error) {
	opVersion := l.service.Operations().CaptureVersion()
	protectedNames := []string{}
	cleanupAllowed := true
	if l.svcCtx != nil && l.svcCtx.TaskStore != nil {
		records, err := l.svcCtx.TaskStore.List()
		if err != nil {
			// A failed recovery-store read must never make a stale Docker
			// snapshot authoritative for deleting persisted port settings.
			cleanupAllowed = false
			logx.Errorf("load task protections for port cleanup: %v", err)
		} else {
			protectedNames = protectedPortNames(records)
		}
	}
	var items []containerService.Info
	var err error
	if cleanupAllowed {
		items, err = l.service.ListDetailsWithOperationVersion(l.ctx, l.ports, protectedNames, opVersion)
	} else {
		items, err = l.service.ListDetailsWithoutCleanupWithOperationVersion(l.ctx, l.ports, opVersion)
	}
	if err != nil {
		return &types.Resp{Code: 500, Msg: err.Error(), Data: map[string]interface{}{}}, err
	}
	return &types.Resp{Code: 200, Msg: "success", Data: items}, nil
}

func protectedPortNames(records []task.Record) []string {
	seen := make(map[string]struct{})
	add := func(name string) {
		name = strings.TrimLeft(strings.TrimSpace(name), "/")
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	for _, record := range records {
		parentProtected := !record.Status.Terminal() || record.RequiresAttention ||
			len(record.ProtectedNames) > 0 || record.SnapshotRef != "" ||
			(record.Recovery.Status != "" && record.Recovery.Status != "not-needed" && record.Recovery.Status != "restored")
		if parentProtected {
			for _, name := range record.ProtectedNames {
				add(name)
			}
			add(record.Name)
			add(record.BackupContainerName)
		}
		for _, item := range record.Items {
			itemProtected := parentProtected || item.RequiresAttention || len(item.ProtectedNames) > 0 || item.SnapshotRef != "" ||
				(item.Recovery.Status != "" && item.Recovery.Status != "not-needed" && item.Recovery.Status != "restored")
			if itemProtected {
				for _, name := range item.ProtectedNames {
					add(name)
				}
				add(item.Name)
			}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	return result
}

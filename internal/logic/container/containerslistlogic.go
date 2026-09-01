package container

import (
	"context"
	"sort"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
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
	Id          string     `json:"id"`
	Status      string     `json:"status"`
	Name        string     `json:"name"`
	UsingImage  string     `json:"usingImage"`
	CreateImage string     `json:"createImage"`
	CreateTime  string     `json:"createTime"`
	RunningTime string     `json:"runningTime"`
	HaveUpdate  bool       `json:"haveUpdate"`
	Ports       []PortInfo `json:"ports"`
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

	sort.SliceStable(mapped, func(i, j int) bool {
		if mapped[i].HostPort != mapped[j].HostPort {
			return mapped[i].HostPort < mapped[j].HostPort
		}
		if mapped[i].ContainerPort != mapped[j].ContainerPort {
			return mapped[i].ContainerPort < mapped[j].ContainerPort
		}
		if mapped[i].Protocol != mapped[j].Protocol {
			return mapped[i].Protocol < mapped[j].Protocol
		}
		return mapped[i].HostIP < mapped[j].HostIP
	})

	return mapped
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
	resp.Msg = "success"
	var containerInfoList []Info
	list = utiles.CheckImageUpdate(l.svcCtx, list)
	for _, v := range list {
		var containerInfo Info
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
		containerInspect, err := utiles.GetContainerInspect(l.svcCtx, v.ID)
		if err != nil {
			containerInfo.CreateImage = ""
			l.Error("get image name error" + v.ID)
		}
		containerInfo.CreateImage = containerInspect.Config.Image
		t := time.Unix(v.Created, 0)
		containerInfo.CreateTime = t.Format("2006-01-02 15:04:05")
		containerInfo.RunningTime = v.Status
		containerInfo.HaveUpdate = v.Update
		containerInfo.Ports = mapContainerPorts(v.Ports)
		containerInfoList = append(containerInfoList, containerInfo)
	}
	resp.Data = containerInfoList
	return resp, nil
}

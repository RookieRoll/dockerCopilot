package image

import (
	"context"
	"time"

	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ImagesListLogic struct {
	logx.Logger
	ctx     context.Context
	service *imageService.Service
}

type Info struct {
	Id         string `json:"id"`
	Name       string `json:"name"`
	Tag        string `json:"tag"`
	Size       string `json:"size"`
	InUsed     bool   `json:"inUsed"`
	CreateTime string `json:"createTime"`
}

func NewImagesListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ImagesListLogic {
	return &ImagesListLogic{
		Logger:  logx.WithContext(ctx),
		ctx:     ctx,
		service: imageServiceFromContext(svcCtx),
	}
}

func imageServiceFromContext(ctx *svc.ServiceContext) *imageService.Service {
	if ctx.Images != nil {
		return ctx.Images
	}
	return imageService.NewService(ctx.DockerClient)
}

func (l *ImagesListLogic) ImagesList() (resp *types.Resp, err error) {
	resp = &types.Resp{}
	list, err := l.service.List(l.ctx)
	if err != nil {
		resp.Code = 500
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	resp.Code = 200
	resp.Msg = "success"
	var imageInfoList []Info
	for _, v := range list {
		var imageInfo Info
		imageInfo.Id = v.ID
		imageInfo.Name = v.ImageName
		imageInfo.Tag = v.ImageTag
		imageInfo.Size = v.SizeFormat
		imageInfo.InUsed = v.InUsed
		t := time.Unix(v.Created, 0)
		imageInfo.CreateTime = t.Format("2006-01-02 15:04:05")
		imageInfoList = append(imageInfoList, imageInfo)
	}
	resp.Data = imageInfoList
	return resp, nil
}

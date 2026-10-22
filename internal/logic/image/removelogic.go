package image

import (
	"context"
	"strings"

	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type RemoveLogic struct {
	logx.Logger
	ctx     context.Context
	service *imageService.Service
}

func NewRemoveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RemoveLogic {
	return &RemoveLogic{
		Logger:  logx.WithContext(ctx),
		ctx:     ctx,
		service: imageServiceFromContext(svcCtx),
	}
}

func (l *RemoveLogic) Remove(req *types.RemoveImageReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	var imageId = req.Id
	if strings.HasPrefix(imageId, "sha256:") {
		imageId = strings.TrimPrefix(imageId, "sha256:")
	}
	err = l.service.Remove(l.ctx, req.Id, req.Force)
	if err != nil {
		resp.Code = 409
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, nil
	}
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]interface{}{}
	return resp, nil
}

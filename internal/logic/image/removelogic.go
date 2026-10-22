package image

import (
	"context"
	"errors"
	"net/http"

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
		service: svcCtx.ImageService(),
	}
}

func (l *RemoveLogic) Remove(req *types.RemoveImageReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	if req == nil {
		return &types.Resp{Code: http.StatusBadRequest, Msg: "image ID is required", Data: map[string]any{}}, errors.New("image ID is required")
	}
	err = l.service.Remove(l.ctx, req.Id, req.Force)
	if err != nil {
		resp.Code = http.StatusInternalServerError
		var statusError interface{ HTTPStatus() int }
		if errors.As(err, &statusError) {
			resp.Code = statusError.HTTPStatus()
		}
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		var conflict *imageService.ConflictError
		if errors.As(err, &conflict) && conflict.TaskID != "" {
			resp.Data = map[string]string{"taskID": conflict.TaskID}
		}
		return resp, err
	}
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]interface{}{}
	return resp, nil
}

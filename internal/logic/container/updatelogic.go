package container

import (
	"context"
	"github.com/google/uuid"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateLogic struct {
	logx.Logger
	ctx              context.Context
	service          *containerService.Service
	deleteOldDefault bool
}

func NewUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateLogic {
	return &UpdateLogic{
		Logger:           logx.WithContext(ctx),
		ctx:              ctx,
		service:          serviceFromContext(svcCtx),
		deleteOldDefault: svcCtx.Config.ResolveDeleteOldContainer(nil),
	}
}

func (l *UpdateLogic) Update(req *types.ContainerUpdateReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	taskID := uuid.New().String()
	go func() {
		// Catch any panic and log the error
		defer func() {
			if r := recover(); r != nil {
				l.Errorf("Recovered from panic in UpdateContainer: %v", r)
			}
		}()
		imageNameAndTag := req.ImageNameAndTag
		delOldContainer := l.deleteOldDefault
		if req.DelOldContainer != nil {
			delOldContainer = *req.DelOldContainer
		}
		err := l.service.Update(context.Background(), req.Id, req.ContainerName, imageNameAndTag, delOldContainer, taskID)
		if err != nil {
			l.Errorf("Error in UpdateContainer: %v", err)
		}
	}()
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]string{"taskID": taskID}
	return resp, nil
}

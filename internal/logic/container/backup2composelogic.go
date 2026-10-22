package container

import (
	"context"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type Backup2composeLogic struct {
	logx.Logger
	ctx     context.Context
	service *backupService.Service
}

func NewBackup2composeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *Backup2composeLogic {
	return &Backup2composeLogic{
		Logger:  logx.WithContext(ctx),
		ctx:     ctx,
		service: backupServiceFromContext(svcCtx),
	}
}

func (l *Backup2composeLogic) Backup2compose() (resp *types.Resp, err error) {
	resp = &types.Resp{}
	err = l.service.ExportCompose(l.ctx)
	if err != nil {
		return nil, err
	}
	resp.Code = 200
	resp.Msg = "success"
	return
}

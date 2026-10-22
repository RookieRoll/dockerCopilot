package container

import (
	"context"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type BackupLogic struct {
	logx.Logger
	ctx     context.Context
	service *backupService.Service
}

func NewBackupLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BackupLogic {
	return &BackupLogic{
		Logger:  logx.WithContext(ctx),
		ctx:     ctx,
		service: backupServiceFromContext(svcCtx),
	}
}

func (l *BackupLogic) Backup() (resp *types.Resp, err error) {
	resp = &types.Resp{}
	err = l.service.Export(l.ctx)
	if err != nil {
		resp.Code = 500
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	resp.Msg = "success"
	resp.Code = 200
	resp.Data = map[string]interface{}{}
	return resp, nil
}

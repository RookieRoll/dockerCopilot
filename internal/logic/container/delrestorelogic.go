package container

import (
	"context"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type DelRestoreLogic struct {
	logx.Logger
	ctx     context.Context
	service *backupService.Service
}

func NewDelRestoreLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelRestoreLogic {
	return &DelRestoreLogic{
		Logger:  logx.WithContext(ctx),
		ctx:     ctx,
		service: backupServiceFromContext(svcCtx),
	}
}

func (l *DelRestoreLogic) DelRestore(req *types.DelContainerBackupReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	if err := l.service.Delete(req.Filename, ".json", ".yaml"); err != nil {
		resp.Code = 400
		resp.Msg = "非法文件名"
		resp.Data = map[string]interface{}{}
		return resp, nil
	}
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]interface{}{}
	return resp, nil
}

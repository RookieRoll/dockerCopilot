package container

import (
	"context"
	"errors"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"net/http"

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
	if err := l.service.Delete(req.Filename, ".json", ".yaml", ".yml"); err != nil {
		resp.Code = http.StatusBadRequest
		var statusError interface{ HTTPStatus() int }
		if errors.As(err, &statusError) {
			resp.Code = statusError.HTTPStatus()
		}
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]interface{}{}
	return resp, nil
}

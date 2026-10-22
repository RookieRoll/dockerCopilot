package container

import (
	"context"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type BackupLogic struct {
	logx.Logger
	ctx        context.Context
	taskRunner *task.Runner
	service    *backupService.Service
}

func NewBackupLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BackupLogic {
	return &BackupLogic{
		Logger:     logx.WithContext(ctx),
		ctx:        ctx,
		taskRunner: svcCtx.TaskRunner,
		service:    backupServiceFromContext(svcCtx),
	}
}

func (l *BackupLogic) Submit() (*types.Resp, error) {
	if l.taskRunner == nil {
		return nil, taskUnavailableError("task runner is unavailable")
	}
	id, err := l.taskRunner.Submit(task.TaskSpec{
		Record: task.Record{Type: "backup-export", Name: "container-config"},
		Run: func(ctx context.Context, execution *task.Execution) error {
			if err := execution.Progress("exporting", 0, "容器配置导出已开始"); err != nil {
				return err
			}
			if err := l.service.Export(ctx); err != nil {
				return err
			}
			return execution.Progress("completed", 100, "容器配置导出完成")
		},
	})
	if err != nil {
		return &types.Resp{Msg: err.Error(), Data: map[string]any{}}, taskSubmitError{cause: err}
	}
	return &types.Resp{Code: 200, Msg: "accepted", Data: map[string]string{"taskID": id}}, nil
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

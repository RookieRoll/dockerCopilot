package container

import (
	"context"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type Backup2composeLogic struct {
	logx.Logger
	ctx        context.Context
	taskRunner *task.Runner
	service    *backupService.Service
}

func NewBackup2composeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *Backup2composeLogic {
	return &Backup2composeLogic{
		Logger:     logx.WithContext(ctx),
		ctx:        ctx,
		taskRunner: svcCtx.TaskRunner,
		service:    backupServiceFromContext(svcCtx),
	}
}

func (l *Backup2composeLogic) Submit() (*types.Resp, error) {
	if l.taskRunner == nil {
		return nil, taskUnavailableError("task runner is unavailable")
	}
	id, err := l.taskRunner.Submit(task.TaskSpec{
		Record: task.Record{Type: "backup-compose-export", Name: "compose"},
		Run: func(ctx context.Context, execution *task.Execution) error {
			if err := execution.Progress("exporting", 0, "Compose导出已开始"); err != nil {
				return err
			}
			if err := l.service.ExportCompose(ctx); err != nil {
				return err
			}
			return execution.Progress("completed", 100, "Compose导出完成")
		},
	})
	if err != nil {
		return &types.Resp{Msg: err.Error(), Data: map[string]any{}}, taskSubmitError{cause: err}
	}
	return &types.Resp{Code: 200, Msg: "accepted", Data: map[string]string{"taskID": id}}, nil
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

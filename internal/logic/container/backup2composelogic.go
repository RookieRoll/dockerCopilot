package container

import (
	"context"
	"github.com/google/uuid"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/operation"

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
	gate       *operation.MutationGate
}

func NewBackup2composeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *Backup2composeLogic {
	return &Backup2composeLogic{
		Logger:     logx.WithContext(ctx),
		ctx:        ctx,
		taskRunner: svcCtx.TaskRunner,
		service:    backupServiceFromContext(svcCtx),
		gate:       svcCtx.MutationGate(),
	}
}

func (l *Backup2composeLogic) Submit() (*types.Resp, error) {
	if l.taskRunner == nil {
		return nil, taskUnavailableError("task runner is unavailable")
	}
	taskID := uuid.NewString()
	var mutationLease *operation.MutationLease
	id, err := l.taskRunner.Submit(task.TaskSpec{
		Record: task.Record{TaskID: taskID, Type: "backup-compose-export", Name: "compose", RequestID: observability.RequestIDFromContext(l.ctx)},
		OnAccepted: func() error {
			var acquireErr error
			mutationLease, acquireErr = l.gate.Acquire(l.ctx, taskID)
			return acquireErr
		},
		OnSettled: func() {
			if mutationLease != nil {
				mutationLease.Release()
			}
		},
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
		if mutationLease != nil {
			mutationLease.Release()
		}
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

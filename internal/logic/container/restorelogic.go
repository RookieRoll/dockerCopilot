package container

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"path/filepath"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type RestoreLogic struct {
	logx.Logger
	ctx        context.Context
	taskRunner *task.Runner
	service    *backupService.Service
}

func NewRestoreLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RestoreLogic {
	return &RestoreLogic{
		Logger:     logx.WithContext(ctx),
		ctx:        ctx,
		taskRunner: svcCtx.TaskRunner,
		service:    backupServiceFromContext(svcCtx),
	}
}

func (l *RestoreLogic) Restore(req *types.ContainerRestoreReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	if req == nil {
		return &types.Resp{Code: http.StatusBadRequest, Msg: "filename is required", Data: map[string]any{}}, requestError("filename is required")
	}
	fileName := req.Filename
	fullPath, pathErr := l.service.Resolve(fileName, ".json")
	if pathErr != nil {
		resp.Code = 400
		resp.Msg = pathErr.Error()
		resp.Data = map[string]interface{}{}
		return resp, requestError(pathErr.Error())
	}
	fileName = filepath.Base(fullPath)
	if filepath.Ext(fileName) != ".json" {
		err = fmt.Errorf("目前仅支持config备份恢复")
		resp.Code = 400
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, requestError(err.Error())
	}
	if l.taskRunner == nil {
		return nil, taskUnavailableError("task runner is unavailable")
	}
	taskID, submitErr := l.taskRunner.Submit(task.TaskSpec{
		Record: task.Record{Type: "backup-restore", Name: fileName},
		Run: func(ctx context.Context, execution *task.Execution) error {
			if err := execution.Progress("restoring", 0, "备份恢复已开始"); err != nil {
				return err
			}
			result, restoreErr := l.service.RestoreWithResults(ctx, fileName, execution.TaskID())
			if len(result.Items) > 0 {
				if err := execution.Critical("restore-items", func(record *task.Record) {
					for _, item := range result.Items {
						record.Items = append(record.Items, task.Item{
							ID: fmt.Sprintf("%d", item.Index), Name: item.Name,
							Status: item.Status, Error: item.Error,
						})
					}
				}); err != nil {
					return errors.Join(restoreErr, fmt.Errorf("persist restore item results: %w", err))
				}
			}
			if restoreErr != nil {
				return restoreErr
			}
			return execution.Progress("completed", 100, "备份恢复完成")
		},
	})
	if submitErr != nil {
		return &types.Resp{Msg: submitErr.Error(), Data: map[string]any{}}, taskSubmitError{cause: submitErr}
	}
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]interface{}{
		"taskID": taskID,
	}
	return resp, nil
}

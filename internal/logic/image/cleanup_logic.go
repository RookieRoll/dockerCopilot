package image

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type CleanupRequest = types.ImageCleanupReq

type CleanupLogic struct {
	logx.Logger
	ctx     context.Context
	service *imageService.Service
	runner  *task.Runner
	gate    *operation.MutationGate
}

func NewCleanupLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CleanupLogic {
	return &CleanupLogic{
		Logger: logx.WithContext(ctx), ctx: ctx,
		service: svcCtx.ImageService(), runner: svcCtx.TaskRunner,
		gate: svcCtx.MutationGate(),
	}
}

type cleanupRequestError string

func (e cleanupRequestError) Error() string     { return string(e) }
func (e cleanupRequestError) HTTPStatus() int   { return http.StatusBadRequest }
func (e cleanupRequestError) ErrorCode() string { return "INVALID_IMAGE_CLEANUP" }

type cleanupUnavailableError string

func (e cleanupUnavailableError) Error() string     { return string(e) }
func (e cleanupUnavailableError) HTTPStatus() int   { return http.StatusServiceUnavailable }
func (e cleanupUnavailableError) ErrorCode() string { return "TASK_SERVICE_UNAVAILABLE" }

func (l *CleanupLogic) Submit(req *CleanupRequest) (*types.Resp, error) {
	if req == nil {
		return nil, cleanupRequestError("cleanup policy is required")
	}
	policy := imageService.CleanupPolicy(strings.TrimSpace(req.Policy))
	if policy != imageService.CleanupDangling && policy != imageService.CleanupUnused {
		return nil, cleanupRequestError("cleanup policy must be dangling or unused")
	}
	if l.runner == nil || l.service == nil || l.gate == nil {
		return nil, cleanupUnavailableError("task-backed image cleanup is unavailable")
	}
	taskID := uuid.NewString()
	var mutationLease *operation.MutationLease
	taskID, err := l.runner.Submit(task.TaskSpec{
		Record: task.Record{
			TaskID: taskID, Type: "image-cleanup", Name: string(policy), ImageRef: string(policy),
			RequestID: observability.RequestIDFromContext(l.ctx),
		},
		OnAccepted: func() error {
			var acquireErr error
			mutationLease, acquireErr = l.gate.Acquire(context.Background(), taskID)
			return acquireErr
		},
		OnSettled: func() {
			if mutationLease != nil {
				mutationLease.Release()
				mutationLease = nil
			}
		},
		Run: func(ctx context.Context, execution *task.Execution) error {
			if err := execution.Progress("refreshing-images", 0, "正在重新检查镜像使用状态"); err != nil {
				return err
			}
			result, cleanupErr := l.service.Cleanup(ctx, policy)
			if cleanupErr != nil {
				return cleanupErr
			}
			if err := execution.Critical("cleanup-results", func(record *task.Record) {
				record.Items = make([]task.Item, 0, len(result.Items))
				record.SuccessCount, record.FailedCount, record.SkippedCount = result.Deleted, result.Failed, result.Skipped
				for _, item := range result.Items {
					name := item.Name
					if item.Tag != "" {
						name += ":" + item.Tag
					}
					record.Items = append(record.Items, task.Item{ID: item.ImageID, Name: name, Status: item.Status, Error: item.Error})
				}
			}); err != nil {
				return err
			}
			if result.Skipped > 0 {
				execution.Warning(fmt.Sprintf("%d images were skipped because they became used or protected by an active task", result.Skipped))
			}
			if result.Failed > 0 {
				return fmt.Errorf("image cleanup completed with %d failed and %d skipped out of %d images", result.Failed, result.Skipped, result.Total)
			}
			return execution.Progress("completed", 100, fmt.Sprintf("镜像清理完成：删除 %d，跳过 %d", result.Deleted, result.Skipped))
		},
	})
	if err != nil {
		if mutationLease != nil {
			mutationLease.Release()
		}
		return &types.Resp{Msg: err.Error(), Data: map[string]any{}}, fmt.Errorf("accept image cleanup task: %w", err)
	}
	return &types.Resp{Code: http.StatusOK, Msg: "accepted", Data: map[string]string{"taskID": taskID, "status": "queued"}}, nil
}

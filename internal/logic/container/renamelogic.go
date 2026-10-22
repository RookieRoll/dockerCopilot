package container

import (
	"context"
	"errors"
	"net/http"

	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type portMigrationError struct{ cause error }

func (e portMigrationError) Error() string {
	return "container was renamed but port configuration migration is pending recovery"
}
func (e portMigrationError) Unwrap() error     { return e.cause }
func (e portMigrationError) HTTPStatus() int   { return http.StatusInternalServerError }
func (e portMigrationError) ErrorCode() string { return "PORT_CONFIG_MIGRATION_FAILED" }

type portRenameIntentError struct{ cause error }

func (e portRenameIntentError) Error() string {
	return "container rename was not attempted because its recovery intent could not be persisted"
}
func (e portRenameIntentError) Unwrap() error     { return e.cause }
func (e portRenameIntentError) HTTPStatus() int   { return http.StatusInternalServerError }
func (e portRenameIntentError) ErrorCode() string { return "PORT_CONFIG_RENAME_INTENT_FAILED" }

type portRenameOutcomeError struct{ cause error }

func (e portRenameOutcomeError) Error() string {
	return "container rename outcome is unknown; port configuration is protected for recovery"
}
func (e portRenameOutcomeError) Unwrap() error     { return e.cause }
func (e portRenameOutcomeError) HTTPStatus() int   { return http.StatusServiceUnavailable }
func (e portRenameOutcomeError) ErrorCode() string { return "CONTAINER_RENAME_OUTCOME_UNKNOWN" }

type RenameLogic struct {
	logx.Logger
	ctx     context.Context
	service *containerService.Service
	ports   *settings.PortOverridesService
}

func NewRenameLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenameLogic {
	return &RenameLogic{
		Logger:  logx.WithContext(ctx),
		ctx:     ctx,
		service: serviceFromContext(svcCtx),
		ports:   portServiceFromContext(svcCtx),
	}
}

func (l *RenameLogic) Rename(req *types.ContainerRenameReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	var prepared, renamed bool
	oldName := ""
	err = l.service.RenameWithLifecycle(l.ctx, req.Id, req.NewName,
		func(sourceName, containerID string) error {
			oldName = sourceName
			if err := l.ports.BeginRename(sourceName, req.NewName, containerID); err != nil {
				return portRenameIntentError{cause: err}
			}
			prepared = sourceName != req.NewName
			return nil
		},
		func(sourceName, _ string) error {
			renamed = true
			if err := l.ports.CompleteRename(sourceName, req.NewName); err != nil {
				return portMigrationError{cause: err}
			}
			return nil
		},
	)
	if err != nil {
		if renamed {
			observability.LogOperationResult(l.ctx, "container.rename_ports", req.Id, "warning", err)
			resp.Code = http.StatusInternalServerError
			resp.Msg = err.Error()
			resp.Data = map[string]interface{}{"renamed": true, "oldName": oldName, "containerName": req.NewName, "portsMigrated": false, "renamePending": true}
			return resp, err
		}
		if prepared {
			unknown := portRenameOutcomeError{cause: err}
			observability.LogOperationResult(l.ctx, "container.rename", req.Id, "warning", unknown)
			resp.Code = unknown.HTTPStatus()
			resp.Msg = unknown.Error()
			resp.Data = map[string]interface{}{"renamed": false, "containerName": req.NewName, "renamePending": true, "oldName": oldName}
			return resp, unknown
		}
		observability.LogOperationResult(l.ctx, "container.rename", req.Id, "failed", err)
		var classified observability.ClassifiedError
		if errors.As(err, &classified) {
			resp.Code = classified.HTTPStatus()
		} else {
			resp.Code = http.StatusBadRequest
		}
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	observability.LogOperationResult(l.ctx, "container.rename", req.Id, "succeeded")
	resp.Code = 200
	resp.Msg = "success"
	resp.Data = map[string]interface{}{"renamed": true, "containerName": req.NewName, "portsMigrated": true}
	return resp, nil
}

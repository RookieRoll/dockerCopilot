package task

import (
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

type unavailableError string

func (e unavailableError) Error() string     { return string(e) }
func (e unavailableError) HTTPStatus() int   { return http.StatusServiceUnavailable }
func (e unavailableError) ErrorCode() string { return "TASK_SERVICE_UNAVAILABLE" }

type invalidQueryError string

func (e invalidQueryError) Error() string     { return string(e) }
func (e invalidQueryError) HTTPStatus() int   { return http.StatusBadRequest }
func (e invalidQueryError) ErrorCode() string { return "INVALID_TASK_QUERY" }

func ListTasksHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.TaskListReq
		if err := httpx.Parse(r, &req); err != nil {
			observability.WriteBadRequest(w, r, err)
			return
		}
		if svcCtx == nil || svcCtx.TaskQuery == nil {
			observability.WriteHTTPError(w, r, unavailableError("task query service is unavailable"))
			return
		}
		if req.Status != "" && req.Status != "queued" && req.Status != "running" && req.Status != "succeeded" && req.Status != "failed" && req.Status != "interrupted" {
			observability.WriteHTTPError(w, r, invalidQueryError("invalid task status"))
			return
		}
		if req.Page < 0 || req.Limit < 0 || req.Limit > task.MaximumTaskPageSize {
			observability.WriteHTTPError(w, r, invalidQueryError("invalid task page or limit"))
			return
		}
		page, err := svcCtx.TaskQuery.List(task.TaskFilter{
			Status: task.Status(req.Status), Type: req.Type, ContainerID: req.ContainerID,
			ContainerName: req.ContainerName, Page: req.Page, Limit: req.Limit,
		})
		if err != nil {
			observability.WriteHTTPError(w, r, err)
			return
		}
		observability.WriteAPIResult(w, r, &types.Resp{Code: http.StatusOK, Msg: "success", Data: page}, nil)
	}
}

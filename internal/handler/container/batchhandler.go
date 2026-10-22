package container

import (
	"net/http"

	containerLogic "github.com/onlyLTY/dockerCopilot/internal/logic/container"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
)

func BatchHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req containerLogic.BatchRequest
		if err := httpx.Parse(r, &req); err != nil {
			observability.WriteBadRequest(w, r, err)
			return
		}
		taskID, err := containerLogic.NewBatchLogic(r.Context(), svcCtx).Submit(&req)
		if err != nil {
			observability.WriteAPIResult(w, r, nil, err)
			return
		}
		observability.WriteAPIResult(w, r, &types.Resp{
			Code: http.StatusOK, Msg: "batch task accepted",
			Data: map[string]any{"taskID": taskID, "status": "queued"},
		}, nil)
	}
}

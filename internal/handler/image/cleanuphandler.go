package image

import (
	"net/http"

	imageLogic "github.com/onlyLTY/dockerCopilot/internal/logic/image"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

func CleanupHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req imageLogic.CleanupRequest
		if err := httpx.Parse(r, &req); err != nil {
			observability.WriteBadRequest(w, r, err)
			return
		}
		resp, err := imageLogic.NewCleanupLogic(r.Context(), svcCtx).Submit(&req)
		observability.WriteAPIResult(w, r, resp, err)
	}
}

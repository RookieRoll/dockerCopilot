package auth

import (
	"github.com/onlyLTY/dockerCopilot/internal/logic/auth"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
	"net/http"
)

func LoginHandler(ctx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.LoginReq
		if err := httpx.Parse(r, &req); err != nil {
			observability.WriteBadRequest(w, r, err)
			return
		}
		l := auth.NewLoginLogic(r.Context(), ctx)
		resp, err := l.Login(&req)
		observability.WriteAPIResult(w, r, resp, err)
	}
}

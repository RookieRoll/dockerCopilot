package container

import (
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/logic/container"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func ListBackupsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := container.NewListBackupsLogic(r.Context(), svcCtx)
		resp, err := l.ListBackups()
		observability.WriteAPIResult(w, r, resp, err)
	}
}

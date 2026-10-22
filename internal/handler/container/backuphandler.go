package container

import (
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/logic/container"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func BackupHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := container.NewBackupLogic(r.Context(), svcCtx)
		resp, err := l.Backup()
		observability.WriteAPIResult(w, r, resp, err)
	}
}

func BackupTaskHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := container.NewBackupLogic(r.Context(), svcCtx)
		resp, err := l.Submit()
		observability.WriteAPIResult(w, r, resp, err)
	}
}

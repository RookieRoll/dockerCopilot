package handler

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/zeromicro/go-zero/rest"
)

func mutationGateMiddleware(svcCtx *svc.ServiceContext) rest.Middleware {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !isSynchronousMutation(r.Method, r.URL.Path) {
				next(w, r)
				return
			}
			lease, err := svcCtx.MutationGate().Acquire(r.Context(), "http:"+uuid.NewString())
			if err != nil {
				observability.WriteHTTPError(w, r, err)
				return
			}
			defer lease.Release()
			next(w, r)
		}
	}
}

func isSynchronousMutation(method, path string) bool {
	path = strings.TrimSuffix(path, "/")
	if method == http.MethodGet {
		return path == "/api/container/backup" || path == "/api/container/backup2compose"
	}
	if method == http.MethodPut {
		return (strings.HasPrefix(path, "/api/container/") && strings.HasSuffix(path, "/configured-ports")) || path == "/api/program"
	}
	if method == http.MethodDelete {
		return path == "/api/container/backups" || strings.HasPrefix(path, "/api/image/")
	}
	if method == http.MethodPost {
		if path == "/api/icons" {
			return true
		}
		if strings.HasPrefix(path, "/api/container/") && (strings.HasSuffix(path, "/start") || strings.HasSuffix(path, "/stop") || strings.HasSuffix(path, "/restart") || strings.HasSuffix(path, "/rename")) {
			return true
		}
	}
	return false
}

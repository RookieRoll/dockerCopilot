package image

import (
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/logic/image"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func ImagesListHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := image.NewImagesListLogic(r.Context(), svcCtx)
		resp, err := l.ImagesList()
		observability.WriteAPIResult(w, r, resp, err)
	}
}

package version

import (
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/logic/version"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func UpdateProgramHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := programUpdateRequestID(r.Header.Get("X-Request-ID"))
		responseRequest := r.Clone(r.Context())
		responseRequest.Header = r.Header.Clone()
		responseRequest.Header.Set("X-Request-ID", requestID)
		l := version.NewUpdateProgramLogic(r.Context(), svcCtx)
		l.SetRequestID(requestID)
		resp, err := l.UpdateProgram()
		observability.WriteAPIResult(w, responseRequest, resp, err)
	}
}

func programUpdateRequestID(header string) string {
	return observability.NormalizeRequestID(header)
}

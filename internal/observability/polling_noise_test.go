package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx/logtest"
)

func TestSuccessfulTaskPollingResponsesDoNotEmitErrorLogs(t *testing.T) {
	collector := logtest.NewCollector(t)
	for range 5 {
		req := httptest.NewRequest(http.MethodGet, "/api/progress/task-id", nil)
		response := httptest.NewRecorder()
		WriteAPIResult(response, req, &types.Resp{Code: http.StatusOK, Msg: "success", Data: map[string]any{"status": "running"}}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("poll response status = %d", response.Code)
		}
	}
	if logs := strings.TrimSpace(collector.String()); logs != "" {
		t.Fatalf("successful polling generated noisy error logs: %s", logs)
	}
}

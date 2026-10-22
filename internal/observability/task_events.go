package observability

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
)

// LogTaskEvent emits a structured lifecycle event with stable task and
// request correlation fields. Callers pass only identifiers and event names.
func LogTaskEvent(ctx context.Context, event, taskID, requestID string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if requestID == "" {
		requestID = RequestIDFromContext(ctx)
	}
	logx.WithContext(ctx).Infow("task lifecycle",
		logx.Field("event", event),
		logx.Field("taskID", taskID),
		logx.Field("requestId", requestID),
	)
}

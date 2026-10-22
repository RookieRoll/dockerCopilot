package observability

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
)

// LogTaskEvent emits a structured lifecycle event with stable task and
// request correlation fields. Callers pass only identifiers and event names.
func LogTaskEvent(ctx context.Context, event, taskID, requestID string, fields ...logx.LogField) {
	if ctx == nil {
		ctx = context.Background()
	}
	if requestID == "" {
		requestID = RequestIDFromContext(ctx)
	}
	base := []logx.LogField{
		logx.Field("event", event),
		logx.Field("taskId", taskID),
		logx.Field("taskID", taskID),
		logx.Field("requestId", requestID),
	}
	severity := lifecycleSeverity(event, fields)
	base = append(base, logx.Field("severity", severity))
	base = append(base, fields...)
	switch severity {
	case "ERROR":
		logx.WithContext(ctx).Errorw("task lifecycle", base...)
	default:
		// go-zero's logger has no WARN level. Keep the event at INFO transport
		// level and encode WARN in the structured severity field.
		logx.WithContext(ctx).Infow("task lifecycle", base...)
	}
}

// LogOperationResult records only the action, resource ID and outcome for a
// short synchronous mutation; errors and Docker payloads stay out of this log.
func LogOperationResult(ctx context.Context, operation, resourceID, result string, cause ...error) {
	if ctx == nil {
		ctx = context.Background()
	}
	fields := []logx.LogField{
		logx.Field("requestId", RequestIDFromContext(ctx)),
		logx.Field("operation", operation),
		logx.Field("resourceID", resourceID),
		logx.Field("result", result),
	}
	if len(cause) > 0 && cause[0] != nil {
		_, _, _, code := classifyHTTPError(cause[0])
		severity := "ERROR"
		if result == "warning" {
			severity = "WARN"
		}
		fields = append(fields, logx.Field("errorCode", code), logx.Field("severity", severity))
		if severity == "ERROR" {
			logx.WithContext(ctx).Errorw("operation result", fields...)
		} else {
			logx.WithContext(ctx).Infow("operation result", fields...)
		}
		return
	}
	severity := "INFO"
	if result == "warning" {
		severity = "WARN"
	}
	fields = append(fields, logx.Field("severity", severity))
	logx.WithContext(ctx).Infow("operation result", fields...)
}

func lifecycleSeverity(event string, fields []logx.LogField) string {
	values := make(map[string]any, len(fields))
	for _, field := range fields {
		values[field.Key] = field.Value
	}
	status, _ := values["status"].(string)
	recoveryStatus, _ := values["recoveryStatus"].(string)
	attention, _ := values["requiresAttention"].(bool)
	switch {
	case event == "task.finished" && status == "failed":
		return "ERROR"
	case event == "task.recovery" && (recoveryStatus == "unknown" || recoveryStatus == "failed" || attention):
		return "WARN"
	case event == "task.finished" && (status == "interrupted" || attention):
		return "WARN"
	default:
		return "INFO"
	}
}

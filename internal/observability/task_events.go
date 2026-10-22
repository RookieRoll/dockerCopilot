package observability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// LogTaskPullProgress emits a throttled, sanitized detail event for long image
// pulls. Callers own throttling so Docker progress does not flood logs.
func LogTaskPullProgress(ctx context.Context, taskID, requestID string, stage string, percentage int, message string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if requestID == "" {
		requestID = RequestIDFromContext(ctx)
	}
	logx.WithContext(ctx).Debugw("task pull progress",
		logx.Field("event", "task.pull_progress"), logx.Field("taskId", taskID),
		logx.Field("taskID", taskID), logx.Field("requestId", requestID),
		logx.Field("severity", "DEBUG"), logx.Field("stage", stage),
		logx.Field("percentage", percentage), logx.Field("message", RedactText(message)))
}

// LogOperationResult records only the action, resource ID and outcome for a
// short synchronous mutation; errors and Docker payloads stay out of this log.
func LogOperationResult(ctx context.Context, operation, resourceID, result string, cause ...error) {
	logOperationResult(ctx, operation, resourceID, result, 0, cause...)
}

// LogOperationResultWithDuration is used by synchronous container mutations
// whose latency is useful during incident diagnosis.
func LogOperationResultWithDuration(ctx context.Context, operation, containerID, result string, duration time.Duration, cause ...error) {
	logOperationResult(ctx, operation, containerID, result, duration, cause...)
}

func logOperationResult(ctx context.Context, operation, resourceID, result string, duration time.Duration, cause ...error) {
	if ctx == nil {
		ctx = context.Background()
	}
	fields := []logx.LogField{
		logx.Field("requestId", RequestIDFromContext(ctx)),
		logx.Field("operation", operation),
		logx.Field("resourceID", resourceID),
		logx.Field("result", result),
	}
	if strings.HasPrefix(operation, "container.") {
		fields = append(fields, logx.Field("containerId", resourceID))
	}
	if duration > 0 {
		fields = append(fields, logx.Field("durationMs", duration.Milliseconds()))
	}
	if len(cause) > 0 && cause[0] != nil {
		code := SafeErrorCode(cause[0])
		severity := "ERROR"
		if result == "warning" {
			severity = "WARN"
		}
		fields = append(fields, logx.Field("errorCode", code), logx.Field("error", operationErrorText(cause[0])), logx.Field("severity", severity))
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

func operationErrorText(err error) string {
	var parts []string
	seen := make(map[string]struct{})
	for depth := 0; err != nil && depth < 8; depth++ {
		part := strings.TrimSpace(err.Error())
		if _, exists := seen[part]; !exists {
			seen[part] = struct{}{}
			parts = append(parts, part)
		}
		err = errors.Unwrap(err)
	}
	text := RedactText(strings.Join(parts, ": "))
	textRunes := []rune(text)
	if len(textRunes) > 1024 {
		text = fmt.Sprintf("%s…", string(textRunes[:1024]))
	}
	return text
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
	case event == "task.recovery" && recoveryStatus == "failed":
		return "ERROR"
	case event == "task.recovery" && (recoveryStatus == "unknown" || attention):
		return "WARN"
	case event == "task.finished" && numericField(values["warningCount"]) > 0:
		return "WARN"
	case event == "task.finished" && (status == "interrupted" || attention):
		return "WARN"
	default:
		return "INFO"
	}
}

func numericField(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	default:
		return 0
	}
}

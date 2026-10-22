package observability

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type requestIDKey struct{}

// RequestIDMiddleware gives every request a canonical UUID correlation ID.
// Invalid client values are replaced before they can enter task records/logs.
func RequestIDMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := NormalizeRequestID(r.Header.Get("X-Request-ID"))
		w.Header().Set("X-Request-ID", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		next(w, r)
	}
}

// NormalizeRequestID accepts UUID values only and always returns canonical
// lowercase text, generating a new ID for empty or malformed input.
func NormalizeRequestID(value string) string {
	if id, err := uuid.Parse(strings.TrimSpace(value)); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// RequestIDFromContext returns the canonical ID installed by the HTTP
// middleware, or creates one for direct logic callers outside an HTTP request.
func RequestIDFromContext(ctx context.Context) string {
	if ctx != nil {
		if id, ok := ctx.Value(requestIDKey{}).(string); ok && uuid.Validate(id) == nil {
			return id
		}
	}
	return uuid.NewString()
}

func requestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	if uuid.Validate(id) == nil {
		return id
	}
	return ""
}

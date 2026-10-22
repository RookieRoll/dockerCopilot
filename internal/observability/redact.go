package observability

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

var (
	urlPattern                  = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
	urlUserInfoPattern          = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)
	bearerPattern               = regexp.MustCompile(`(?i)(\bbearer\s+)[^\s,;"'<>]+`)
	basicPattern                = regexp.MustCompile(`(?i)(\bbasic\s+)[^\s,;"'<>]+`)
	authorizationPattern        = regexp.MustCompile(`(?i)(\b(?:proxy-)?authorization["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|(?:bearer|basic)\s+[^\s,;"']+|[^\s,;"']+)`)
	cookiePattern               = regexp.MustCompile(`(?i)(\b(?:cookie|set-cookie)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\r\n,"'}]+)`)
	secretAssignmentPattern     = regexp.MustCompile(`(?i)(\b(?:secret(?:key)?|token|identity[_-]?token|access[_-]?token|refresh[_-]?token|password|passwd|credential(?:s)?|api[_-]?key|auths?|auth|cookie|set-cookie|docker[_-]?auth[_-]?config|registry[_-]?(?:auth|credential|password|token)|client[_-]?secret|private[_-]?key)\b["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}&]+)`)
	sensitiveFragmentKeyPattern = regexp.MustCompile(`(?i)["']?(env|environment|inspect|backup|requestbody|responsebody|httprequest|request|body)["']?\s*:\s*`)
)

type redactingTextWriter struct {
	dst io.Writer
	mu  sync.Mutex
}

// NewRedactingTextWriter wraps legacy plain-text diagnostic sinks that do not
// use logx, such as task runner output, with the same text redactor.
func NewRedactingTextWriter(dst io.Writer) io.Writer {
	return &redactingTextWriter{dst: dst}
}

func (w *redactingTextWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dst == nil || len(p) == 0 {
		return len(p), nil
	}
	safe := []byte(RedactText(string(p)))
	n, err := w.dst.Write(safe)
	if err != nil {
		return 0, err
	}
	if n < len(safe) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

// RedactLogJSON sanitizes a structured log record before it is sent to any
// output. Invalid JSON is treated as unstructured text and sanitized too.
func RedactLogJSON(line []byte) []byte {
	var value any
	if err := json.Unmarshal(bytes.TrimSpace(line), &value); err != nil {
		return []byte(RedactText(string(line)))
	}
	encoded, err := json.Marshal(redactValue(value))
	if err != nil {
		return []byte(RedactText(string(line)))
	}
	return append(encoded, '\n')
}

// RedactText handles error strings, HTTP dumps, panic stacks, and embedded
// JSON without removing useful network or daemon error context.
func RedactText(text string) string {
	trimmed := strings.TrimSpace(text)
	if json.Valid([]byte(trimmed)) {
		var value any
		if json.Unmarshal([]byte(trimmed), &value) == nil {
			if encoded, err := json.Marshal(redactValue(value)); err == nil {
				return string(encoded)
			}
		}
	}
	text = redactPlainText(text)
	for range 2 {
		text = redactEncodedSegments(text)
	}
	return text
}

func redactPlainText(text string) string {
	text = urlPattern.ReplaceAllStringFunc(text, redactURLToken)
	text = urlUserInfoPattern.ReplaceAllString(text, `${1}[REDACTED]@`)
	text = bearerPattern.ReplaceAllString(text, `${1}[REDACTED]`)
	text = basicPattern.ReplaceAllString(text, `${1}[REDACTED]`)
	text = authorizationPattern.ReplaceAllString(text, `${1}[REDACTED]`)
	text = cookiePattern.ReplaceAllString(text, `${1}[REDACTED]`)
	text = redactSensitiveFragments(text)
	text = secretAssignmentPattern.ReplaceAllString(text, `${1}[REDACTED]`)
	return text
}

func redactSensitiveFragments(text string) string {
	matches := sensitiveFragmentKeyPattern.FindAllStringIndex(text, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		valueStart := matches[i][1]
		valueEnd := scanJSONValue(text, valueStart)
		if valueEnd <= valueStart {
			continue
		}
		text = text[:valueStart] + `"[REDACTED]"` + text[valueEnd:]
	}
	return text
}

func scanJSONValue(text string, start int) int {
	for start < len(text) && (text[start] == ' ' || text[start] == '\t') {
		start++
	}
	if start >= len(text) {
		return start
	}
	if text[start] == '"' {
		escaped := false
		for i := start + 1; i < len(text); i++ {
			if escaped {
				escaped = false
				continue
			}
			if text[i] == '\\' {
				escaped = true
			} else if text[i] == '"' {
				return i + 1
			}
		}
		return len(text)
	}
	if text[start] != '[' && text[start] != '{' {
		end := start
		for end < len(text) && text[end] != ',' && text[end] != '}' && text[end] != '\n' {
			end++
		}
		return end
	}
	stack := []byte{text[start]}
	quoted, escaped := false, false
	for i := start + 1; i < len(text); i++ {
		ch := text[i]
		if quoted {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
			}
			continue
		}
		switch ch {
		case '"':
			quoted = true
		case '[', '{':
			stack = append(stack, ch)
		case ']', '}':
			if len(stack) == 0 {
				return i
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1
			}
		}
	}
	return len(text)
}

func redactEncodedSegments(text string) string {
	clean := text
	for _, field := range strings.Fields(text) {
		if !strings.Contains(field, "%") {
			continue
		}
		decoded, err := url.QueryUnescape(field)
		if err != nil || decoded == field || !hasSensitiveTextContext(decoded) && !strings.Contains(strings.ToLower(decoded), "https://") && !strings.Contains(strings.ToLower(decoded), "http://") {
			continue
		}
		clean = strings.Replace(clean, field, redactPlainText(decoded), 1)
	}
	return clean
}

func hasSensitiveTextContext(text string) bool {
	return secretAssignmentPattern.MatchString(text) || authorizationPattern.MatchString(text) || cookiePattern.MatchString(text) || bearerPattern.MatchString(text) || basicPattern.MatchString(text)
}

func redactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(v))
		for key, item := range v {
			if isSensitiveContainerKey(key) || isSensitiveKey(key) {
				clean[key] = "[REDACTED]"
			} else {
				clean[key] = redactValue(item)
			}
		}
		return clean
	case []any:
		clean := make([]any, len(v))
		for i := range v {
			clean[i] = redactValue(v[i])
		}
		return clean
	case string:
		clean := RedactText(v)
		// JSON frequently appears inside a quoted log message or wrapped error.
		// Decode one level when the whole string is itself JSON, then recurse.
		trimmed := strings.TrimSpace(clean)
		if json.Valid([]byte(trimmed)) {
			var nested any
			if json.Unmarshal([]byte(trimmed), &nested) == nil {
				if nestedText, ok := nested.(string); ok && nestedText != v {
					return redactValue(nestedText)
				}
				if _, isMap := nested.(map[string]any); isMap {
					if encoded, err := json.Marshal(redactValue(nested)); err == nil {
						return string(encoded)
					}
				}
				if _, isArray := nested.([]any); isArray {
					if encoded, err := json.Marshal(redactValue(nested)); err == nil {
						return string(encoded)
					}
				}
			}
		}
		return clean
	default:
		return value
	}
}

func normalizeKey(key string) string {
	key = strings.ToLower(key)
	decoded, err := url.QueryUnescape(key)
	if err == nil {
		key = strings.ToLower(decoded)
	}
	var normalized strings.Builder
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			normalized.WriteRune(r)
		}
	}
	return normalized.String()
}

func isSensitiveKey(key string) bool {
	n := normalizeKey(key)
	for _, marker := range []string{"secret", "token", "password", "passwd", "credential", "apikey", "authorization", "cookie", "privatekey", "accesskey"} {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return n == "auth" || n == "auths" || strings.HasSuffix(n, "auth")
}

func isSensitiveContainerKey(key string) bool {
	n := normalizeKey(key)
	return n == "env" || strings.Contains(n, "environment") || strings.Contains(n, "inspect") || strings.Contains(n, "backup") || n == "requestbody" || n == "responsebody" || n == "httprequest" || n == "request" || n == "body"
}

func redactURLToken(token string) string {
	trimmed := strings.TrimRight(token, ".,;:!?)]}\\")
	suffix := token[len(trimmed):]
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return RedactTextNonURL(token)
	}
	if u.User != nil {
		u.User = url.User("[REDACTED]")
	}
	if u.RawQuery != "" {
		parts := strings.Split(u.RawQuery, "&")
		for i, part := range parts {
			key, _, hasValue := strings.Cut(part, "=")
			decoded, decodeErr := url.QueryUnescape(key)
			if decodeErr == nil && isSensitiveKey(decoded) && hasValue {
				parts[i] = key + "=%5BREDACTED%5D"
			} else if decodeErr != nil && hasValue && isSensitiveKey(key) {
				parts[i] = key + "=%5BREDACTED%5D"
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	return u.String() + suffix
}

func RedactTextNonURL(text string) string {
	text = authorizationPattern.ReplaceAllString(text, `${1}[REDACTED]`)
	text = cookiePattern.ReplaceAllString(text, `${1}[REDACTED]`)
	return secretAssignmentPattern.ReplaceAllString(text, `${1}[REDACTED]`)
}

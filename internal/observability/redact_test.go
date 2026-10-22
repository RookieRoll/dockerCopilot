package observability

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRedactLogJSONRemovesSensitiveFieldsAndNestedContent(t *testing.T) {
	line := []byte(`{"level":"error","content":"Get https://alice:p%40ss@example.test/api?access_token=URLSECRET failed: TLS handshake","authorization":"Bearer HEADERSECRET","details":{"secretKey":"CONFIGSECRET","nested":"{\"password\":\"JSONSECRET\",\"cause\":\"daemon refused connection\"}"},"Env":["DB_PASSWORD=ENVSECRET"],"inspect":{"Config":{"Env":["FULLINSPECTSECRET"]}},"backup":{"Config":"BACKUPSECRET"}}`)
	got := string(RedactLogJSON(line))
	for _, secret := range []string{"p%40ss", "URLSECRET", "HEADERSECRET", "CONFIGSECRET", "JSONSECRET", "ENVSECRET", "FULLINSPECTSECRET", "BACKUPSECRET"} {
		if strings.Contains(got, secret) {
			t.Errorf("output leaks %q: %s", secret, got)
		}
	}
	for _, useful := range []string{"example.test", "TLS handshake", "daemon refused connection", "[REDACTED]"} {
		if !strings.Contains(got, useful) {
			t.Errorf("output lost useful context %q: %s", useful, got)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("redacted record is not valid JSON: %v", err)
	}
}

func TestRedactTextHandlesHeadersQueriesNestedEncodingAndMalformedURL(t *testing.T) {
	input := "Authorization: Bearer abc123\nCookie: session=secret-cookie\nrequest https://user:pass@registry.test/v2?%61ccess%5Ftoken=qp%3Dsecret and token%3Ddouble%2520secret; cause=dns lookup failed"
	got := RedactText(input)
	for _, secret := range []string{"abc123", "secret-cookie", "user:pass", "qp%3Dsecret"} {
		if strings.Contains(got, secret) {
			t.Errorf("output leaks %q: %s", secret, got)
		}
	}
	for _, useful := range []string{"registry.test", "dns lookup failed"} {
		if !strings.Contains(got, useful) {
			t.Errorf("output lost useful context %q: %s", useful, got)
		}
	}
	malformed := RedactText("https://registry.test/v2?token=hidden%ZZ&x=1")
	if strings.Contains(malformed, "hidden") || !strings.Contains(malformed, "registry.test") {
		t.Fatalf("malformed query was not safely cleaned: %q", malformed)
	}
	panicText := RedactText("panic: Bearer fake-jwt")
	if strings.Contains(panicText, "fake-jwt") || !strings.Contains(panicText, "panic:") {
		t.Fatalf("bare bearer token was not redacted: %q", panicText)
	}
	invalidURL := RedactText(`Get "http://user:pass@host/%zz": invalid URL escape`)
	if strings.Contains(invalidURL, "user:pass") || !strings.Contains(invalidURL, "host") || !strings.Contains(invalidURL, "invalid URL escape") {
		t.Fatalf("malformed URL lost cause or leaked userinfo: %q", invalidURL)
	}
	wrappedEnv := RedactText(`daemon: {"Env":["CUSTOM_ENV=sensitive"],"Image":"nginx"}`)
	if strings.Contains(wrappedEnv, "CUSTOM_ENV=sensitive") || !strings.Contains(wrappedEnv, "nginx") {
		t.Fatalf("embedded env data was not redacted or safe field lost: %q", wrappedEnv)
	}
}

func TestRedactTextCleansEncodedAuthenticationContextsOnly(t *testing.T) {
	input := "Authorization%3A%20Bearer%20encoded-secret Bearer%20bearer-secret Cookie%3A%20session%3Dcookie-secret ordinary%20text"
	got := RedactText(input)
	for _, secret := range []string{"encoded-secret", "bearer-secret", "cookie-secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("output leaks encoded credential %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "ordinary%20text") {
		t.Errorf("ordinary URL-encoded text changed: %s", got)
	}
}

func TestRedactingTextWriterPropagatesWriteFailures(t *testing.T) {
	wantErr := errors.New("disk unavailable")
	writer := NewRedactingTextWriter(failingWriter{err: wantErr})
	if n, err := writer.Write([]byte("diagnostic")); n != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("writer error=(%d,%v), want (0,%v)", n, err, wantErr)
	}
	writer = NewRedactingTextWriter(failingWriter{n: 1})
	if n, err := writer.Write([]byte("diagnostic")); n != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write=(%d,%v), want (0,%v)", n, err, io.ErrShortWrite)
	}
}

type failingWriter struct {
	n   int
	err error
}

func (w failingWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return w.n, w.err
	}
	return w.n, nil
}

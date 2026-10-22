package observability

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/types"
	zeroxerrors "github.com/zeromicro/x/errors"
)

func TestWriteHTTPErrorKeepsBusinessCodeOutOfStatusLine(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	request.Header.Set("X-Request-ID", "req-123")
	WriteHTTPError(recorder, request, &zeroxerrors.CodeMsg{Code: 50001, Msg: "remote lookup failed"})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("HTTP status = %d, want 500", recorder.Code)
	}
	var got HTTPErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != 50001 || got.Msg != "remote lookup failed" || got.RequestID != "req-123" || got.ErrorCode != "BUSINESS_ERROR" {
		t.Fatalf("unexpected compatible error envelope: %#v", got)
	}
}

func TestWriteAPIResultNeverUsesBusinessCodeAsHTTPStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteAPIResult(recorder, httptest.NewRequest(http.MethodGet, "/version", nil), &types.Resp{Code: 50001, Msg: "lookup failed", Data: map[string]any{}}, &zeroxerrors.CodeMsg{Code: 50001, Msg: "lookup failed"})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("HTTP status = %d, want 500", recorder.Code)
	}
}

func TestWriteAPIResultMapsLegacyBodyErrorCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		want int
	}{{"business-server-code", 50001, http.StatusInternalServerError}, {"bad-request", 400, http.StatusBadRequest}} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			WriteAPIResult(recorder, httptest.NewRequest(http.MethodGet, "/", nil), &types.Resp{Code: test.code, Msg: "failed", Data: map[string]any{}}, nil)
			if recorder.Code != test.want {
				t.Fatalf("HTTP status = %d, want %d", recorder.Code, test.want)
			}
		})
	}
}

func TestWriteAPIResultKeepsFailedTaskQuerySuccessful(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteAPIResult(recorder, httptest.NewRequest(http.MethodGet, "/api/tasks/id", nil), &types.Resp{Code: 200, Msg: "failed task is data", Data: map[string]any{"status": "failed"}}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("failed task query HTTP status = %d, want 200", recorder.Code)
	}
}

func TestWriteAPIResultNilResponseWithNilErrorIsFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteAPIResult(recorder, httptest.NewRequest(http.MethodGet, "/", nil), nil, nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("HTTP status = %d, want 500", recorder.Code)
	}
}

func TestWriteHTTPErrorHandlesNilAndIllegalBusinessCode(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteHTTPError(recorder, httptest.NewRequest(http.MethodGet, "/", nil), nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("nil error HTTP status = %d, want 500", recorder.Code)
	}
	var got HTTPErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ErrorCode != "INTERNAL_ERROR" || got.RequestID == "" {
		t.Fatalf("nil error did not produce diagnostic envelope: %#v", got)
	}
}

package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
	zeroxerrors "github.com/zeromicro/x/errors"
)

// HTTPErrorResponse preserves the legacy code/msg/data envelope while adding
// stable machine-readable classification and request correlation fields.
type HTTPErrorResponse struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	Data      any    `json:"data"`
	ErrorCode string `json:"errorCode"`
	RequestID string `json:"requestId"`
}

// ClassifiedError lets application layers provide protocol classification
// without making this HTTP package depend on task or business packages.
type ClassifiedError interface {
	error
	HTTPStatus() int
	ErrorCode() string
}

type classifiedHTTPError struct {
	status int
	code   string
	cause  error
}

func (e classifiedHTTPError) Error() string     { return e.cause.Error() }
func (e classifiedHTTPError) Unwrap() error     { return e.cause }
func (e classifiedHTTPError) HTTPStatus() int  { return e.status }
func (e classifiedHTTPError) ErrorCode() string { return e.code }

func BadRequest(err error) error {
	if err == nil {
		err = errors.New("invalid request")
	}
	return classifiedHTTPError{status: http.StatusBadRequest, code: "INVALID_REQUEST", cause: err}
}

func ServiceUnavailable(err error) error {
	if err == nil {
		err = errors.New("service dependency is unavailable")
	}
	return classifiedHTTPError{status: http.StatusServiceUnavailable, code: "DEPENDENCY_UNAVAILABLE", cause: err}
}

// WriteAPIResult preserves the existing application envelope while ensuring
// its business code is never used as an HTTP status. A nil response without an
// error is treated as an internal failure instead of writing an empty success.
func WriteAPIResult(w http.ResponseWriter, r *http.Request, response *types.Resp, err error) {
	responseOnlyError := err == nil && response != nil && (response.Code < 200 || response.Code >= 300)
	if err == nil && response == nil {
		err = errors.New("handler returned an empty response")
	}
	if err == nil && response != nil && response.Code >= 200 && response.Code < 300 {
		writeJSON(w, http.StatusOK, response)
		return
	}
	if err == nil && response != nil {
		msg := response.Msg
		if msg == "" {
			msg = "request failed"
		}
		err = errors.New(msg)
	}
	if err != nil {
		if response == nil {
			WriteHTTPError(w, r, err)
			return
		}
		status, defaultCode, defaultMessage, errorCode := classifyHTTPError(err)
		if responseOnlyError {
			status = legalHTTPStatus(response.Code)
			if status >= 400 && status < 500 {
				errorCode = "REQUEST_REJECTED"
			} else {
				errorCode = "BUSINESS_ERROR"
			}
		}
		requestID := requestID(r)
		w.Header().Set("X-Request-ID", requestID)
		code, message, data := defaultCode, defaultMessage, response.Data
		if response.Code != 0 {
			code = response.Code
		}
		if response.Msg != "" {
			message = response.Msg
		}
		logx.Errorf("http request failed requestId=%s errorCode=%s status=%d error=%s", requestID, errorCode, status, RedactText(err.Error()))
		writeJSON(w, status, HTTPErrorResponse{Code: code, Msg: RedactText(message), Data: data, ErrorCode: errorCode, RequestID: requestID})
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func WriteHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	status, response := HTTPError(err, r)
	w.Header().Set("X-Request-ID", response.RequestID)
	writeJSON(w, status, response)
}

func WriteBadRequest(w http.ResponseWriter, r *http.Request, err error) {
	WriteHTTPError(w, r, BadRequest(err))
}

// HTTPError returns a legal HTTP status and compatible error envelope. It is
// also used by go-zero's process-wide error callback, which has no Request.
func HTTPError(err error, r *http.Request) (int, HTTPErrorResponse) {
	if err == nil {
		err = errors.New("request failed")
	}
	status, code, msg, errorCode := classifyHTTPError(err)
	id := requestID(r)
	logx.Errorf("http request failed requestId=%s errorCode=%s status=%d error=%s", id, errorCode, status, RedactText(err.Error()))
	return status, HTTPErrorResponse{Code: code, Msg: RedactText(msg), Data: map[string]any{}, ErrorCode: errorCode, RequestID: id}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func classifyHTTPError(err error) (status, bodyCode int, message, errorCode string) {
	if err == nil {
		err = errors.New("request failed")
	}
	var classified ClassifiedError
	if errors.As(err, &classified) {
		status = legalHTTPStatus(classified.HTTPStatus())
		errorCode = strings.TrimSpace(classified.ErrorCode())
		if errorCode == "" {
			errorCode = "INTERNAL_ERROR"
		}
		return status, status, err.Error(), errorCode
	}
	var codeMsg *zeroxerrors.CodeMsg
	if errors.As(err, &codeMsg) && codeMsg != nil {
		bodyCode = codeMsg.Code
		message = codeMsg.Msg
		status = legalHTTPStatus(codeMsg.Code)
		if status >= 400 && status < 500 {
			return status, bodyCode, message, "REQUEST_REJECTED"
		}
		return status, bodyCode, message, "BUSINESS_ERROR"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, http.StatusGatewayTimeout, err.Error(), "TIMEOUT"
	}
	if errors.Is(err, os.ErrNotExist) {
		return http.StatusNotFound, http.StatusNotFound, err.Error(), "NOT_FOUND"
	}
	if cerrdefs.IsNotFound(err) {
		return http.StatusNotFound, http.StatusNotFound, err.Error(), "NOT_FOUND"
	}
	if cerrdefs.IsConflict(err) {
		return http.StatusConflict, http.StatusConflict, err.Error(), "CONFLICT"
	}
	if cerrdefs.IsUnavailable(err) {
		return http.StatusServiceUnavailable, http.StatusServiceUnavailable, err.Error(), "DEPENDENCY_UNAVAILABLE"
	}
	if cerrdefs.IsUnauthorized(err) {
		return http.StatusUnauthorized, http.StatusUnauthorized, err.Error(), "UNAUTHORIZED"
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		if networkErr.Timeout() {
			return http.StatusGatewayTimeout, http.StatusGatewayTimeout, err.Error(), "TIMEOUT"
		}
		return http.StatusServiceUnavailable, http.StatusServiceUnavailable, err.Error(), "DEPENDENCY_UNAVAILABLE"
	}
	return http.StatusInternalServerError, http.StatusInternalServerError, err.Error(), "INTERNAL_ERROR"
}

func legalHTTPStatus(value int) int {
	if value >= 400 && value <= 599 {
		return value
	}
	return http.StatusInternalServerError
}

func requestID(r *http.Request) string {
	if r != nil {
		value := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if len(value) <= 128 && value != "" {
			valid := true
			for _, char := range value {
				if char < 0x21 || char > 0x7e {
					valid = false
					break
				}
			}
			if valid {
				return value
			}
		}
	}
	return uuid.NewString()
}

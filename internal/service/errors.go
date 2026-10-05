package service

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/translate"
)

// StatusError is returned to the host as a plugin error envelope with http_status.
type StatusError struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
}

func (e *StatusError) Error() string { return e.Message }

// StatusCode implements the host's status-coder convention.
func (e *StatusError) StatusCode() int { return e.HTTPStatus }

func statusErrorf(status int, code, format string, args ...any) *StatusError {
	return &StatusError{Code: code, Message: fmt.Sprintf(format, args...), HTTPStatus: status}
}

// classifyUpstream maps transport and Ollama failures to client-facing statuses.
func classifyUpstream(model string, err error) *StatusError {
	var se *StatusError
	if errors.As(err, &se) {
		return se
	}
	var re *translate.RequestError
	if errors.As(err, &re) {
		return &StatusError{Code: "invalid_request_error", Message: re.Message, HTTPStatus: http.StatusBadRequest}
	}
	var he *ollama.HTTPError
	if errors.As(err, &he) {
		return fromOllamaStatus(model, he.StatusCode, he.Message)
	}
	var ue *translate.UpstreamError
	if errors.As(err, &ue) {
		return &StatusError{Code: "upstream_error", Message: fmt.Sprintf("Ollama failed while serving %s: %s", model, ue.Message), HTTPStatus: http.StatusBadGateway, Retryable: true}
	}
	return &StatusError{Code: "upstream_unreachable", Message: fmt.Sprintf("Ollama request for %s failed: %v", model, err), HTTPStatus: http.StatusBadGateway, Retryable: true}
}

func fromOllamaStatus(model string, status int, message string) *StatusError {
	if message == "" {
		message = http.StatusText(status)
	}
	switch {
	case status == http.StatusBadRequest:
		return &StatusError{Code: "invalid_request_error", Message: "Ollama rejected the request for " + model + ": " + message, HTTPStatus: http.StatusBadRequest}
	case status == http.StatusNotFound:
		return &StatusError{Code: "model_not_found", Message: "Ollama does not have model " + model + ": " + message, HTTPStatus: http.StatusNotFound}
	case status == http.StatusTooManyRequests:
		return &StatusError{Code: "rate_limit_exceeded", Message: "Ollama is busy: " + message, HTTPStatus: http.StatusTooManyRequests, Retryable: true}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &StatusError{Code: "upstream_auth_failed", Message: fmt.Sprintf("Ollama rejected the plugin's credentials (HTTP %d): %s", status, message), HTTPStatus: http.StatusBadGateway}
	case status >= 500:
		return &StatusError{Code: "upstream_error", Message: fmt.Sprintf("Ollama failed to serve %s (HTTP %d): %s", model, status, message), HTTPStatus: http.StatusBadGateway, Retryable: true}
	default:
		return &StatusError{Code: "upstream_error", Message: fmt.Sprintf("Ollama returned HTTP %d for %s: %s", status, model, message), HTTPStatus: http.StatusBadGateway}
	}
}

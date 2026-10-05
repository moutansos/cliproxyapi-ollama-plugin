package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/upstream"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// hostBridge implements service.Host with CLIProxyAPI host callbacks, so all
// upstream traffic follows host transport policy and request logging.
type hostBridge struct{}

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	OperationID    string      `json:"operation_id,omitempty"`
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

// hostHTTPResponse matches pluginapi.HTTPResponse (no JSON tags on the host side).
type hostHTTPResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type hostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id"`
}

type hostStreamIDRequest struct {
	StreamID string `json:"stream_id"`
}

type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type hostOperationOpenRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostOperationOpenResponse struct {
	OperationID string `json:"operation_id"`
}

type hostOperationCancelRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
	OperationID    string `json:"operation_id"`
}

type hostStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type hostStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type hostLogRequest struct {
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`
}

func callHost(method string, payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	out, rc := rawHostCall(method, raw)
	if len(out) == 0 {
		if rc != 0 {
			return nil, fmt.Errorf("host callback %s failed (rc=%d)", method, rc)
		}
		return nil, nil
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("decode host callback %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("host callback %s: %s", method, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host callback %s failed (rc=%d)", method, rc)
	}
	return env.Result, nil
}

// withOperation makes a host HTTP call cancelable when ctx can be cancelled.
func withOperation(ctx context.Context, callbackID string, call func(operationID string) error) error {
	if ctx == nil || ctx.Done() == nil {
		return call("")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := callHost(pluginabi.MethodHostHTTPOperationOpen, hostOperationOpenRequest{HostCallbackID: callbackID})
	if err != nil {
		return call("")
	}
	var open hostOperationOpenResponse
	if err := json.Unmarshal(raw, &open); err != nil || strings.TrimSpace(open.OperationID) == "" {
		return call("")
	}
	finished := make(chan struct{})
	go func() {
		defer func() { _ = recover() }()
		select {
		case <-ctx.Done():
			_, _ = callHost(pluginabi.MethodHostHTTPCancel, hostOperationCancelRequest{HostCallbackID: callbackID, OperationID: open.OperationID})
		case <-finished:
		}
	}()
	errCall := call(open.OperationID)
	close(finished)
	if errCall != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return errCall
}

func (hostBridge) Do(ctx context.Context, req upstream.Request) (upstream.Response, error) {
	var resp hostHTTPResponse
	err := withOperation(ctx, req.CallbackID, func(operationID string) error {
		raw, err := callHost(pluginabi.MethodHostHTTPDo, hostHTTPRequest{
			HostCallbackID: req.CallbackID,
			OperationID:    operationID,
			Method:         req.Method,
			URL:            req.URL,
			Headers:        req.Headers,
			Body:           req.Body,
		})
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &resp)
	})
	if err != nil {
		return upstream.Response{}, err
	}
	return upstream.Response{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

func (hostBridge) OpenStream(ctx context.Context, req upstream.Request) (upstream.Stream, error) {
	var resp hostHTTPStreamResponse
	err := withOperation(ctx, req.CallbackID, func(operationID string) error {
		raw, err := callHost(pluginabi.MethodHostHTTPDoStream, hostHTTPRequest{
			HostCallbackID: req.CallbackID,
			OperationID:    operationID,
			Method:         req.Method,
			URL:            req.URL,
			Headers:        req.Headers,
			Body:           req.Body,
		})
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &resp)
	})
	if err != nil {
		return upstream.Stream{}, err
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return upstream.Stream{}, fmt.Errorf("host HTTP stream response has no stream_id")
	}
	return upstream.Stream{StatusCode: resp.StatusCode, Headers: resp.Headers, ID: resp.StreamID}, nil
}

func (hostBridge) Read(ctx context.Context, streamID string) ([]byte, bool, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
	}
	raw, err := callHost(pluginabi.MethodHostHTTPStreamRead, hostStreamIDRequest{StreamID: streamID})
	if err != nil {
		return nil, true, err
	}
	var resp hostHTTPStreamReadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, true, fmt.Errorf("decode host stream chunk: %w", err)
	}
	if resp.Error != "" {
		return resp.Payload, true, fmt.Errorf("%s", resp.Error)
	}
	return resp.Payload, resp.Done, nil
}

func (hostBridge) Close(streamID string) {
	_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, hostStreamIDRequest{StreamID: streamID})
}

func (hostBridge) EmitStream(streamID string, payload []byte) error {
	_, err := callHost(pluginabi.MethodHostStreamEmit, hostStreamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func (hostBridge) CloseOutput(streamID, message string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, hostStreamCloseRequest{StreamID: streamID, Error: message})
}

func (hostBridge) Log(level, message string) {
	_, _ = callHost(pluginabi.MethodHostLog, hostLogRequest{Level: level, Message: message})
}

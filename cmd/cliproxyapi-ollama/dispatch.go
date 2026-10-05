package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/service"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const pluginRepository = "https://github.com/moutansos/cliproxyapi-ollama-plugin"

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

// registrationCapability mirrors the host's rpcCapabilities JSON (v8.0.11).
type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	ModelRouter           bool                         `json:"model_router"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
	ResponseInterceptor   bool                         `json:"response_interceptor"`
	ManagementAPI         bool                         `json:"management_api"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcModelRouteRequest struct {
	RequestedModel string `json:"RequestedModel"`
	SourceFormat   string `json:"SourceFormat"`
}

type rpcResponseInterceptRequest struct {
	pluginapi.ResponseInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRegistration struct {
	Routes []pluginapi.ManagementRoute `json:"routes,omitempty"`
}

type streamStartResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

func handleMethod(method string, request []byte) ([]byte, bool) {
	result, err := dispatch(method, request)
	if err != nil {
		var se *service.StatusError
		if errors.As(err, &se) {
			return errorEnvelope(se.Code, se.Message, se.HTTPStatus, se.Retryable), true
		}
		return errorEnvelope("plugin_error", err.Error(), http.StatusInternalServerError, false), true
	}
	raw, err := okEnvelope(result)
	if err != nil {
		return errorEnvelope("encoding_error", err.Error(), http.StatusInternalServerError, false), true
	}
	return raw, false
}

func dispatch(method string, request []byte) (any, error) {
	ctx := context.Background()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
		}
		svc.Configure(req.ConfigYAML)
		return pluginRegistration(), nil
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		svc.Stop(2 * time.Second)
		return struct{}{}, nil
	case pluginabi.MethodModelStatic:
		return svc.StaticModels(), nil
	case pluginabi.MethodModelRoute:
		var req rpcModelRouteRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		return svc.Route(req.RequestedModel), nil
	case pluginabi.MethodExecutorIdentifier:
		return identifierResponse{Identifier: service.Provider}, nil
	case pluginabi.MethodExecutorExecute:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		return svc.Execute(ctx, service.ExecRequest{ExecutorRequest: req.ExecutorRequest, CallbackID: req.HostCallbackID})
	case pluginabi.MethodExecutorExecuteStream:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		headers, err := svc.ExecuteStream(ctx, service.ExecRequest{ExecutorRequest: req.ExecutorRequest, CallbackID: req.HostCallbackID, StreamID: req.StreamID})
		if err != nil {
			return nil, err
		}
		return streamStartResponse{Headers: headers}, nil
	case pluginabi.MethodExecutorCountTokens:
		return nil, svc.CountTokens()
	case pluginabi.MethodExecutorHTTPRequest:
		return nil, &service.StatusError{Code: "not_supported", Message: "raw HTTP passthrough is not supported by the Ollama provider", HTTPStatus: http.StatusNotImplemented}
	case pluginabi.MethodResponseInterceptAfter:
		var req rpcResponseInterceptRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return pluginapi.ResponseInterceptResponse{}, nil
		}
		return svc.InterceptResponse(req.ResponseInterceptRequest), nil
	case pluginabi.MethodManagementRegister:
		return managementRegistration{Routes: svc.ManagementRoutes()}, nil
	case pluginabi.MethodManagementHandle:
		var req rpcManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		return svc.HandleManagement(ctx, req.ManagementRequest), nil
	default:
		return nil, &service.StatusError{Code: "unknown_method", Message: "unknown plugin method: " + method, HTTPStatus: http.StatusNotImplemented}
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Ollama provider",
			Version:          pluginVersion,
			Author:           "moutansos",
			GitHubRepository: pluginRepository,
			ConfigFields:     configFields(),
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeStatic,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
			ResponseInterceptor:   true,
			ManagementAPI:         true,
		},
	}
}

func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Ollama base URL, e.g. http://192.168.1.10:11434 (native API; /v1 or /api suffixes are stripped). Default http://127.0.0.1:11434."},
		{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Optional bearer token sent to Ollama (only needed behind an authenticating proxy)."},
		{Name: "model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Public model ID prefix, CLIProxyAPI-style: IDs are <prefix>/<exact Ollama name:tag> (the slash is added; surrounding slashes are ignored). Default ollama. Empty = no prefix. Requests inside this namespace are routed to Ollama."},
		{Name: "refresh_interval_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Discovery interval (/api/tags, /api/show for new digests, /api/ps). 10-86400, default 60."},
		{Name: "request_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Timeout for each discovery call. 1-300, default 10. Inference is not time-limited."},
		{Name: "default_context_policy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"observe", "managed"}, Description: "Policy for models whose override is absent or 'inherit'. observe (default) injects nothing and advertises observed/configured/fallback context; managed injects num_ctx/num_predict/keep_alive."},
		{Name: "default_num_ctx", Type: pluginapi.ConfigFieldTypeInteger, Description: "Managed num_ctx for models that inherit managed without their own num_ctx. Capped to the model's declared maximum. 0 = unset."},
		{Name: "default_num_predict", Type: pluginapi.ConfigFieldTypeInteger, Description: "Managed output ceiling (num_predict). Smaller client max_tokens values are preserved. 0 = no ceiling."},
		{Name: "default_keep_alive", Type: pluginapi.ConfigFieldTypeString, Description: "Managed keep_alive, e.g. 5m, 30m, or -1 to keep loaded. Empty = Ollama default."},
		{Name: "fallback_context_length", Type: pluginapi.ConfigFieldTypeInteger, Description: "Observe-mode context to advertise when a model is not loaded and has no num_ctx parameter (set it to the server's OLLAMA_CONTEXT_LENGTH). 0 = unknown (omitted)."},
		{Name: "verify_managed_allocation", Type: pluginapi.ConfigFieldTypeBoolean, Description: "After managed requests, check /api/ps and advertise a smaller allocation if Ollama did not honor num_ctx. Default true."},
		{Name: "include_models", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional glob patterns (e.g. [\"qwen3*\"]) of Ollama model names to expose. Empty = all completion-capable models."},
		{Name: "exclude_models", Type: pluginapi.ConfigFieldTypeArray, Description: "Glob patterns of Ollama model names to hide."},
		{Name: "models", Type: pluginapi.ConfigFieldTypeObject, Description: "Per-model overrides keyed by exact Ollama name:tag, e.g. {\"qwen3:8b\": {\"policy\": \"managed\", \"num_ctx\": 8192, \"num_predict\": 2048, \"keep_alive\": \"5m\"}}. Fields: policy (inherit|observe|managed), num_ctx, num_predict, keep_alive, fallback_context_length, display_name."},
	}
}

func okEnvelope(value any) ([]byte, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string, status int, retryable bool) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
			Retryable:  retryable,
		},
	})
	return raw
}

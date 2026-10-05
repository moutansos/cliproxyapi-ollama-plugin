package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/catalog"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/modellist"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Route claims requests for this plugin's models so they execute on its own
// executor without CLIProxyAPI auth selection. Routing reads the same snapshot
// that listing uses, so newly discovered models route immediately.
func (s *Service) Route(requestedModel string) pluginapi.ModelRouteResponse {
	snap := s.Snapshot()
	if !snap.Claims(requestedModel) {
		return pluginapi.ModelRouteResponse{Handled: false}
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "ollama model",
	}
}

// StaticModels returns the snapshot as host model metadata. The host only
// re-reads this at startup and after configuration changes; live changes are
// covered by Route and the /v1/models interceptor.
func (s *Service) StaticModels() pluginapi.ModelResponse {
	if s.staticWaited.CompareAndSwap(false, true) {
		s.WaitReady(s.opts.StaticModelsWait)
	}
	snap := s.Snapshot()
	models := make([]pluginapi.ModelInfo, 0, len(snap.Models))
	for _, m := range snap.Models {
		models = append(models, modelInfo(m))
	}
	return pluginapi.ModelResponse{Provider: Provider, Models: models}
}

func modelInfo(m catalog.Model) pluginapi.ModelInfo {
	input := []string{"text"}
	if m.Vision {
		input = append(input, "image")
	}
	params := []string{"temperature", "top_p", "max_tokens", "stop", "seed", "response_format"}
	if m.Tools {
		params = append(params, "tools", "tool_choice")
	}
	if m.Thinking {
		params = append(params, "reasoning_effort")
	}
	return pluginapi.ModelInfo{
		ID:                        m.ID,
		Object:                    "model",
		Created:                   m.Created,
		OwnedBy:                   Provider,
		Type:                      Provider,
		DisplayName:               m.DisplayName,
		Name:                      m.Upstream,
		Description:               "Ollama model " + m.Upstream,
		ContextLength:             int64(m.Resolution.ContextLength),
		MaxCompletionTokens:       int64(m.Resolution.MaxCompletionTokens),
		SupportedParameters:       params,
		SupportedInputModalities:  input,
		SupportedOutputModalities: []string{"text"},
	}
}

// ListEntries returns /v1/models entries from the current snapshot.
func (s *Service) ListEntries() []modellist.Entry {
	snap := s.Snapshot()
	out := make([]modellist.Entry, 0, len(snap.Models))
	for _, m := range snap.Models {
		out = append(out, modellist.Entry{
			ID:                  m.ID,
			Object:              "model",
			Created:             m.Created,
			OwnedBy:             Provider,
			DisplayName:         m.DisplayName,
			ContextLength:       m.Resolution.ContextLength,
			MaxCompletionTokens: m.Resolution.MaxCompletionTokens,
		})
	}
	return out
}

// owns reports whether an existing /v1/models entry was contributed by this plugin.
func (s *Service) owns(snap *catalog.Snapshot) modellist.OwnedFunc {
	return func(id, ownedBy string) bool {
		if ownedBy != Provider {
			return false
		}
		if _, ok := snap.Lookup(id); ok {
			return true
		}
		prefix := snap.Config.Instance.Namespace()
		return prefix != "" && strings.HasPrefix(id, prefix)
	}
}

// InterceptResponse rewrites only the OpenAI /v1/models list. Every other
// response is returned untouched (empty body means "no change" to the host).
func (s *Service) InterceptResponse(req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	if req.Model != "" || req.RequestedModel != "" || req.Stream || len(req.RequestBody) > 0 || len(req.OriginalRequest) > 0 {
		return pluginapi.ResponseInterceptResponse{}
	}
	if req.SourceFormat != "openai" || (req.StatusCode != 0 && req.StatusCode != http.StatusOK) {
		return pluginapi.ResponseInterceptResponse{}
	}
	snap := s.Snapshot()
	body, ok := modellist.Enrich(req.Body, s.owns(snap), s.ListEntries())
	if !ok {
		return pluginapi.ResponseInterceptResponse{}
	}
	return pluginapi.ResponseInterceptResponse{Body: body}
}

// Management route paths, relative to /v0/management.
const (
	StatusRoutePath  = "/cliproxyapi-ollama/status"
	RefreshRoutePath = "/cliproxyapi-ollama/refresh"
)

// ManagementRoutes lists authenticated diagnostic routes.
func (s *Service) ManagementRoutes() []pluginapi.ManagementRoute {
	return []pluginapi.ManagementRoute{
		{Method: http.MethodGet, Path: StatusRoutePath, Description: "Ollama provider status: discovery, context sources, managed verification."},
		{Method: http.MethodPost, Path: RefreshRoutePath, Description: "Run Ollama discovery now and return the status."},
	}
}

// HandleManagement serves the diagnostic routes.
func (s *Service) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimRight(req.Path, "/")
	switch {
	case strings.HasSuffix(path, RefreshRoutePath) && strings.EqualFold(req.Method, http.MethodPost):
		rctx, cancel := context.WithTimeout(ctx, 2*s.Config().Instance.RequestTimeout+5*time.Second)
		_ = s.RefreshNow(rctx)
		cancel()
		return jsonResponse(http.StatusOK, s.Status())
	case strings.HasSuffix(path, StatusRoutePath):
		return jsonResponse(http.StatusOK, s.Status())
	default:
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"encode_failed"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

// Status is the diagnostics document (contains no secrets).
func (s *Service) Status() map[string]any {
	snap := s.Snapshot()
	state := snap.State
	models := make([]map[string]any, 0, len(snap.Models))
	for _, m := range snap.Models {
		models = append(models, map[string]any{
			"id":           m.ID,
			"upstream":     m.Upstream,
			"display_name": m.DisplayName,
			"digest":       m.Facts.Digest,
			"capabilities": m.Facts.Capabilities,
			"resolution":   m.Resolution,
		})
	}
	unmatched := []string{}
	for name := range snap.Config.Models {
		if _, ok := state.Facts[name]; !ok {
			unmatched = append(unmatched, name)
		}
	}
	problems := []map[string]string{}
	for _, m := range snap.Models {
		if m.Resolution.Problem != "" {
			problems = append(problems, map[string]string{"upstream": m.Upstream, "problem": m.Resolution.Problem})
		}
	}
	return map[string]any{
		"plugin":                  "cliproxyapi-ollama",
		"version":                 s.version,
		"snapshot_generation":     snap.Generation,
		"snapshot_built_at":       snap.BuiltAt,
		"ready":                   snap.Ready(),
		"discovery":               state,
		"config":                  snap.Config.Summary(),
		"config_issues":           snap.ConfigIssue,
		"model_problems":          problems,
		"unmatched_overrides":     unmatched,
		"models":                  models,
		"excluded":                snap.Excluded,
		"context_source_priority": []string{"observed (/api/ps)", "configured (num_ctx from /api/show)", "fallback (fallback_context_length)", "unknown"},
	}
}

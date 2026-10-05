package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/catalog"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/config"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/translate"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	requestPathMetadataKey = "request_path"
	maxErrorBodyBytes      = 64 * 1024
)

// ExecRequest is an executor call with its host correlation IDs.
type ExecRequest struct {
	pluginapi.ExecutorRequest
	CallbackID string
	StreamID   string
}

type prepared struct {
	snap  *catalog.Snapshot
	model catalog.Model
	cfg   config.Config
	built translate.Built
}

func (s *Service) prepare(ctx context.Context, req ExecRequest, stream bool) (prepared, error) {
	id := strings.TrimSpace(req.Model)
	snap, m, ok := s.lookupWithRefresh(ctx, id)
	if !ok {
		if !snap.Ready() {
			return prepared{}, &StatusError{Code: "upstream_unavailable", Message: fmt.Sprintf("model %s is unavailable: Ollama has not been reachable yet (%s)", id, snap.State.LastError), HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
		}
		return prepared{}, statusErrorf(http.StatusNotFound, "model_not_found", "model %s is not available from Ollama at %s", id, snap.Config.Instance.BaseURL)
	}
	payload := req.Payload
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = req.OriginalRequest
	}
	res := m.Resolution
	built, err := translate.BuildChatRequest(payload, translate.Options{
		Upstream:          m.Upstream,
		Stream:            stream,
		Managed:           res.Policy == config.PolicyManaged,
		NumCtx:            res.ManagedNumCtx,
		NumPredictCeiling: res.NumPredictCeiling,
		KeepAlive:         res.KeepAlive,
		Thinking:          m.Thinking,
		ThinkLevels:       m.Facts.ThinkLevels,
		ThinkTrue:         m.Facts.ThinkTrue,
		ThinkFalse:        m.Facts.ThinkFalse,
	})
	if err != nil {
		return prepared{}, classifyUpstream(id, err)
	}
	return prepared{snap: snap, model: m, cfg: snap.Config, built: built}, nil
}

// Execute serves a non-streaming chat completion.
func (s *Service) Execute(ctx context.Context, req ExecRequest) (pluginapi.ExecutorResponse, error) {
	p, err := s.prepare(ctx, req, false)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	client := s.client(p.cfg)
	resp, err := s.host.Do(ctx, client.ChatRequestFor(p.built.Body, req.CallbackID))
	if err != nil {
		return pluginapi.ExecutorResponse{}, classifyUpstream(p.model.ID, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		s.onUpstreamStatus(resp.StatusCode)
		return pluginapi.ExecutorResponse{}, classifyUpstream(p.model.ID, ollama.ErrorFromBody(resp.StatusCode, resp.Body))
	}
	var chat ollama.ChatResponse
	if err := json.Unmarshal(resp.Body, &chat); err != nil {
		return pluginapi.ExecutorResponse{}, statusErrorf(http.StatusBadGateway, "upstream_error", "decode Ollama response for %s: %v", p.model.ID, err)
	}
	if chat.Error != "" {
		return pluginapi.ExecutorResponse{}, classifyUpstream(p.model.ID, &translate.UpstreamError{Message: chat.Error})
	}
	payload, err := translate.ChatCompletion(chat, p.model.ID, translate.NewID("chatcmpl-"), s.opts.Now().Unix())
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	s.afterManaged(p)
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// ExecuteStream opens the upstream stream synchronously, so HTTP failures are
// returned with http_status, then forwards translated chunks asynchronously.
func (s *Service) ExecuteStream(ctx context.Context, req ExecRequest) (http.Header, error) {
	if strings.TrimSpace(req.StreamID) == "" {
		return nil, statusErrorf(http.StatusInternalServerError, "executor_error", "stream_id is required for executor.execute_stream")
	}
	p, err := s.prepare(ctx, req, true)
	if err != nil {
		return nil, err
	}
	client := s.client(p.cfg)
	st, err := s.host.OpenStream(ctx, client.ChatRequestFor(p.built.Body, req.CallbackID))
	if err != nil {
		return nil, classifyUpstream(p.model.ID, err)
	}
	if st.StatusCode < 200 || st.StatusCode > 299 {
		body := s.drain(ctx, st.ID)
		s.onUpstreamStatus(st.StatusCode)
		return nil, classifyUpstream(p.model.ID, ollama.ErrorFromBody(st.StatusCode, body))
	}

	tr := &translate.StreamTranslator{
		ID:           translate.NewID("chatcmpl-"),
		Model:        p.model.ID,
		Created:      s.opts.Now().Unix(),
		IncludeUsage: p.built.IncludeUsage,
		SSEPrefix:    needsSSEPrefix(req.ExecutorRequest),
	}
	go s.pump(p, tr, st.ID, req.StreamID)
	return http.Header{"Content-Type": []string{"text/event-stream"}}, nil
}

func (s *Service) drain(ctx context.Context, streamID string) []byte {
	defer s.host.Close(streamID)
	var body []byte
	for len(body) < maxErrorBodyBytes {
		chunk, done, err := s.host.Read(ctx, streamID)
		body = append(body, chunk...)
		if done || err != nil {
			break
		}
	}
	return body
}

func (s *Service) pump(p prepared, tr *translate.StreamTranslator, upstreamID, outputID string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := false
	finish := func(msg string) {
		if !closed {
			closed = true
			s.host.CloseOutput(outputID, msg)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			s.host.Close(upstreamID)
			finish(fmt.Sprintf("plugin stream panic: %v", r))
		}
	}()
	emit := func(frames [][]byte) bool {
		for _, f := range frames {
			if err := s.host.EmitStream(outputID, f); err != nil {
				// Downstream is gone (client cancelled): stop generation upstream.
				s.host.Close(upstreamID)
				closed = true
				return false
			}
		}
		return true
	}
	for {
		chunk, done, err := s.host.Read(ctx, upstreamID)
		if len(chunk) > 0 {
			frames, errFeed := tr.Feed(chunk)
			if !emit(frames) {
				return
			}
			if errFeed != nil {
				s.host.Close(upstreamID)
				finish(classifyUpstream(p.model.ID, errFeed).Message)
				return
			}
		}
		if err != nil {
			finish(classifyUpstream(p.model.ID, err).Message)
			return
		}
		if done {
			break
		}
	}
	frames, errFlush := tr.Flush()
	if !emit(frames) {
		return
	}
	if errFlush != nil {
		finish(classifyUpstream(p.model.ID, errFlush).Message)
		return
	}
	if !tr.Finished() {
		finish(fmt.Sprintf("Ollama stream for %s ended before completion", p.model.ID))
		return
	}
	finish("")
	s.afterManaged(p)
}

// needsSSEPrefix decides the stream frame shape. For OpenAI chat/completions
// clients the host passes chunks through and adds "data: " itself; for every
// other client protocol the host's OpenAI stream translator expects SSE lines.
func needsSSEPrefix(req pluginapi.ExecutorRequest) bool {
	if path, ok := req.Metadata[requestPathMetadataKey].(string); ok && strings.TrimSpace(path) != "" {
		path = strings.TrimRight(strings.TrimSpace(path), "/")
		return !(strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/completions"))
	}
	if len(req.OriginalRequest) == 0 || len(req.Payload) == 0 {
		return false
	}
	return !bytes.Equal(bytes.TrimSpace(req.OriginalRequest), bytes.TrimSpace(req.Payload))
}

func (s *Service) onUpstreamStatus(status int) {
	if status == http.StatusNotFound {
		s.TriggerRefresh()
	}
}

// afterManaged verifies a managed allocation with /api/ps in the background.
func (s *Service) afterManaged(p prepared) {
	res := p.model.Resolution
	if res.Policy != config.PolicyManaged || !p.cfg.VerifyManagedAllocation || res.ManagedNumCtx <= 0 {
		return
	}
	if _, busy := s.verifying.LoadOrStore(p.model.Upstream, true); busy {
		return
	}
	go func() {
		defer s.verifying.Delete(p.model.Upstream)
		defer func() { _ = recover() }()
		time.Sleep(s.opts.VerifyDelay)
		cfg := s.Config()
		if cfg.Instance.BaseURL != p.cfg.Instance.BaseURL {
			return
		}
		v, err := s.disc.VerifyManaged(context.Background(), s.client(cfg), cfg.Instance, p.model.Upstream, res.ManagedNumCtx)
		if err != nil {
			s.logf("warn", "could not verify managed num_ctx for %s: %v", p.model.Upstream, err)
			return
		}
		if v.Loaded && v.Observed != v.Expected {
			s.logf("warn", "managed num_ctx discrepancy for %s: expected %d, Ollama reports %d", p.model.Upstream, v.Expected, v.Observed)
		}
		s.rebuild()
	}()
}

// CountTokens is not supported by Ollama's public API.
func (s *Service) CountTokens() error {
	return statusErrorf(http.StatusNotImplemented, "not_supported", "token counting is not supported by the Ollama provider")
}

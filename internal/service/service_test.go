package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/catalog"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/testutil"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/upstream"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeHost struct {
	*upstream.NetHTTP
	mu       sync.Mutex
	emitted  map[string][][]byte
	closed   map[string]string
	done     map[string]chan struct{}
	failEmit bool
	logs     []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{NetHTTP: upstream.NewNetHTTP(nil), emitted: map[string][][]byte{}, closed: map[string]string{}, done: map[string]chan struct{}{}}
}

func (h *fakeHost) doneChan(id string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done[id] == nil {
		h.done[id] = make(chan struct{})
	}
	return h.done[id]
}

func (h *fakeHost) EmitStream(id string, payload []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failEmit {
		return errors.New("stream closed")
	}
	h.emitted[id] = append(h.emitted[id], append([]byte(nil), payload...))
	return nil
}

func (h *fakeHost) CloseOutput(id, msg string) {
	ch := h.doneChan(id)
	h.mu.Lock()
	h.closed[id] = msg
	h.mu.Unlock()
	close(ch)
}

func (h *fakeHost) Log(level, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, level+": "+msg)
}

func (h *fakeHost) wait(t *testing.T, id string) ([][]byte, string) {
	t.Helper()
	select {
	case <-h.doneChan(id):
	case <-time.After(5 * time.Second):
		t.Fatalf("stream %s not closed", id)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.emitted[id], h.closed[id]
}

func setup(t *testing.T, cfgYAML string) (*Service, *fakeHost, *testutil.FakeOllama) {
	t.Helper()
	f := testutil.NewFakeOllama(t)
	f.Set(func(f *testutil.FakeOllama) {
		f.Tags = []ollama.TagModel{
			{Name: "qwen3:8b", Digest: "d1", Details: ollama.ModelDetails{Family: "qwen3"}},
			{Name: "tiny:latest", Digest: "d2", Details: ollama.ModelDetails{Family: "llama"}},
			{Name: "embed:latest", Digest: "d3"},
		}
		f.Shows["qwen3:8b"] = testutil.ShowJSON([]string{"completion", "tools", "thinking"}, "num_ctx 16384", "qwen3", 40960)
		f.Shows["tiny:latest"] = testutil.ShowJSON([]string{"completion"}, "", "llama", 8192)
		f.Shows["embed:latest"] = testutil.ShowJSON([]string{"embedding"}, "", "bert", 512)
		f.ChatFunc = func(w http.ResponseWriter, req ollama.ChatRequest) {
			if req.Stream {
				testutil.NDJSON(w,
					map[string]any{"model": req.Model, "message": map[string]any{"role": "assistant", "content": "Hel"}, "done": false},
					map[string]any{"model": req.Model, "message": map[string]any{"role": "assistant", "content": "lo"}, "done": false},
					map[string]any{"model": req.Model, "message": map[string]any{"role": "assistant", "content": ""}, "done": true, "done_reason": "stop", "prompt_eval_count": 3, "eval_count": 2},
				)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "message": map[string]any{"role": "assistant", "content": "Hello"}, "done": true, "done_reason": "stop", "prompt_eval_count": 3, "eval_count": 1})
		}
	})
	host := newFakeHost()
	s := New(host, "test", Options{StaticModelsWait: time.Second, OnDemandMinInterval: time.Millisecond, VerifyDelay: time.Millisecond})
	t.Cleanup(func() { s.Stop(time.Second) })
	s.Configure([]byte("base_url: " + f.URL() + "\n" + cfgYAML))
	if !s.WaitReady(5 * time.Second) {
		t.Fatalf("service never became ready: %+v", s.Snapshot().State)
	}
	return s, host, f
}

func chatReq(model string, stream bool) ExecRequest {
	payload := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"max_tokens":50}`)
	return ExecRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           model,
			Stream:          stream,
			Payload:         payload,
			OriginalRequest: payload,
			Metadata:        map[string]any{"request_path": "/v1/chat/completions"},
		},
		StreamID: "out-1",
	}
}

func lastChatBody(t *testing.T, f *testutil.FakeOllama) map[string]any {
	t.Helper()
	calls := f.CallsTo("/api/chat")
	if len(calls) == 0 {
		t.Fatal("no chat call")
	}
	var m map[string]any
	if err := json.Unmarshal(calls[len(calls)-1].Body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRouteClaimsOnlyOwnModels(t *testing.T) {
	s, _, _ := setup(t, "")
	if r := s.Route("ollama/qwen3:8b"); !r.Handled || r.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("route = %+v", r)
	}
	if r := s.Route("ollama/not-installed:1b"); !r.Handled {
		t.Fatal("prefix namespace must be claimed so missing models get a clear 404")
	}
	for _, other := range []string{"claude-opus-5-5", "gpt-5", "qwen3:8b", ""} {
		if r := s.Route(other); r.Handled {
			t.Fatalf("claimed foreign model %q", other)
		}
	}
}

func TestStaticModelsAndModelInfo(t *testing.T) {
	s, _, _ := setup(t, "")
	resp := s.StaticModels()
	if resp.Provider != "ollama" || len(resp.Models) != 2 {
		t.Fatalf("static = %+v", resp)
	}
	var qwen pluginapi.ModelInfo
	for _, m := range resp.Models {
		if m.ID == "ollama/qwen3:8b" {
			qwen = m
		}
	}
	if qwen.ContextLength != 16384 || qwen.OwnedBy != "ollama" || qwen.Name != "qwen3:8b" {
		t.Fatalf("qwen = %+v", qwen)
	}
}

func TestExecuteObserveNonStream(t *testing.T) {
	s, _, f := setup(t, "")
	resp, err := s.Execute(context.Background(), chatReq("ollama/qwen3:8b", false))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(resp.Payload, &out)
	if out["model"] != "ollama/qwen3:8b" || !strings.Contains(string(resp.Payload), "Hello") {
		t.Fatalf("payload = %s", resp.Payload)
	}
	body := lastChatBody(t, f)
	if body["model"] != "qwen3:8b" {
		t.Fatalf("upstream model = %v", body["model"])
	}
	if _, ok := body["keep_alive"]; ok {
		t.Fatal("observe sent keep_alive")
	}
	if opts, _ := body["options"].(map[string]any); opts["num_ctx"] != nil {
		t.Fatal("observe sent num_ctx")
	}
	f.AssertNoMutations(t)
}

func TestExecuteManagedStreamAndVerification(t *testing.T) {
	s, host, f := setup(t, "models: {qwen3:8b: {policy: managed, num_ctx: 65536, num_predict: 20, keep_alive: 1m}}")
	m, _ := s.Snapshot().Lookup("ollama/qwen3:8b")
	if m.Resolution.ContextLength != 40960 || !m.Resolution.CappedToDeclaredMax {
		t.Fatalf("managed num_ctx must be capped to the declared maximum: %+v", m.Resolution)
	}
	// Ollama reports a smaller allocation after the request.
	f.Set(func(f *testutil.FakeOllama) {
		f.PS = []ollama.PSModel{{Name: "qwen3:8b", Digest: "d1", ContextLength: 32768}}
	})
	headers, err := s.ExecuteStream(context.Background(), chatReq("ollama/qwen3:8b", true))
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("headers = %v", headers)
	}
	frames, closeMsg := host.wait(t, "out-1")
	if closeMsg != "" || len(frames) != 3 {
		t.Fatalf("frames = %d close = %q", len(frames), closeMsg)
	}
	if strings.HasPrefix(string(frames[0]), "data:") {
		t.Fatal("chat/completions passthrough frames must be bare JSON")
	}
	body := lastChatBody(t, f)
	opts := body["options"].(map[string]any)
	if opts["num_ctx"].(float64) != 40960 || opts["num_predict"].(float64) != 20 || body["keep_alive"] != "1m0s" {
		t.Fatalf("managed body = %v", body)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m, _ = s.Snapshot().Lookup("ollama/qwen3:8b")
		if m.Resolution.Verification != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m.Resolution.ContextLength != 32768 || m.Resolution.Discrepancy == "" {
		t.Fatalf("verified smaller allocation must be advertised: %+v", m.Resolution)
	}
}

func TestStreamSSEPrefixForTranslatedClients(t *testing.T) {
	s, host, _ := setup(t, "")
	req := chatReq("ollama/tiny:latest", true)
	req.Metadata = map[string]any{"request_path": "/v1/messages"}
	if _, err := s.ExecuteStream(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	frames, _ := host.wait(t, "out-1")
	for _, f := range frames {
		if !strings.HasPrefix(string(f), "data: ") {
			t.Fatalf("frame for translated client lacks SSE prefix: %s", f)
		}
	}
}

func TestErrorsCarryHTTPStatus(t *testing.T) {
	s, _, f := setup(t, "")
	_, err := s.Execute(context.Background(), chatReq("ollama/not-installed:1b", false))
	var se *StatusError
	if !errors.As(err, &se) || se.HTTPStatus != http.StatusNotFound {
		t.Fatalf("missing model err = %v", err)
	}

	f.Set(func(f *testutil.FakeOllama) {
		f.ChatFunc = func(w http.ResponseWriter, req ollama.ChatRequest) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"model requires more system memory (64 GiB) than is available"}`))
		}
	})
	_, err = s.Execute(context.Background(), chatReq("ollama/qwen3:8b", false))
	if !errors.As(err, &se) || se.HTTPStatus != http.StatusBadGateway || !strings.Contains(se.Message, "system memory") {
		t.Fatalf("non-stream upstream err = %v", err)
	}
	_, err = s.ExecuteStream(context.Background(), chatReq("ollama/qwen3:8b", true))
	if !errors.As(err, &se) || se.HTTPStatus != http.StatusBadGateway || !strings.Contains(se.Message, "system memory") {
		t.Fatalf("stream upstream err = %v", err)
	}

	bad := chatReq("ollama/qwen3:8b", false)
	bad.Payload = []byte(`{"messages":[]}`)
	_, err = s.Execute(context.Background(), bad)
	if !errors.As(err, &se) || se.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("bad request err = %v", err)
	}
}

func TestMidStreamErrorClosesWithMessage(t *testing.T) {
	s, host, f := setup(t, "")
	f.Set(func(f *testutil.FakeOllama) {
		f.ChatFunc = func(w http.ResponseWriter, req ollama.ChatRequest) {
			testutil.NDJSON(w,
				map[string]any{"message": map[string]any{"content": "a"}, "done": false},
				map[string]any{"error": "runner crashed"},
			)
		}
	})
	if _, err := s.ExecuteStream(context.Background(), chatReq("ollama/tiny:latest", true)); err != nil {
		t.Fatal(err)
	}
	_, msg := host.wait(t, "out-1")
	if !strings.Contains(msg, "runner crashed") {
		t.Fatalf("close message = %q", msg)
	}
}

func TestCancellationClosesUpstream(t *testing.T) {
	s, host, f := setup(t, "")
	release := make(chan struct{})
	f.Set(func(f *testutil.FakeOllama) {
		f.ChatFunc = func(w http.ResponseWriter, req ollama.ChatRequest) {
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			for i := 0; i < 50; i++ {
				_, _ = w.Write([]byte(`{"message":{"content":"x"},"done":false}` + "\n"))
				fl.Flush()
				select {
				case <-release:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
	})
	t.Cleanup(func() { close(release) })
	host.mu.Lock()
	host.failEmit = true
	host.mu.Unlock()
	if _, err := s.ExecuteStream(context.Background(), chatReq("ollama/tiny:latest", true)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for host.OpenStreams() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if host.OpenStreams() != 0 {
		t.Fatal("upstream stream not closed after downstream cancellation")
	}
}

func TestInterceptResponseOnlyTouchesModelList(t *testing.T) {
	s, _, _ := setup(t, "fallback_context_length: 4096")
	list := []byte(`{"data":[{"created":1,"id":"claude-opus-5-5","object":"model","owned_by":"anthropic"},{"created":1,"id":"ollama/gone:1b","object":"model","owned_by":"ollama"}],"object":"list"}`)
	resp := s.InterceptResponse(pluginapi.ResponseInterceptRequest{SourceFormat: "openai", StatusCode: 200, Body: list})
	out := string(resp.Body)
	if !strings.Contains(out, `{"created":1,"id":"claude-opus-5-5","object":"model","owned_by":"anthropic"}`) {
		t.Fatalf("other provider changed: %s", out)
	}
	if strings.Contains(out, "gone:1b") || !strings.Contains(out, `"id":"ollama/qwen3:8b"`) || !strings.Contains(out, `"context_length":16384`) {
		t.Fatalf("own entries not refreshed: %s", out)
	}
	if !strings.Contains(out, `"id":"ollama/tiny:latest","object":"model","created":0,"owned_by":"ollama","display_name":"ollama/tiny:latest","context_length":4096`) {
		t.Fatalf("fallback context not advertised: %s", out)
	}

	chat := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`)
	for _, req := range []pluginapi.ResponseInterceptRequest{
		{SourceFormat: "openai", Model: "gpt-5", StatusCode: 200, Body: chat},
		{SourceFormat: "openai", Model: "gpt-5", StatusCode: 200, Body: list},
		{SourceFormat: "claude", StatusCode: 200, Body: list},
	} {
		if r := s.InterceptResponse(req); r.Body != nil || r.Headers != nil {
			t.Fatalf("modified non-model-list response: %+v", req)
		}
	}
}

func TestConfigRejectionKeepsPreviousConfig(t *testing.T) {
	s, host, f := setup(t, "")
	issues := s.Configure([]byte("base_url: " + f.URL() + "\nrefresh_interval_seconds: 1"))
	if len(issues) == 0 || s.Config().RefreshInterval != time.Minute {
		t.Fatalf("invalid config applied: %+v", s.Config())
	}
	if _, ok := s.Snapshot().Lookup("ollama/qwen3:8b"); !ok {
		t.Fatal("models lost after rejected config")
	}
	host.mu.Lock()
	logged := strings.Join(host.logs, "\n")
	host.mu.Unlock()
	if !strings.Contains(logged, "refresh_interval_seconds") {
		t.Fatalf("rejection not logged: %s", logged)
	}
}

func TestReconfigureAppliesToLaterRequests(t *testing.T) {
	s, _, f := setup(t, "")
	before := s.Snapshot()
	s.Configure([]byte("base_url: " + f.URL() + "\nmodels: {tiny:latest: {policy: managed, num_ctx: 2048}}"))
	m, _ := s.Snapshot().Lookup("ollama/tiny:latest")
	if m.Resolution.Source != catalog.SourceManaged || m.Resolution.ContextLength != 2048 {
		t.Fatalf("managed override not applied: %+v", m.Resolution)
	}
	old, _ := before.Lookup("ollama/tiny:latest")
	if old.Resolution.Source == catalog.SourceManaged {
		t.Fatal("previous snapshot (in-flight requests) must be immutable")
	}
	if _, err := s.Execute(context.Background(), chatReq("ollama/tiny:latest", false)); err != nil {
		t.Fatal(err)
	}
	if lastChatBody(t, f)["options"].(map[string]any)["num_ctx"].(float64) != 2048 {
		t.Fatal("managed num_ctx not injected after reconfigure")
	}
	s.Configure([]byte("base_url: " + f.URL()))
	if _, err := s.Execute(context.Background(), chatReq("ollama/tiny:latest", false)); err != nil {
		t.Fatal(err)
	}
	if opts, _ := lastChatBody(t, f)["options"].(map[string]any); opts["num_ctx"] != nil {
		t.Fatal("num_ctx injected after switching back to observe")
	}
}

func TestOnDemandRefreshFindsNewModel(t *testing.T) {
	s, _, f := setup(t, "")
	f.Set(func(f *testutil.FakeOllama) {
		f.Tags = append(f.Tags, ollama.TagModel{Name: "new:7b", Digest: "d9"})
		f.Shows["new:7b"] = testutil.ShowJSON([]string{"completion"}, "", "llama", 4096)
	})
	if _, err := s.Execute(context.Background(), chatReq("ollama/new:7b", false)); err != nil {
		t.Fatalf("new model not discovered on demand: %v", err)
	}
}

func TestManagementStatus(t *testing.T) {
	s, _, _ := setup(t, "api_key: super-secret-token")
	resp := s.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management" + StatusRoutePath})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := string(resp.Body)
	if strings.Contains(body, "super-secret-token") {
		t.Fatal("diagnostics leaked api_key")
	}
	for _, want := range []string{`"context_source": "configured"`, `"declared_max_context": 40960`, `"embedding-only model"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status missing %s:\n%s", want, body)
		}
	}
}

func TestFirstDiscoveryIsLogged(t *testing.T) {
	_, host, _ := setup(t, "")
	var logged string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		host.mu.Lock()
		logged = strings.Join(host.logs, "\n")
		host.mu.Unlock()
		if strings.Contains(logged, "discovered 2 models") {
			break
		}
	}
	if !strings.Contains(logged, "discovered 2 models") {
		t.Fatalf("first successful discovery not logged:\n%s", logged)
	}
}

package translate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
)

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

const simpleReq = `{"model":"ollama/qwen3:8b","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"max_tokens":100,"temperature":0.2,"stop":"END"}`

func TestObserveInjectsNoContextSettings(t *testing.T) {
	b, err := BuildChatRequest([]byte(simpleReq), Options{Upstream: "qwen3:8b"})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, b.Body)
	if m["model"] != "qwen3:8b" || m["stream"] != false {
		t.Fatalf("body = %s", b.Body)
	}
	if _, ok := m["keep_alive"]; ok {
		t.Fatal("observe must not send keep_alive")
	}
	opts := m["options"].(map[string]any)
	if _, ok := opts["num_ctx"]; ok {
		t.Fatal("observe must not send num_ctx")
	}
	if opts["num_predict"].(float64) != 100 || opts["temperature"].(float64) != 0.2 {
		t.Fatalf("options = %v", opts)
	}
	if stop := opts["stop"].([]any); len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop = %v", stop)
	}

	b, _ = BuildChatRequest([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), Options{Upstream: "m"})
	if strings.Contains(string(b.Body), "options") || strings.Contains(string(b.Body), "keep_alive") {
		t.Fatalf("minimal observe request carried options: %s", b.Body)
	}
}

func TestManagedInjectionAndOutputCeiling(t *testing.T) {
	opt := Options{Upstream: "qwen3:8b", Managed: true, NumCtx: 8192, NumPredictCeiling: 512, KeepAlive: "5m0s"}
	b, err := BuildChatRequest([]byte(simpleReq), opt)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, b.Body)
	opts := m["options"].(map[string]any)
	if opts["num_ctx"].(float64) != 8192 || m["keep_alive"] != "5m0s" {
		t.Fatalf("managed body = %s", b.Body)
	}
	if opts["num_predict"].(float64) != 100 || b.NumPredict != 100 {
		t.Fatalf("smaller client max_tokens must be preserved: %v", opts["num_predict"])
	}

	big := `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":4096,"max_tokens":50}`
	b, _ = BuildChatRequest([]byte(big), opt)
	if decode(t, b.Body)["options"].(map[string]any)["num_predict"].(float64) != 512 {
		t.Fatalf("ceiling not applied: %s", b.Body)
	}
	none := `{"messages":[{"role":"user","content":"hi"}]}`
	b, _ = BuildChatRequest([]byte(none), opt)
	if decode(t, b.Body)["options"].(map[string]any)["num_predict"].(float64) != 512 {
		t.Fatalf("ceiling not applied without client limit: %s", b.Body)
	}

	opt.KeepAlive = "-1"
	b, _ = BuildChatRequest([]byte(none), opt)
	if decode(t, b.Body)["keep_alive"].(float64) != -1 {
		t.Fatalf("keep_alive -1 must be numeric: %s", b.Body)
	}
}

func TestToolsImagesAndToolMessages(t *testing.T) {
	req := `{"messages":[
	 {"role":"developer","content":"sys"},
	 {"role":"user","content":[{"type":"text","text":"what is"},{"type":"text","text":"this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
	 {"role":"assistant","content":null,"reasoning_content":"thinking...","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
	 {"role":"tool","tool_call_id":"call_1","content":"result"}
	],
	"tools":[{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object"}}}],
	"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`
	b, err := BuildChatRequest([]byte(req), Options{Upstream: "m"})
	if err != nil {
		t.Fatal(err)
	}
	r := b.Request
	if r.Messages[0].Role != "system" {
		t.Fatalf("developer role = %q", r.Messages[0].Role)
	}
	if r.Messages[1].Content != "what is\nthis?" || len(r.Messages[1].Images) != 1 || r.Messages[1].Images[0] != "AAAA" {
		t.Fatalf("user message = %+v", r.Messages[1])
	}
	a := r.Messages[2]
	if a.Thinking != "thinking..." || len(a.ToolCalls) != 1 || string(a.ToolCalls[0].Function.Arguments) != `{"q":"x"}` {
		t.Fatalf("assistant = %+v", a)
	}
	if tm := r.Messages[3]; tm.Role != "tool" || tm.ToolName != "lookup" || tm.ToolCallID != "call_1" {
		t.Fatalf("tool message = %+v", tm)
	}
	if !strings.Contains(string(r.Tools), `"lookup"`) || string(r.Format) != `{"type":"object"}` {
		t.Fatalf("tools/format = %s %s", r.Tools, r.Format)
	}

	none := `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"a"}}],"tool_choice":"none"}`
	b, _ = BuildChatRequest([]byte(none), Options{Upstream: "m"})
	if b.Request.Tools != nil {
		t.Fatal("tool_choice none must drop tools")
	}
}

func TestRequestErrors(t *testing.T) {
	cases := []string{
		`not json`,
		`{"messages":[]}`,
		`{"messages":[{"role":"wizard","content":"x"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"a","function":{"name":"f","arguments":"not json"}}]}]}`,
	}
	for _, c := range cases {
		_, err := BuildChatRequest([]byte(c), Options{Upstream: "m"})
		var re *RequestError
		if !errors.As(err, &re) {
			t.Errorf("expected RequestError for %s, got %v", c, err)
		}
	}
}

func TestThinkMapping(t *testing.T) {
	req := func(extra string) []byte {
		return []byte(`{"messages":[{"role":"user","content":"x"}]` + extra + `}`)
	}
	cases := []struct {
		name  string
		extra string
		opt   Options
		want  string
	}{
		{"no thinking support", `,"reasoning_effort":"high"`, Options{}, ""},
		{"bool model", `,"reasoning_effort":"high"`, Options{Thinking: true}, "true"},
		{"levels", `,"reasoning_effort":"low"`, Options{Thinking: true, ThinkLevels: []string{"low", "medium", "high"}}, `"low"`},
		{"none", `,"reasoning_effort":"none"`, Options{Thinking: true, ThinkTrue: true, ThinkFalse: true}, "false"},
		{"none unsupported", `,"reasoning_effort":"none"`, Options{Thinking: true, ThinkLevels: []string{"low"}}, ""},
		{"reasoning object", `,"reasoning":{"effort":"medium"}`, Options{Thinking: true}, "true"},
		{"explicit think", `,"think":"high"`, Options{}, `"high"`},
		{"default", ``, Options{Thinking: true}, ""},
	}
	for _, c := range cases {
		b, err := BuildChatRequest(req(c.extra), Options{Upstream: "m", Thinking: c.opt.Thinking, ThinkLevels: c.opt.ThinkLevels, ThinkTrue: c.opt.ThinkTrue, ThinkFalse: c.opt.ThinkFalse})
		if err != nil {
			t.Fatal(err)
		}
		if got := string(b.Request.Think); got != c.want {
			t.Errorf("%s: think = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestChatCompletion(t *testing.T) {
	resp := ollama.ChatResponse{
		Message: ollama.Message{
			Role: "assistant", Content: "", Thinking: "hmm",
			ToolCalls: []ollama.ToolCall{{Function: ollama.ToolCallFunction{Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}}},
		},
		Done: true, DoneReason: "stop", PromptEvalCount: 10, EvalCount: 5, PromptEvalCachedCount: 4,
	}
	raw, err := ChatCompletion(resp, "ollama/m", "chatcmpl-1", 123)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, raw)
	choice := m["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || m["model"] != "ollama/m" || m["object"] != "chat.completion" {
		t.Fatalf("completion = %s", raw)
	}
	msg := choice["message"].(map[string]any)
	tc := msg["tool_calls"].([]any)[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["arguments"] != `{"q":"x"}` || !strings.HasPrefix(tc["id"].(string), "call_") || msg["reasoning_content"] != "hmm" {
		t.Fatalf("message = %v", msg)
	}
	u := m["usage"].(map[string]any)
	if u["prompt_tokens"].(float64) != 10 || u["completion_tokens"].(float64) != 5 || u["total_tokens"].(float64) != 15 {
		t.Fatalf("usage = %v", u)
	}
	if u["prompt_tokens_details"].(map[string]any)["cached_tokens"].(float64) != 4 {
		t.Fatalf("cached tokens = %v", u)
	}

	raw, _ = ChatCompletion(ollama.ChatResponse{Message: ollama.Message{Content: "x"}, Done: true, DoneReason: "length"}, "m", "id", 1)
	if decode(t, raw)["choices"].([]any)[0].(map[string]any)["finish_reason"] != "length" {
		t.Fatal("length finish reason")
	}
}

func ndjson(events ...string) []byte {
	return []byte(strings.Join(events, "\n") + "\n")
}

func TestStreamTranslation(t *testing.T) {
	tr := &StreamTranslator{ID: "chatcmpl-x", Model: "ollama/m", Created: 1}
	data := ndjson(
		`{"message":{"role":"assistant","content":"","thinking":"plan"},"done":false}`,
		`{"message":{"role":"assistant","content":"Hel"},"done":false}`,
		`{"message":{"role":"assistant","content":"lo"},"done":false}`,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"f","arguments":{"a":1}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":7,"eval_count":3}`,
	)
	// Feed in awkward pieces to exercise line buffering.
	var frames [][]byte
	for i := 0; i < len(data); i += 7 {
		end := i + 7
		if end > len(data) {
			end = len(data)
		}
		out, err := tr.Feed(data[i:end])
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, out...)
	}
	if !tr.Finished() || len(frames) != 5 {
		t.Fatalf("frames = %d finished = %v", len(frames), tr.Finished())
	}
	first := decode(t, frames[0])
	delta := first["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["role"] != "assistant" || delta["reasoning_content"] != "plan" || first["object"] != "chat.completion.chunk" {
		t.Fatalf("first frame = %s", frames[0])
	}
	second := decode(t, frames[1])["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if _, hasRole := second["role"]; hasRole || second["content"] != "Hel" {
		t.Fatalf("second frame = %s", frames[1])
	}
	tool := decode(t, frames[3])["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tool["index"].(float64) != 0 || tool["function"].(map[string]any)["arguments"] != `{"a":1}` {
		t.Fatalf("tool frame = %s", frames[3])
	}
	last := decode(t, frames[4])
	if last["choices"].([]any)[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("final frame = %s", frames[4])
	}
	if last["usage"].(map[string]any)["total_tokens"].(float64) != 10 {
		t.Fatalf("usage = %s", frames[4])
	}
	if p, c := tr.Usage(); p != 7 || c != 3 {
		t.Fatalf("usage = %d %d", p, c)
	}
}

func TestStreamIncludeUsageAndSSEPrefix(t *testing.T) {
	tr := &StreamTranslator{ID: "x", Model: "m", IncludeUsage: true, SSEPrefix: true}
	frames, err := tr.Feed(ndjson(
		`{"message":{"content":"hi"},"done":false}`,
		`{"message":{"content":""},"done":true,"done_reason":"length","prompt_eval_count":1,"eval_count":2}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("frames = %d", len(frames))
	}
	for _, f := range frames {
		if !strings.HasPrefix(string(f), "data: {") {
			t.Fatalf("frame without SSE prefix: %s", f)
		}
	}
	final := decode(t, frames[1][len("data: "):])
	if final["choices"].([]any)[0].(map[string]any)["finish_reason"] != "length" {
		t.Fatalf("final = %s", frames[1])
	}
	if _, ok := final["usage"]; ok {
		t.Fatal("usage must be a separate chunk when include_usage is set")
	}
	usageChunk := decode(t, frames[2][len("data: "):])
	if len(usageChunk["choices"].([]any)) != 0 || usageChunk["usage"] == nil {
		t.Fatalf("usage chunk = %s", frames[2])
	}
}

func TestStreamErrors(t *testing.T) {
	tr := &StreamTranslator{ID: "x", Model: "m"}
	_, err := tr.Feed(ndjson(`{"error":"model requires more system memory"}`))
	var ue *UpstreamError
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "memory") {
		t.Fatalf("err = %v", err)
	}
	tr = &StreamTranslator{ID: "x", Model: "m"}
	frames, err := tr.Feed([]byte(`{"message":{"content":"partial"},"done":false}`))
	if err != nil || len(frames) != 0 {
		t.Fatalf("unterminated line must wait: %v %d", err, len(frames))
	}
	frames, err = tr.Flush()
	if err != nil || len(frames) != 1 || tr.Finished() {
		t.Fatalf("flush = %d %v finished=%v", len(frames), err, tr.Finished())
	}
}

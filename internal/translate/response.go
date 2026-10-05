package translate

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
)

// NewID returns an OpenAI-style identifier with the given prefix.
func NewID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "000000000000"
	}
	return prefix + hex.EncodeToString(b[:])
}

type oaToolCallOut struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type usage struct {
	PromptTokens        int            `json:"prompt_tokens"`
	CompletionTokens    int            `json:"completion_tokens"`
	TotalTokens         int            `json:"total_tokens"`
	PromptTokensDetails *promptDetails `json:"prompt_tokens_details,omitempty"`
}

type promptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

func usageFrom(r ollama.ChatResponse) *usage {
	u := &usage{
		PromptTokens:     r.PromptEvalCount,
		CompletionTokens: r.EvalCount,
		TotalTokens:      r.PromptEvalCount + r.EvalCount,
	}
	if r.PromptEvalCachedCount > 0 {
		u.PromptTokensDetails = &promptDetails{CachedTokens: r.PromptEvalCachedCount}
	}
	return u
}

func finishReason(doneReason string, sawToolCalls bool) string {
	if sawToolCalls {
		return "tool_calls"
	}
	switch strings.ToLower(doneReason) {
	case "length":
		return "length"
	default:
		return "stop"
	}
}

func convertToolCalls(calls []ollama.ToolCall, startIndex int, withIndex bool) []oaToolCallOut {
	out := make([]oaToolCallOut, 0, len(calls))
	for i, tc := range calls {
		var c oaToolCallOut
		if withIndex {
			idx := startIndex + i
			c.Index = &idx
		}
		c.ID = tc.ID
		if c.ID == "" {
			c.ID = NewID("call_")
		}
		c.Type = "function"
		c.Function.Name = tc.Function.Name
		args := bytes.TrimSpace(tc.Function.Arguments)
		if len(args) == 0 || bytes.Equal(args, []byte("null")) {
			args = []byte("{}")
		}
		if args[0] == '"' {
			var s string
			if json.Unmarshal(args, &s) == nil {
				args = []byte(s)
			}
		}
		c.Function.Arguments = string(args)
		out = append(out, c)
	}
	return out
}

// ChatCompletion converts a non-streaming /api/chat response to an OpenAI
// chat.completion object.
func ChatCompletion(r ollama.ChatResponse, model, id string, created int64) ([]byte, error) {
	msg := map[string]any{"role": "assistant", "content": r.Message.Content}
	if r.Message.Thinking != "" {
		msg["reasoning_content"] = r.Message.Thinking
	}
	if len(r.Message.ToolCalls) > 0 {
		msg["tool_calls"] = convertToolCalls(r.Message.ToolCalls, 0, false)
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finishReason(r.DoneReason, len(r.Message.ToolCalls) > 0),
		}},
		"usage": usageFrom(r),
	}
	return json.Marshal(out)
}

// StreamTranslator converts /api/chat NDJSON events into OpenAI
// chat.completion.chunk payloads.
type StreamTranslator struct {
	ID           string
	Model        string
	Created      int64
	IncludeUsage bool
	// SSEPrefix emits "data: {json}" frames (needed when CLIProxyAPI translates
	// the stream to another client protocol); otherwise bare JSON objects.
	SSEPrefix bool

	buf          []byte
	sentRole     bool
	toolCount    int
	finished     bool
	finishReason string
	usage        *usage
}

// UpstreamError is an error event received inside the NDJSON stream.
type UpstreamError struct{ Message string }

func (e *UpstreamError) Error() string { return "ollama stream error: " + e.Message }

const maxLineBytes = 32 * 1024 * 1024

// Feed consumes raw stream bytes and returns translated frames.
func (t *StreamTranslator) Feed(data []byte) ([][]byte, error) {
	t.buf = append(t.buf, data...)
	var frames [][]byte
	for {
		idx := bytes.IndexByte(t.buf, '\n')
		if idx < 0 {
			break
		}
		line := bytes.TrimSpace(t.buf[:idx])
		t.buf = t.buf[idx+1:]
		out, err := t.line(line)
		frames = append(frames, out...)
		if err != nil {
			return frames, err
		}
	}
	if len(t.buf) > maxLineBytes {
		return frames, fmt.Errorf("ollama stream line exceeds %d bytes", maxLineBytes)
	}
	return frames, nil
}

// Flush processes any trailing unterminated line.
func (t *StreamTranslator) Flush() ([][]byte, error) {
	line := bytes.TrimSpace(t.buf)
	t.buf = nil
	return t.line(line)
}

// Finished reports whether the terminal done event was seen.
func (t *StreamTranslator) Finished() bool { return t.finished }

// Usage returns prompt and completion token counts after the stream ends.
func (t *StreamTranslator) Usage() (int, int) {
	if t.usage == nil {
		return 0, 0
	}
	return t.usage.PromptTokens, t.usage.CompletionTokens
}

func (t *StreamTranslator) line(line []byte) ([][]byte, error) {
	if len(line) == 0 || t.finished {
		return nil, nil
	}
	var ev ollama.ChatResponse
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("decode ollama stream event: %w", err)
	}
	if ev.Error != "" {
		return nil, &UpstreamError{Message: ev.Error}
	}
	return t.Event(ev)
}

// Event translates one decoded event.
func (t *StreamTranslator) Event(ev ollama.ChatResponse) ([][]byte, error) {
	var frames [][]byte
	delta := map[string]any{}
	if !t.sentRole {
		delta["role"] = "assistant"
	}
	if ev.Message.Thinking != "" {
		delta["reasoning_content"] = ev.Message.Thinking
	}
	if ev.Message.Content != "" {
		delta["content"] = ev.Message.Content
	}
	if len(ev.Message.ToolCalls) > 0 {
		delta["tool_calls"] = convertToolCalls(ev.Message.ToolCalls, t.toolCount, true)
		t.toolCount += len(ev.Message.ToolCalls)
	}
	hasContent := ev.Message.Thinking != "" || ev.Message.Content != "" || len(ev.Message.ToolCalls) > 0
	if hasContent || (!t.sentRole && !ev.Done) {
		t.sentRole = true
		frame, err := t.chunk([]any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}, nil)
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
		delta = map[string]any{}
	}
	if !ev.Done {
		return frames, nil
	}
	t.finished = true
	t.finishReason = finishReason(ev.DoneReason, t.toolCount > 0)
	t.usage = usageFrom(ev)
	if !t.sentRole {
		delta["role"] = "assistant"
		t.sentRole = true
	}
	final := []any{map[string]any{"index": 0, "delta": delta, "finish_reason": t.finishReason}}
	if t.IncludeUsage {
		frame, err := t.chunk(final, nil)
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
		frame, err = t.chunk([]any{}, t.usage)
		if err != nil {
			return nil, err
		}
		return append(frames, frame), nil
	}
	frame, err := t.chunk(final, t.usage)
	if err != nil {
		return nil, err
	}
	return append(frames, frame), nil
}

func (t *StreamTranslator) chunk(choices []any, u *usage) ([]byte, error) {
	obj := map[string]any{
		"id":      t.ID,
		"object":  "chat.completion.chunk",
		"created": t.Created,
		"model":   t.Model,
		"choices": choices,
	}
	if u != nil {
		obj["usage"] = u
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	if t.SSEPrefix {
		return append([]byte("data: "), raw...), nil
	}
	return raw, nil
}

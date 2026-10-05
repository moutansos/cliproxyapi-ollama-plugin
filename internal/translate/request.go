// Package translate converts between OpenAI Chat Completions and native Ollama /api/chat.
package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
)

// RequestError is a client error in the inbound payload (HTTP 400).
type RequestError struct{ Message string }

func (e *RequestError) Error() string { return e.Message }

func badRequest(format string, args ...any) error {
	return &RequestError{Message: fmt.Sprintf(format, args...)}
}

// Options controls how a request is built for one model.
type Options struct {
	Upstream string
	Stream   bool

	// Managed context controls. They are injected only when Managed is true.
	Managed           bool
	NumCtx            int
	NumPredictCeiling int
	KeepAlive         string

	// Model thinking support from discovery.
	Thinking    bool
	ThinkLevels []string
	ThinkTrue   bool
	ThinkFalse  bool
}

// Built is a translated request.
type Built struct {
	Body         []byte
	Request      ollama.ChatRequest
	IncludeUsage bool
	// NumPredict is the num_predict actually sent (0 when omitted).
	NumPredict int
}

type oaRequest struct {
	Messages            []oaMessage      `json:"messages"`
	Tools               json.RawMessage  `json:"tools"`
	ToolChoice          json.RawMessage  `json:"tool_choice"`
	ResponseFormat      *oaRespFormat    `json:"response_format"`
	Temperature         *float64         `json:"temperature"`
	TopP                *float64         `json:"top_p"`
	TopK                *int             `json:"top_k"`
	MinP                *float64         `json:"min_p"`
	Seed                *int64           `json:"seed"`
	Stop                json.RawMessage  `json:"stop"`
	PresencePenalty     *float64         `json:"presence_penalty"`
	FrequencyPenalty    *float64         `json:"frequency_penalty"`
	MaxTokens           *int             `json:"max_tokens"`
	MaxCompletionTokens *int             `json:"max_completion_tokens"`
	ReasoningEffort     string           `json:"reasoning_effort"`
	Reasoning           *oaReasoning     `json:"reasoning"`
	Think               json.RawMessage  `json:"think"`
	StreamOptions       *oaStreamOptions `json:"stream_options"`
}

type oaReasoning struct {
	Effort string `json:"effort"`
}

type oaStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaRespFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema"`
}

type oaMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             string          `json:"name"`
	ToolCalls        []oaToolCall    `json:"tool_calls"`
	ToolCallID       string          `json:"tool_call_id"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type oaPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// BuildChatRequest converts an OpenAI Chat Completions payload into /api/chat.
// Runtime context settings (num_ctx, keep_alive, num_predict ceiling) are only
// injected for managed models.
func BuildChatRequest(payload []byte, opt Options) (Built, error) {
	var in oaRequest
	if err := json.Unmarshal(payload, &in); err != nil {
		return Built{}, badRequest("invalid chat completions request: %v", err)
	}
	if len(in.Messages) == 0 {
		return Built{}, badRequest("messages must not be empty")
	}

	out := ollama.ChatRequest{Model: opt.Upstream, Stream: opt.Stream}
	toolNames := map[string]string{}
	for i, m := range in.Messages {
		msg, err := convertMessage(m, toolNames)
		if err != nil {
			return Built{}, badRequest("messages[%d]: %v", i, err)
		}
		out.Messages = append(out.Messages, msg)
	}

	if tools, err := convertTools(in.Tools, in.ToolChoice); err != nil {
		return Built{}, err
	} else if tools != nil {
		out.Tools = tools
	}

	if in.ResponseFormat != nil {
		switch in.ResponseFormat.Type {
		case "", "text":
		case "json_object":
			out.Format = json.RawMessage(`"json"`)
		case "json_schema":
			if in.ResponseFormat.JSONSchema == nil || len(in.ResponseFormat.JSONSchema.Schema) == 0 {
				return Built{}, badRequest("response_format.json_schema.schema is required")
			}
			out.Format = in.ResponseFormat.JSONSchema.Schema
		default:
			return Built{}, badRequest("unsupported response_format type %q", in.ResponseFormat.Type)
		}
	}

	options := map[string]any{}
	if in.Temperature != nil {
		options["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		options["top_p"] = *in.TopP
	}
	if in.TopK != nil {
		options["top_k"] = *in.TopK
	}
	if in.MinP != nil {
		options["min_p"] = *in.MinP
	}
	if in.Seed != nil {
		options["seed"] = *in.Seed
	}
	if in.PresencePenalty != nil {
		options["presence_penalty"] = *in.PresencePenalty
	}
	if in.FrequencyPenalty != nil {
		options["frequency_penalty"] = *in.FrequencyPenalty
	}
	if stop, err := convertStop(in.Stop); err != nil {
		return Built{}, err
	} else if len(stop) > 0 {
		options["stop"] = stop
	}

	clientMax := 0
	if in.MaxCompletionTokens != nil && *in.MaxCompletionTokens > 0 {
		clientMax = *in.MaxCompletionTokens
	} else if in.MaxTokens != nil && *in.MaxTokens > 0 {
		clientMax = *in.MaxTokens
	}
	numPredict := clientMax
	if opt.Managed && opt.NumPredictCeiling > 0 && (numPredict == 0 || numPredict > opt.NumPredictCeiling) {
		numPredict = opt.NumPredictCeiling
	}
	if numPredict > 0 {
		options["num_predict"] = numPredict
	}
	if opt.Managed {
		if opt.NumCtx > 0 {
			options["num_ctx"] = opt.NumCtx
		}
		if ka := keepAliveJSON(opt.KeepAlive); ka != nil {
			out.KeepAlive = ka
		}
	}
	if len(options) > 0 {
		out.Options = options
	}

	if think := thinkValue(in, opt); think != nil {
		out.Think = think
	}

	body, err := json.Marshal(out)
	if err != nil {
		return Built{}, err
	}
	includeUsage := in.StreamOptions != nil && in.StreamOptions.IncludeUsage
	return Built{Body: body, Request: out, IncludeUsage: includeUsage, NumPredict: numPredict}, nil
}

func keepAliveJSON(ka string) json.RawMessage {
	ka = strings.TrimSpace(ka)
	if ka == "" {
		return nil
	}
	if ka == "-1" {
		return json.RawMessage(`-1`)
	}
	raw, _ := json.Marshal(ka)
	return raw
}

func convertMessage(m oaMessage, toolNames map[string]string) (ollama.Message, error) {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	switch role {
	case "developer":
		role = "system"
	case "function":
		role = "tool"
	case "system", "user", "assistant", "tool":
	default:
		return ollama.Message{}, fmt.Errorf("unsupported role %q", m.Role)
	}
	text, images, err := convertContent(m.Content)
	if err != nil {
		return ollama.Message{}, err
	}
	msg := ollama.Message{Role: role, Content: text, Images: images}
	if role == "assistant" {
		msg.Thinking = m.ReasoningContent
		if msg.Thinking == "" {
			msg.Thinking = m.Reasoning
		}
		for _, tc := range m.ToolCalls {
			args, err := toolArguments(tc.Function.Arguments)
			if err != nil {
				return ollama.Message{}, fmt.Errorf("tool call %q: %v", tc.Function.Name, err)
			}
			if tc.ID != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
			msg.ToolCalls = append(msg.ToolCalls, ollama.ToolCall{
				ID:       tc.ID,
				Function: ollama.ToolCallFunction{Name: tc.Function.Name, Arguments: args},
			})
		}
	}
	if role == "tool" {
		msg.ToolCallID = m.ToolCallID
		msg.ToolName = toolNames[m.ToolCallID]
		if msg.ToolName == "" {
			msg.ToolName = m.Name
		}
	}
	return msg, nil
}

func toolArguments(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	if trimmed[0] == '{' {
		if !json.Valid(trimmed) {
			return nil, fmt.Errorf("arguments are not valid JSON")
		}
		return json.RawMessage(trimmed), nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON string or object")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, fmt.Errorf("arguments must encode a JSON object")
	}
	return json.RawMessage(s), nil
}

func convertContent(raw json.RawMessage) (string, []string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", nil, err
		}
		return s, nil, nil
	}
	if trimmed[0] != '[' {
		return "", nil, fmt.Errorf("content must be a string or an array of parts")
	}
	var parts []oaPart
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return "", nil, fmt.Errorf("invalid content parts: %v", err)
	}
	var texts []string
	var images []string
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			texts = append(texts, p.Text)
		case "image_url", "input_image":
			img, err := imageData(p.ImageURL)
			if err != nil {
				return "", nil, err
			}
			images = append(images, img)
		default:
			return "", nil, fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

func imageData(raw json.RawMessage) (string, error) {
	var url string
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		_ = json.Unmarshal(trimmed, &url)
	} else {
		var obj struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return "", fmt.Errorf("invalid image_url")
		}
		url = obj.URL
	}
	url = strings.TrimSpace(url)
	if !strings.HasPrefix(url, "data:") {
		return "", fmt.Errorf("only base64 data: image URLs are supported")
	}
	comma := strings.IndexByte(url, ',')
	if comma < 0 || !strings.Contains(url[:comma], ";base64") {
		return "", fmt.Errorf("image data URL must be base64 encoded")
	}
	return url[comma+1:], nil
}

func convertTools(raw json.RawMessage, choice json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var c string
	if json.Unmarshal(choice, &c) == nil && c == "none" {
		return nil, nil
	}
	var tools []map[string]any
	if err := json.Unmarshal(trimmed, &tools); err != nil {
		return nil, badRequest("tools must be an array: %v", err)
	}
	out := make([]map[string]any, 0, len(tools))
	for i, t := range tools {
		typ, _ := t["type"].(string)
		if typ != "" && typ != "function" {
			return nil, badRequest("tools[%d]: unsupported tool type %q", i, typ)
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			return nil, badRequest("tools[%d]: function is required", i)
		}
		if _, ok := fn["parameters"]; !ok {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	if len(out) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func convertStop(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, badRequest("invalid stop")
		}
		return []string{s}, nil
	}
	var list []string
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return nil, badRequest("stop must be a string or an array of strings")
	}
	return list, nil
}

func thinkValue(in oaRequest, opt Options) json.RawMessage {
	if trimmed := bytes.TrimSpace(in.Think); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(trimmed)
	}
	effort := strings.ToLower(strings.TrimSpace(in.ReasoningEffort))
	if effort == "" && in.Reasoning != nil {
		effort = strings.ToLower(strings.TrimSpace(in.Reasoning.Effort))
	}
	if effort == "" || !opt.Thinking {
		return nil
	}
	hasLevel := func(level string) bool {
		for _, l := range opt.ThinkLevels {
			if strings.EqualFold(l, level) {
				return true
			}
		}
		return false
	}
	switch effort {
	case "none":
		if len(opt.ThinkLevels) > 0 && !opt.ThinkFalse {
			return nil
		}
		return json.RawMessage(`false`)
	case "minimal":
		if hasLevel("low") {
			return json.RawMessage(`"low"`)
		}
		return json.RawMessage(`false`)
	case "xhigh", "max":
		effort = "high"
	}
	if hasLevel(effort) {
		raw, _ := json.Marshal(effort)
		return raw
	}
	if len(opt.ThinkLevels) > 0 && !opt.ThinkTrue {
		return nil
	}
	return json.RawMessage(`true`)
}

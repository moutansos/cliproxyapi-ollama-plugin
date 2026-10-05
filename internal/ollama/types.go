// Package ollama contains the subset of the native Ollama API used by the plugin.
package ollama

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// TagsResponse is the /api/tags body.
type TagsResponse struct {
	Models []TagModel `json:"models"`
}

// TagModel is one installed model from /api/tags.
type TagModel struct {
	Name         string       `json:"name"`
	Model        string       `json:"model"`
	ModifiedAt   string       `json:"modified_at"`
	Size         int64        `json:"size"`
	Digest       string       `json:"digest"`
	Details      ModelDetails `json:"details"`
	Capabilities []string     `json:"capabilities,omitempty"`
}

// UpstreamName returns the exact name/tag Ollama expects in requests.
func (m TagModel) UpstreamName() string {
	if name := strings.TrimSpace(m.Name); name != "" {
		return name
	}
	return strings.TrimSpace(m.Model)
}

// ModelDetails is the shared details object.
type ModelDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
	ContextLength     int      `json:"context_length,omitempty"`
}

// ShowResponse is the /api/show body (non-verbose).
type ShowResponse struct {
	Parameters   string                     `json:"parameters"`
	Details      ModelDetails               `json:"details"`
	ModelInfo    map[string]json.RawMessage `json:"model_info"`
	Capabilities []string                   `json:"capabilities"`
	ModifiedAt   string                     `json:"modified_at"`
	Thinking     *ThinkingInfo              `json:"thinking,omitempty"`
}

// ThinkingInfo lists the think values a model accepts (newer Ollama).
type ThinkingInfo struct {
	Values  []json.RawMessage `json:"values"`
	Default json.RawMessage   `json:"default"`
}

// PSResponse is the /api/ps body.
type PSResponse struct {
	Models []PSModel `json:"models"`
}

// PSModel is a loaded runner.
type PSModel struct {
	Name          string `json:"name"`
	Model         string `json:"model"`
	Digest        string `json:"digest"`
	ExpiresAt     string `json:"expires_at"`
	SizeVRAM      int64  `json:"size_vram"`
	ContextLength int    `json:"context_length"`
}

// ChatRequest is the /api/chat request body.
type ChatRequest struct {
	Model     string          `json:"model"`
	Messages  []Message       `json:"messages"`
	Tools     json.RawMessage `json:"tools,omitempty"`
	Format    json.RawMessage `json:"format,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
	Stream    bool            `json:"stream"`
	Think     json.RawMessage `json:"think,omitempty"`
	KeepAlive json.RawMessage `json:"keep_alive,omitempty"`
}

// Message is one chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"`
	Images     []string   `json:"images,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a native tool call.
type ToolCall struct {
	ID       string           `json:"id,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the function name and object arguments.
type ToolCallFunction struct {
	Index     *int            `json:"index,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ChatResponse is one /api/chat NDJSON event or the non-streaming body.
type ChatResponse struct {
	Model                 string  `json:"model"`
	CreatedAt             string  `json:"created_at"`
	Message               Message `json:"message"`
	Done                  bool    `json:"done"`
	DoneReason            string  `json:"done_reason,omitempty"`
	PromptEvalCount       int     `json:"prompt_eval_count,omitempty"`
	PromptEvalCachedCount int     `json:"prompt_eval_cached_count,omitempty"`
	EvalCount             int     `json:"eval_count,omitempty"`
	Error                 string  `json:"error,omitempty"`
}

// ErrorBody is Ollama's error envelope.
type ErrorBody struct {
	Error string `json:"error"`
}

// ParseParameterInt extracts an integer Modelfile parameter (for example
// num_ctx) from the /api/show "parameters" text block. The last occurrence wins.
func ParseParameterInt(parameters, name string) (int, bool) {
	found := false
	value := 0
	for _, line := range strings.Split(parameters, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != name {
			continue
		}
		raw := strings.Trim(fields[1], `"`)
		n, err := strconv.Atoi(raw)
		if err != nil {
			if f, errF := strconv.ParseFloat(raw, 64); errF == nil {
				n = int(f)
			} else {
				continue
			}
		}
		value = n
		found = true
	}
	return value, found
}

// ArchitectureContext returns the largest "<arch>.context_length" from model_info.
func ArchitectureContext(info map[string]json.RawMessage) int {
	best := 0
	for key, raw := range info {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil {
			continue
		}
		if int(n) > best {
			best = int(n)
		}
	}
	return best
}

// ThinkValues returns the string/bool think values advertised by /api/show.
func (t *ThinkingInfo) ThinkValues() (levels []string, supportsTrue bool, supportsFalse bool) {
	if t == nil {
		return nil, false, false
	}
	for _, raw := range t.Values {
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			if b {
				supportsTrue = true
			} else {
				supportsFalse = true
			}
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			levels = append(levels, s)
		}
	}
	return levels, supportsTrue, supportsFalse
}

// ParseTime parses Ollama RFC3339 timestamps, returning zero on failure.
func ParseTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return t
}

// HasCapability reports whether a capability list contains name.
func HasCapability(capabilities []string, name string) bool {
	for _, c := range capabilities {
		if strings.EqualFold(strings.TrimSpace(c), name) {
			return true
		}
	}
	return false
}

// Package modellist rewrites the OpenAI /v1/models response so this plugin's
// entries come from the live catalog snapshot and carry context metadata.
package modellist

import (
	"bytes"
	"encoding/json"
)

// Entry is one advertised model.
type Entry struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	Created             int64  `json:"created"`
	OwnedBy             string `json:"owned_by"`
	DisplayName         string `json:"display_name,omitempty"`
	ContextLength       int    `json:"context_length,omitempty"`
	MaxCompletionTokens int    `json:"max_completion_tokens,omitempty"`
}

// OwnedFunc reports whether an existing list entry belongs to this plugin.
type OwnedFunc func(id, ownedBy string) bool

type listEntryHead struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
}

// LooksLikeOpenAIModelList is a cheap pre-check before decoding.
func LooksLikeOpenAIModelList(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) > 0 && trimmed[0] == '{' && bytes.Contains(trimmed, []byte(`"data"`)) && bytes.Contains(trimmed, []byte(`"list"`))
}

// Enrich replaces this plugin's entries in an OpenAI model list with entries.
// Other providers' entries are preserved byte-for-byte and in order. It returns
// ok=false (and the input untouched) when body is not an OpenAI model list.
func Enrich(body []byte, owned OwnedFunc, entries []Entry) ([]byte, bool) {
	if !LooksLikeOpenAIModelList(body) {
		return body, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, false
	}
	var object string
	if err := json.Unmarshal(top["object"], &object); err != nil || object != "list" {
		return body, false
	}
	var data []json.RawMessage
	if err := json.Unmarshal(top["data"], &data); err != nil {
		return body, false
	}

	kept := make([]json.RawMessage, 0, len(data)+len(entries))
	insertAt := -1
	for _, item := range data {
		var head listEntryHead
		if err := json.Unmarshal(item, &head); err == nil && owned(head.ID, head.OwnedBy) {
			if insertAt < 0 {
				insertAt = len(kept)
			}
			continue
		}
		kept = append(kept, item)
	}
	if insertAt < 0 {
		insertAt = len(kept)
	}
	ours := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		if e.Object == "" {
			e.Object = "model"
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return body, false
		}
		ours = append(ours, raw)
	}
	merged := make([]json.RawMessage, 0, len(kept)+len(ours))
	merged = append(merged, kept[:insertAt]...)
	merged = append(merged, ours...)
	merged = append(merged, kept[insertAt:]...)

	newData, err := json.Marshal(merged)
	if err != nil {
		return body, false
	}
	top["data"] = newData
	out, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	return out, true
}

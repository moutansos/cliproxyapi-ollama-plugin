package modellist

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func owned(id, ownedBy string) bool {
	return ownedBy == "ollama" && strings.HasPrefix(id, "ollama/")
}

func TestEnrichReplacesOnlyOwnEntries(t *testing.T) {
	claude := `{"created":1700000000,"id":"claude-opus-5-5","object":"model","owned_by":"anthropic"}`
	gpt := `{"created":1700000001,"id":"gpt-5","object":"model","owned_by":"openai"}`
	stale := `{"created":1,"id":"ollama/removed:latest","object":"model","owned_by":"ollama"}`
	foreign := `{"created":2,"id":"ollama/lookalike","object":"model","owned_by":"someone-else"}`
	body := []byte(`{"data":[` + claude + `,` + stale + `,` + gpt + `,` + foreign + `],"object":"list"}`)

	out, ok := Enrich(body, owned, []Entry{
		{ID: "ollama/qwen3:8b", Created: 5, OwnedBy: "ollama", DisplayName: "qwen3:8b (Ollama)", ContextLength: 16384, MaxCompletionTokens: 2048},
		{ID: "ollama/llama3.2:3b", Created: 6, OwnedBy: "ollama", DisplayName: "llama3.2:3b (Ollama)"},
	})
	if !ok {
		t.Fatal("not enriched")
	}
	for _, original := range []string{claude, gpt, foreign} {
		if !bytes.Contains(out, []byte(original)) {
			t.Fatalf("other provider entry modified or dropped: %s\n%s", original, out)
		}
	}
	if bytes.Contains(out, []byte("removed:latest")) {
		t.Fatal("stale entry kept")
	}
	var parsed struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Object != "list" || len(parsed.Data) != 5 {
		t.Fatalf("data = %v", parsed.Data)
	}
	ids := []string{}
	for _, d := range parsed.Data {
		ids = append(ids, d["id"].(string))
	}
	want := "claude-opus-5-5,ollama/qwen3:8b,ollama/llama3.2:3b,gpt-5,ollama/lookalike"
	if strings.Join(ids, ",") != want {
		t.Fatalf("order = %v", ids)
	}
	q := parsed.Data[1]
	if q["context_length"].(float64) != 16384 || q["max_completion_tokens"].(float64) != 2048 || q["display_name"] != "qwen3:8b (Ollama)" || q["object"] != "model" {
		t.Fatalf("enriched entry = %v", q)
	}
	if _, ok := parsed.Data[2]["context_length"]; ok {
		t.Fatal("unknown context must be omitted, not zero")
	}
}

func TestEnrichAppendsWhenNoOwnEntries(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"a","object":"model","owned_by":"x"}]}`)
	out, ok := Enrich(body, owned, []Entry{{ID: "ollama/m", OwnedBy: "ollama"}})
	if !ok || !strings.Contains(string(out), `"id":"a"`) || !strings.HasSuffix(strings.TrimSpace(string(out)), `"object":"list"}`) {
		t.Fatalf("out = %s", out)
	}
	if strings.Index(string(out), `"id":"a"`) > strings.Index(string(out), `"ollama/m"`) {
		t.Fatal("own entries should be appended after existing ones")
	}
}

func TestEnrichIgnoresOtherBodies(t *testing.T) {
	for _, body := range []string{
		`{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`,
		`{"models":[{"slug":"gpt-5"}]}`,
		`{"data":[{"id":"claude"}],"has_more":false,"first_id":"list"}`,
		`not json`,
		``,
	} {
		out, ok := Enrich([]byte(body), owned, []Entry{{ID: "ollama/m"}})
		if ok || string(out) != body {
			t.Fatalf("modified non-list body %q -> %q", body, out)
		}
	}
}

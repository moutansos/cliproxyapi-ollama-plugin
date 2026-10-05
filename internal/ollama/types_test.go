package ollama

import (
	"encoding/json"
	"testing"
)

func TestParseParameterInt(t *testing.T) {
	params := "min_p                          0\nnum_ctx                        65536\nstop \"<|im_end|>\"\nnum_predict -1\nnum_ctx 32768\n"
	if n, ok := ParseParameterInt(params, "num_ctx"); !ok || n != 32768 {
		t.Fatalf("num_ctx = %d %v (last wins)", n, ok)
	}
	if n, ok := ParseParameterInt(params, "num_predict"); !ok || n != -1 {
		t.Fatalf("num_predict = %d %v", n, ok)
	}
	if _, ok := ParseParameterInt(params, "num_gpu"); ok {
		t.Fatal("missing parameter reported")
	}
}

func TestArchitectureContextAndThinking(t *testing.T) {
	var show ShowResponse
	raw := `{"model_info":{"general.architecture":"qwen35","qwen35.context_length":262144,"qwen35.block_count":64},
	"capabilities":["tools","thinking","completion"],"thinking":{"values":[true,false,"low","high"],"default":true}}`
	if err := json.Unmarshal([]byte(raw), &show); err != nil {
		t.Fatal(err)
	}
	if got := ArchitectureContext(show.ModelInfo); got != 262144 {
		t.Fatalf("arch ctx = %d", got)
	}
	levels, yes, no := show.Thinking.ThinkValues()
	if !yes || !no || len(levels) != 2 || levels[0] != "low" {
		t.Fatalf("think values = %v %v %v", levels, yes, no)
	}
	if !HasCapability(show.Capabilities, "Completion") {
		t.Fatal("capability match should be case-insensitive")
	}
}

func TestErrorFromBody(t *testing.T) {
	e := ErrorFromBody(404, []byte(`{"error":"model 'x' not found"}`))
	if e.StatusCode != 404 || e.Message != "model 'x' not found" {
		t.Fatalf("err = %+v", e)
	}
	if e := ErrorFromBody(502, []byte("bad gateway")); e.Message != "bad gateway" {
		t.Fatalf("plain body = %+v", e)
	}
}

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// fakeOllama implements the read-only discovery endpoints and /api/chat.
// /api/ps reports the context length of the last chat request, the way a
// real runner reflects the num_ctx it was loaded with.
type fakeOllama struct {
	mu        sync.Mutex
	options   map[string]any
	loadedCtx int
}

func newFakeOllama() *fakeOllama { return &fakeOllama{} }

func (f *fakeOllama) lastOptions() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{}
	for k, v := range f.options {
		out[k] = v
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case "/api/tags":
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{
			map[string]any{"name": "tiny:1b", "model": "tiny:1b", "digest": "d-tiny", "modified_at": "2026-09-01T10:00:00Z", "details": map[string]any{"family": "llama"}},
			map[string]any{"name": "embed:latest", "model": "embed:latest", "digest": "d-embed", "details": map[string]any{"family": "nomic-bert"}},
		}})
	case "/api/show":
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Model {
		case "tiny:1b":
			writeJSON(w, http.StatusOK, map[string]any{
				"parameters":   "num_ctx 4096\ntemperature 0.7",
				"capabilities": []string{"completion", "tools"},
				"model_info":   map[string]any{"general.architecture": "llama", "llama.context_length": 131072},
				"details":      map[string]any{"family": "llama"},
			})
		case "embed:latest":
			writeJSON(w, http.StatusOK, map[string]any{"capabilities": []string{"embedding"}, "model_info": map[string]any{"nomic-bert.context_length": 2048}})
		default:
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "model not found"})
		}
	case "/api/ps":
		f.mu.Lock()
		ctx := f.loadedCtx
		f.mu.Unlock()
		models := []any{}
		if ctx > 0 {
			models = append(models, map[string]any{"name": "tiny:1b", "model": "tiny:1b", "digest": "d-tiny", "context_length": ctx, "expires_at": time.Now().Add(time.Minute).Format(time.RFC3339)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": models})
	case "/api/chat":
		var req struct {
			Model   string         `json:"model"`
			Stream  bool           `json:"stream"`
			Options map[string]any `json:"options"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if req.Model != "tiny:1b" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "model '" + req.Model + "' not found"})
			return
		}
		ctx := 4096
		if v, ok := req.Options["num_ctx"].(float64); ok {
			ctx = int(v)
		}
		f.mu.Lock()
		f.options = req.Options
		f.loadedCtx = ctx
		f.mu.Unlock()
		done := map[string]any{"model": req.Model, "message": map[string]any{"role": "assistant", "content": ""}, "done": true, "done_reason": "stop", "prompt_eval_count": 5, "eval_count": 2}
		if !req.Stream {
			done["message"] = map[string]any{"role": "assistant", "content": "pong"}
			writeJSON(w, http.StatusOK, done)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		for _, piece := range []string{"po", "ng"} {
			_ = enc.Encode(map[string]any{"model": req.Model, "message": map[string]any{"role": "assistant", "content": piece}, "done": false})
			if flusher != nil {
				flusher.Flush()
			}
		}
		_ = enc.Encode(done)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected path " + r.URL.Path})
	}
}

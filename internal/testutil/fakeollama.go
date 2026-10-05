// Package testutil provides a fake Ollama server for tests (no live Ollama needed).
package testutil

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
)

// Call records one request received by the fake.
type Call struct {
	Method string
	Path   string
	Body   []byte
	Auth   string
}

// FakeOllama is a scriptable Ollama API.
type FakeOllama struct {
	Server *httptest.Server

	mu        sync.Mutex
	Tags      []ollama.TagModel
	Shows     map[string]json.RawMessage
	ShowFail  map[string]int
	PS        []ollama.PSModel
	TagsDown  bool
	PSDown    bool
	ChatFunc  func(w http.ResponseWriter, req ollama.ChatRequest)
	Calls     []Call
	showCount map[string]int
}

// NewFakeOllama starts a fake server and registers cleanup.
func NewFakeOllama(t testing.TB) *FakeOllama {
	t.Helper()
	f := &FakeOllama{Shows: map[string]json.RawMessage{}, ShowFail: map[string]int{}, showCount: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Server.Close)
	return f
}

// URL is the base URL of the fake.
func (f *FakeOllama) URL() string { return f.Server.URL }

// Set mutates state under the lock.
func (f *FakeOllama) Set(fn func(f *FakeOllama)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// ShowCount returns how many times /api/show was called for name.
func (f *FakeOllama) ShowCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.showCount[name]
}

// CallsTo returns recorded calls for a path.
func (f *FakeOllama) CallsTo(path string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.Calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// Paths returns every path requested.
func (f *FakeOllama) Paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.Calls))
	for _, c := range f.Calls {
		out = append(out, c.Path)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *FakeOllama) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.Calls = append(f.Calls, Call{Method: r.Method, Path: r.URL.Path, Body: body, Auth: r.Header.Get("Authorization")})
	f.mu.Unlock()

	switch r.URL.Path {
	case "/api/tags":
		f.mu.Lock()
		down, tags := f.TagsDown, append([]ollama.TagModel(nil), f.Tags...)
		f.mu.Unlock()
		if down {
			writeJSON(w, http.StatusServiceUnavailable, ollama.ErrorBody{Error: "server down"})
			return
		}
		writeJSON(w, http.StatusOK, ollama.TagsResponse{Models: tags})
	case "/api/show":
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.showCount[req.Model]++
		raw, ok := f.Shows[req.Model]
		fail := f.ShowFail[req.Model]
		f.mu.Unlock()
		if fail != 0 {
			writeJSON(w, fail, ollama.ErrorBody{Error: "show failed for " + req.Model})
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, ollama.ErrorBody{Error: "model '" + req.Model + "' not found"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	case "/api/ps":
		f.mu.Lock()
		down, ps := f.PSDown, append([]ollama.PSModel(nil), f.PS...)
		f.mu.Unlock()
		if down {
			writeJSON(w, http.StatusInternalServerError, ollama.ErrorBody{Error: "ps down"})
			return
		}
		writeJSON(w, http.StatusOK, ollama.PSResponse{Models: ps})
	case "/api/chat":
		var req ollama.ChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, ollama.ErrorBody{Error: err.Error()})
			return
		}
		f.mu.Lock()
		fn := f.ChatFunc
		f.mu.Unlock()
		if fn == nil {
			writeJSON(w, http.StatusNotFound, ollama.ErrorBody{Error: "no chat handler"})
			return
		}
		fn(w, req)
	default:
		writeJSON(w, http.StatusNotFound, ollama.ErrorBody{Error: "unexpected path " + r.URL.Path})
	}
}

// ShowJSON builds an /api/show body.
func ShowJSON(capabilities []string, parameters string, arch string, archCtx int) json.RawMessage {
	info := map[string]any{"general.architecture": arch}
	if archCtx > 0 {
		info[arch+".context_length"] = archCtx
	}
	raw, _ := json.Marshal(map[string]any{
		"parameters":   parameters,
		"details":      map[string]any{"family": arch, "format": "gguf"},
		"model_info":   info,
		"capabilities": capabilities,
		"modified_at":  "2026-09-01T10:00:00Z",
	})
	return raw
}

// NDJSON writes newline-delimited events, flushing after each.
func NDJSON(w http.ResponseWriter, events ...any) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, ev := range events {
		raw, _ := json.Marshal(ev)
		_, _ = w.Write(append(raw, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// MutatingPaths lists endpoints the plugin must never call.
var MutatingPaths = []string{"/api/pull", "/api/create", "/api/copy", "/api/delete", "/api/push", "/api/blobs"}

// AssertNoMutations fails if a model-mutating endpoint was called.
func (f *FakeOllama) AssertNoMutations(t testing.TB) {
	t.Helper()
	for _, p := range f.Paths() {
		for _, m := range MutatingPaths {
			if strings.HasPrefix(p, m) {
				t.Fatalf("plugin called mutating endpoint %s", p)
			}
		}
	}
}

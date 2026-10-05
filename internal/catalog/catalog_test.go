package catalog

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/config"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/testutil"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/upstream"
)

func fixture(t *testing.T) *testutil.FakeOllama {
	f := testutil.NewFakeOllama(t)
	f.Set(func(f *testutil.FakeOllama) {
		f.Tags = []ollama.TagModel{
			{Name: "qwen3:8b", Digest: "d-qwen-1", ModifiedAt: "2026-09-01T10:00:00Z", Details: ollama.ModelDetails{Family: "qwen3"}},
			{Name: "llama3.2:3b", Digest: "d-llama", Details: ollama.ModelDetails{Family: "llama"}},
			{Name: "nomic-embed-text:latest", Digest: "d-embed", Details: ollama.ModelDetails{Family: "nomic-bert"}},
			{Name: "broken:latest", Digest: "d-broken"},
		}
		f.Shows["qwen3:8b"] = testutil.ShowJSON([]string{"completion", "tools", "thinking"}, "num_ctx                        16384\nnum_predict 2048\nstop \"<|im_end|>\"", "qwen3", 40960)
		f.Shows["llama3.2:3b"] = testutil.ShowJSON([]string{"completion", "vision"}, "temperature 0.7", "llama", 131072)
		f.Shows["nomic-embed-text:latest"] = testutil.ShowJSON([]string{"embedding"}, "", "nomic-bert", 2048)
		f.ShowFail["broken:latest"] = http.StatusInternalServerError
		f.PS = []ollama.PSModel{{Name: "llama3.2:3b", Model: "llama3.2:3b", Digest: "d-llama", ContextLength: 4096}}
	})
	return f
}

func instance(base string) config.Instance {
	inst := config.Default().Instance
	inst.BaseURL = base
	return inst
}

func client(base string) ollama.Client {
	return ollama.Client{BaseURL: base, Transport: upstream.NewNetHTTP(nil)}
}

func TestDiscoveryParsingAndEmbeddingExclusion(t *testing.T) {
	f := fixture(t)
	d := NewDiscoverer(nil)
	if err := d.Refresh(context.Background(), client(f.URL()), instance(f.URL())); err != nil {
		t.Fatal(err)
	}
	snap := Build(d.State(), config.Default(), 1, time.Now())
	if len(snap.Models) != 2 {
		t.Fatalf("models = %d, excluded = %+v", len(snap.Models), snap.Excluded)
	}
	q, ok := snap.Lookup("ollama/qwen3:8b")
	if !ok || q.Upstream != "qwen3:8b" {
		t.Fatalf("lookup failed: %+v", q)
	}
	if q.Facts.ConfiguredNumCtx != 16384 || q.Facts.ConfiguredNumPredict != 2048 || q.Facts.DeclaredMax != 40960 {
		t.Fatalf("facts = %+v", q.Facts)
	}
	if !q.Tools || !q.Thinking || q.Vision {
		t.Fatalf("capabilities = %+v", q)
	}
	reasons := map[string]string{}
	for _, e := range snap.Excluded {
		reasons[e.Upstream] = e.Reason
	}
	if !strings.Contains(reasons["nomic-embed-text:latest"], "embedding") {
		t.Fatalf("embedding model not excluded as embedding: %v", reasons)
	}
	if !strings.Contains(reasons["broken:latest"], "details unavailable") {
		t.Fatalf("broken model reason: %v", reasons)
	}
	f.AssertNoMutations(t)
}

func TestDigestCaching(t *testing.T) {
	f := fixture(t)
	d := NewDiscoverer(nil)
	c, inst := client(f.URL()), instance(f.URL())
	for i := 0; i < 3; i++ {
		if err := d.Refresh(context.Background(), c, inst); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.ShowCount("qwen3:8b"); n != 1 {
		t.Fatalf("show called %d times for unchanged digest", n)
	}
	// Failed shows are retried on later passes.
	if n := f.ShowCount("broken:latest"); n != 3 {
		t.Fatalf("broken show called %d times", n)
	}
	f.Set(func(f *testutil.FakeOllama) { f.Tags[0].Digest = "d-qwen-2" })
	if err := d.Refresh(context.Background(), c, inst); err != nil {
		t.Fatal(err)
	}
	if n := f.ShowCount("qwen3:8b"); n != 2 {
		t.Fatalf("changed digest must trigger show; count = %d", n)
	}
}

func TestOutageRetainsLastSnapshot(t *testing.T) {
	f := fixture(t)
	d := NewDiscoverer(nil)
	c, inst := client(f.URL()), instance(f.URL())
	if err := d.Refresh(context.Background(), c, inst); err != nil {
		t.Fatal(err)
	}
	f.Set(func(f *testutil.FakeOllama) { f.TagsDown = true })
	if err := d.Refresh(context.Background(), c, inst); err == nil {
		t.Fatal("expected refresh error")
	}
	state := d.State()
	if state.LastError == "" {
		t.Fatal("last error not recorded")
	}
	snap := Build(state, config.Default(), 2, time.Now())
	if len(snap.Models) != 2 || !snap.Ready() {
		t.Fatalf("outage dropped models: %d", len(snap.Models))
	}

	// A network-level outage (server gone) also retains state.
	f.Server.Close()
	if err := d.Refresh(context.Background(), c, inst); err == nil {
		t.Fatal("expected error with server closed")
	}
	if snap := Build(d.State(), config.Default(), 3, time.Now()); len(snap.Models) != 2 {
		t.Fatal("models lost after connection failure")
	}
}

func TestIncludeExcludeSkipsShow(t *testing.T) {
	f := fixture(t)
	d := NewDiscoverer(nil)
	inst := instance(f.URL())
	inst.ExcludeModels = []string{"llama*"}
	if err := d.Refresh(context.Background(), client(f.URL()), inst); err != nil {
		t.Fatal(err)
	}
	if f.ShowCount("llama3.2:3b") != 0 {
		t.Fatal("excluded model was inspected")
	}
	cfg := config.Default()
	cfg.Instance = inst
	if _, ok := Build(d.State(), cfg, 1, time.Now()).Lookup("ollama/llama3.2:3b"); ok {
		t.Fatal("excluded model advertised")
	}
}

func TestObservedContextFromPS(t *testing.T) {
	f := fixture(t)
	d := NewDiscoverer(nil)
	if err := d.Refresh(context.Background(), client(f.URL()), instance(f.URL())); err != nil {
		t.Fatal(err)
	}
	snap := Build(d.State(), config.Default(), 1, time.Now())
	l, _ := snap.Lookup("ollama/llama3.2:3b")
	if l.Resolution.Source != SourceObserved || l.Resolution.ContextLength != 4096 || l.Resolution.ObservedAt == nil {
		t.Fatalf("llama resolution = %+v", l.Resolution)
	}
	q, _ := snap.Lookup("ollama/qwen3:8b")
	if q.Resolution.Source != SourceConfigured || q.Resolution.ContextLength != 16384 {
		t.Fatalf("qwen resolution = %+v", q.Resolution)
	}
	if q.Resolution.MaxCompletionTokens != 2048 {
		t.Fatalf("max completion = %d", q.Resolution.MaxCompletionTokens)
	}
}

func TestResolveObserve(t *testing.T) {
	now := time.Now()
	obs := &Observation{ContextLength: 8192, ObservedAt: now}
	f := Facts{DeclaredMax: 32768, ConfiguredNumCtx: 16384}
	eff := config.Effective{Policy: config.PolicyObserve, FallbackContextLength: 4096}

	if r := Resolve(f, obs, eff, nil); r.Source != SourceObserved || r.ContextLength != 8192 {
		t.Fatalf("observed: %+v", r)
	}
	if r := Resolve(f, nil, eff, nil); r.Source != SourceConfigured || r.ContextLength != 16384 {
		t.Fatalf("configured: %+v", r)
	}
	if r := Resolve(Facts{DeclaredMax: 32768}, nil, eff, nil); r.Source != SourceFallback || r.ContextLength != 4096 {
		t.Fatalf("fallback: %+v", r)
	}
	if r := Resolve(Facts{DeclaredMax: 2048}, nil, eff, nil); r.Source != SourceFallback || r.ContextLength != 2048 || !r.CappedToDeclaredMax {
		t.Fatalf("fallback capped: %+v", r)
	}
	r := Resolve(Facts{DeclaredMax: 131072}, nil, config.Effective{Policy: config.PolicyObserve}, nil)
	if r.Source != SourceUnknown || r.ContextLength != 0 {
		t.Fatalf("unknown must not advertise the architecture maximum: %+v", r)
	}
	if r.DeclaredMax != 131072 {
		t.Fatal("declared max must remain available for diagnostics")
	}
}

func TestResolveManaged(t *testing.T) {
	f := Facts{DeclaredMax: 8192, ConfiguredNumCtx: 4096}
	eff := config.Effective{Policy: config.PolicyManaged, NumCtx: 16384, NumPredict: 20000, KeepAlive: "5m0s"}
	r := Resolve(f, nil, eff, nil)
	if r.Source != SourceManaged || r.ContextLength != 8192 || r.ManagedNumCtx != 8192 || !r.CappedToDeclaredMax || r.RequestedNumCtx != 16384 {
		t.Fatalf("managed capped: %+v", r)
	}
	if r.MaxCompletionTokens != 8192 || r.NumPredictCeiling != 20000 {
		t.Fatalf("num_predict ceiling: %+v", r)
	}

	eff.NumCtx = 4096
	eff.NumPredict = 1024
	ok := Resolve(f, nil, eff, &Verification{Expected: 4096, Observed: 4096, Loaded: true})
	if !ok.Verified || ok.ContextLength != 4096 || ok.Discrepancy != "" {
		t.Fatalf("verified: %+v", ok)
	}
	small := Resolve(f, nil, eff, &Verification{Expected: 4096, Observed: 2048, Loaded: true})
	if small.ContextLength != 2048 || small.Discrepancy == "" || small.Verified {
		t.Fatalf("smaller allocation must be advertised and flagged: %+v", small)
	}
	stale := Resolve(f, nil, eff, &Verification{Expected: 9999, Observed: 2048, Loaded: true})
	if stale.ContextLength != 4096 || stale.Verification != nil {
		t.Fatalf("verification for an old target must be ignored: %+v", stale)
	}
	other := Resolve(f, &Observation{ContextLength: 2048, ObservedAt: time.Now()}, eff, nil)
	if other.ContextLength != 4096 || other.Discrepancy == "" {
		t.Fatalf("runner loaded by another client: %+v", other)
	}
}

func TestResolveInherit(t *testing.T) {
	cfg := config.Parse([]byte("default_context_policy: managed\ndefault_num_ctx: 4096\nmodels: {qwen3:8b: {policy: inherit}}")).Config
	r := Resolve(Facts{DeclaredMax: 40960}, nil, cfg.EffectiveFor("qwen3:8b"), nil)
	if r.Policy != config.PolicyManaged || r.ContextLength != 4096 {
		t.Fatalf("inherit managed: %+v", r)
	}
	cfg = config.Parse([]byte("models: {qwen3:8b: {policy: inherit}}")).Config
	r = Resolve(Facts{ConfiguredNumCtx: 2048}, nil, cfg.EffectiveFor("qwen3:8b"), nil)
	if r.Policy != config.PolicyObserve || r.Source != SourceConfigured {
		t.Fatalf("inherit observe: %+v", r)
	}
}

func TestClaims(t *testing.T) {
	snap := Build(State{Facts: map[string]Facts{}}, config.Default(), 1, time.Now())
	if !snap.Claims("ollama/anything:tag") || snap.Claims("gpt-5") || snap.Claims("ollama/") {
		t.Fatal("prefix claim rules wrong")
	}
}

func TestVerifyManaged(t *testing.T) {
	f := fixture(t)
	d := NewDiscoverer(nil)
	c, inst := client(f.URL()), instance(f.URL())
	if err := d.Refresh(context.Background(), c, inst); err != nil {
		t.Fatal(err)
	}
	f.Set(func(f *testutil.FakeOllama) {
		f.PS = []ollama.PSModel{{Name: "qwen3:8b", Digest: "d-qwen-1", ContextLength: 2048}}
	})
	v, err := d.VerifyManaged(context.Background(), c, inst, "qwen3:8b", 8192)
	if err != nil || !v.Loaded || v.Observed != 2048 {
		t.Fatalf("verify = %+v, %v", v, err)
	}
	cfg := config.Parse([]byte("models: {qwen3:8b: {policy: managed, num_ctx: 8192}}")).Config
	cfg.Instance = inst
	q, _ := Build(d.State(), cfg, 1, time.Now()).Lookup("ollama/qwen3:8b")
	if q.Resolution.ContextLength != 2048 || q.Resolution.Discrepancy == "" {
		t.Fatalf("discrepancy not advertised: %+v", q.Resolution)
	}
}

func TestClaimsWithBarePrefix(t *testing.T) {
	cfg := config.Parse([]byte("model_prefix: local")).Config
	snap := Build(State{Facts: map[string]Facts{}}, cfg, 1, time.Now())
	if !snap.Claims("local/qwen3:8b") {
		t.Fatal("namespace with added separator not claimed")
	}
	if snap.Claims("localqwen3:8b") || snap.Claims("local/") || snap.Claims("local") {
		t.Fatal("IDs without the separator must not be claimed")
	}
	none := Build(State{Facts: map[string]Facts{}}, config.Parse([]byte("model_prefix: ''")).Config, 1, time.Now())
	if none.Claims("qwen3:8b") {
		t.Fatal("with no prefix only advertised IDs may be claimed")
	}
}

func TestDefaultDisplayNameIncludesPrefix(t *testing.T) {
	facts := State{Facts: map[string]Facts{
		"qwen3:8b": {Upstream: "qwen3:8b", ShowOK: true, Capabilities: []string{"completion"}},
	}, Order: []string{"qwen3:8b"}}
	cases := map[string]string{
		"model_prefix: local": "local/qwen3:8b",
		"":                    "ollama/qwen3:8b",
		"model_prefix: ''":    "qwen3:8b (Ollama)",
		"model_prefix: local\nmodels: {qwen3:8b: {display_name: Coder}}": "Coder",
	}
	for yamlText, want := range cases {
		cfg := config.Parse([]byte(yamlText)).Config
		m := Build(facts, cfg, 1, time.Now()).Models[0]
		if m.DisplayName != want {
			t.Errorf("%q: display name = %q, want %q", yamlText, m.DisplayName, want)
		}
	}
}

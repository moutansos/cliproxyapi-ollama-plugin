package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	res := Parse(nil)
	if len(res.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", res.Issues)
	}
	c := res.Config
	if c.DefaultPolicy != PolicyObserve || c.Instance.ModelPrefix != "ollama" || c.Instance.PublicID("m:1") != "ollama/m:1" || c.Instance.BaseURL != DefaultBaseURL {
		t.Fatalf("bad defaults: %+v", c)
	}
	if !c.VerifyManagedAllocation || c.RefreshInterval != time.Minute {
		t.Fatalf("bad defaults: %+v", c)
	}
}

func TestParseFullConfig(t *testing.T) {
	yamlText := `
enabled: true
priority: 5
base_url: http://192.0.2.10:11434/v1/
api_key: secret
model_prefix: local/
refresh_interval_seconds: "30"
request_timeout_seconds: 5
default_context_policy: observe
default_num_ctx: 8192
default_num_predict: 1024
default_keep_alive: 10m
fallback_context_length: 4096
verify_managed_allocation: false
include_models: ["qwen*", "llama3.2:3b"]
exclude_models: "*embed*, bad:tag"
models:
  qwen3:8b:
    policy: managed
    num_ctx: 16384
    num_predict: 512
    keep_alive: 300
  llama3.2:3b:
    policy: observe
    fallback_context_length: 2048
    display_name: Llama Small
`
	res := Parse([]byte(yamlText))
	if res.HasFatal() {
		t.Fatalf("unexpected fatal: %v", res.Issues)
	}
	c := res.Config
	if c.Instance.BaseURL != "http://192.0.2.10:11434" {
		t.Fatalf("base url = %q", c.Instance.BaseURL)
	}
	if c.Instance.APIKey != "secret" || c.Instance.ModelPrefix != "local" || c.Instance.PublicID("qwen3:8b") != "local/qwen3:8b" {
		t.Fatalf("instance = %+v", c.Instance)
	}
	if c.RefreshInterval != 30*time.Second || c.Instance.RequestTimeout != 5*time.Second {
		t.Fatalf("durations = %v %v", c.RefreshInterval, c.Instance.RequestTimeout)
	}
	if c.DefaultKeepAlive != "10m0s" || c.FallbackContextLength != 4096 || c.VerifyManagedAllocation {
		t.Fatalf("defaults = %+v", c)
	}
	if len(c.Instance.IncludeModels) != 2 || len(c.Instance.ExcludeModels) != 2 || c.Instance.ExcludeModels[1] != "bad:tag" {
		t.Fatalf("patterns = %v %v", c.Instance.IncludeModels, c.Instance.ExcludeModels)
	}
	q := c.Models["qwen3:8b"]
	if q.Policy != PolicyManaged || q.NumCtx != 16384 || q.NumPredict != 512 || q.KeepAlive != "5m0s" {
		t.Fatalf("qwen override = %+v", q)
	}
	l := c.Models["llama3.2:3b"]
	if l.Policy != PolicyObserve || l.FallbackContextLength != 2048 || l.DisplayName != "Llama Small" {
		t.Fatalf("llama override = %+v", l)
	}
	if !c.Instance.Included("qwen3:8b") || c.Instance.Included("nomic-embed-text:latest") || c.Instance.Included("mistral:7b") {
		t.Fatal("include/exclude rules not applied")
	}
}

func TestModelsAsJSONString(t *testing.T) {
	res := Parse([]byte(`models: '{"qwen3:8b": {"policy": "managed", "num_ctx": 4096}}'`))
	if res.HasFatal() {
		t.Fatalf("fatal: %v", res.Issues)
	}
	if res.Config.Models["qwen3:8b"].NumCtx != 4096 {
		t.Fatalf("models = %+v", res.Config.Models)
	}
}

func TestFatalIssues(t *testing.T) {
	cases := map[string]string{
		"bad scheme":    "base_url: ftp://host",
		"creds":         "base_url: http://user:pw@host:11434",
		"bad prefix":    "model_prefix: 'a b'",
		"nested prefix": "model_prefix: a/b",
		"interval":      "refresh_interval_seconds: 1",
		"policy":        "default_context_policy: inherit",
		"num_ctx small": "default_num_ctx: 10",
		"keep_alive":    "default_keep_alive: forever",
		"pattern":       "include_models: ['[']",
		"models type":   "models: [1, 2]",
		"not mapping":   "- a",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if res := Parse([]byte(text)); !res.HasFatal() {
				t.Fatalf("expected fatal issue for %q, got %v", text, res.Issues)
			}
		})
	}
}

func TestPerModelValidation(t *testing.T) {
	yamlText := `
default_context_policy: observe
models:
  good:latest: {policy: managed, num_ctx: 8192}
  badpolicy:latest: {policy: turbo}
  unknownfield:latest: {policy: managed, num_ctx: 8192, temperature: 1}
  observewithctx:latest: {policy: observe, num_ctx: 8192}
  tiny:latest: {policy: managed, num_ctx: 12}
  managedfallback:latest: {policy: managed, num_ctx: 8192, fallback_context_length: 4096}
  badkeepalive:latest: {policy: managed, num_ctx: 8192, keep_alive: soon}
`
	res := Parse([]byte(yamlText))
	if res.HasFatal() {
		t.Fatalf("per-model issues must not be fatal: %v", res.Issues)
	}
	if _, ok := res.Config.Models["good:latest"]; !ok {
		t.Fatal("valid override dropped")
	}
	for _, name := range []string{"badpolicy:latest", "unknownfield:latest", "observewithctx:latest", "tiny:latest", "managedfallback:latest", "badkeepalive:latest"} {
		if _, ok := res.Config.Models[name]; ok {
			t.Errorf("invalid override %s kept", name)
		}
		found := false
		for _, issue := range res.Issues {
			if issue.Model == name {
				found = true
			}
		}
		if !found {
			t.Errorf("no issue reported for %s", name)
		}
	}
}

func TestEffectiveFor(t *testing.T) {
	res := Parse([]byte(`
default_context_policy: managed
default_num_ctx: 4096
default_num_predict: 256
default_keep_alive: 2m
fallback_context_length: 2048
models:
  a:1: {policy: inherit, num_ctx: 8192}
  b:1: {policy: observe, fallback_context_length: 1024}
  c:1: {num_predict: 64}
`))
	if res.HasFatal() {
		t.Fatalf("fatal: %v", res.Issues)
	}
	c := res.Config
	a := c.EffectiveFor("a:1")
	if a.Policy != PolicyManaged || a.NumCtx != 8192 || a.NumPredict != 256 || a.KeepAlive != "2m0s" {
		t.Fatalf("inherit override = %+v", a)
	}
	b := c.EffectiveFor("b:1")
	if b.Policy != PolicyObserve || b.FallbackContextLength != 1024 || b.NumCtx != 0 {
		t.Fatalf("observe override = %+v", b)
	}
	cc := c.EffectiveFor("c:1")
	if cc.Policy != PolicyManaged || cc.NumCtx != 4096 || cc.NumPredict != 64 {
		t.Fatalf("partial override = %+v", cc)
	}
	d := c.EffectiveFor("unlisted:1")
	if d.Policy != PolicyManaged || d.NumCtx != 4096 {
		t.Fatalf("default managed = %+v", d)
	}

	noCtx := Parse([]byte("default_context_policy: managed")).Config.EffectiveFor("x:1")
	if noCtx.Policy != PolicyObserve || !strings.Contains(noCtx.Problem, "num_ctx") {
		t.Fatalf("managed without num_ctx must fall back to observe with a problem: %+v", noCtx)
	}

	obs := Parse([]byte("models: {x:1: {num_ctx: 8192}}")).Config.EffectiveFor("x:1")
	if obs.Policy != PolicyObserve || obs.NumCtx != 0 || obs.Problem == "" {
		t.Fatalf("inherit->observe must ignore managed fields and flag it: %+v", obs)
	}
}

func TestNormalizeKeepAlive(t *testing.T) {
	cases := map[string]string{"": "", "5m": "5m0s", "90": "1m30s", "-1": "-1", "-5m": "-1", "0": "0s"}
	for in, want := range cases {
		got, err := NormalizeKeepAlive(in)
		if err != nil || got != want {
			t.Errorf("NormalizeKeepAlive(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeKeepAlive("later"); err == nil {
		t.Error("expected error")
	}
}

func TestUnknownTopLevelFieldIsWarningOnly(t *testing.T) {
	res := Parse([]byte("surprise: 1"))
	if res.HasFatal() || len(res.Issues) != 1 {
		t.Fatalf("issues = %v", res.Issues)
	}
}

func TestModelPrefixFollowsCPAConvention(t *testing.T) {
	cases := map[string]string{
		"local":     "local/qwen3:8b",
		"local/":    "local/qwen3:8b",
		" /local/ ": "local/qwen3:8b",
		"":          "qwen3:8b",
		"/":         "qwen3:8b",
	}
	for in, want := range cases {
		res := Parse([]byte("model_prefix: '" + in + "'"))
		if res.HasFatal() {
			t.Fatalf("prefix %q rejected: %v", in, res.Issues)
		}
		if got := res.Config.Instance.PublicID("qwen3:8b"); got != want {
			t.Errorf("prefix %q: PublicID = %q, want %q", in, got, want)
		}
	}
	if got := Parse(nil).Config.Instance.Namespace(); got != "ollama/" {
		t.Fatalf("default namespace = %q", got)
	}
}

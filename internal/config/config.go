// Package config parses and validates plugins.configs.cliproxyapi-ollama.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Policy is a per-model context policy.
type Policy string

const (
	PolicyInherit Policy = "inherit"
	PolicyObserve Policy = "observe"
	PolicyManaged Policy = "managed"
)

const (
	DefaultBaseURL              = "http://127.0.0.1:11434"
	DefaultModelPrefix          = "ollama"
	DefaultRefreshInterval      = 60 * time.Second
	DefaultRequestTimeout       = 10 * time.Second
	MinRefreshInterval          = 10 * time.Second
	MaxRefreshInterval          = 24 * time.Hour
	MinRequestTimeout           = 1 * time.Second
	MaxRequestTimeout           = 5 * time.Minute
	MaxContextTokens            = 16 * 1024 * 1024
	MinNumCtx                   = 256
	DefaultInstanceID           = "default"
	defaultVerifyManagedEnabled = true
	maxModelOverrideKeyLength   = 512
	maxMatchPatternLength       = 512
	maxDisplayNameLength        = 200
	maxPrefixLength             = 64
	maxAPIKeyLength             = 4096
	maxKeepAliveStringLength    = 32
	keepAliveForeverSentinel    = "-1"
)

// ModelOverride is one entry of the per-model "models" map, keyed by the exact
// upstream Ollama model name/tag.
type ModelOverride struct {
	Policy                Policy `json:"policy,omitempty"`
	NumCtx                int    `json:"num_ctx,omitempty"`
	NumPredict            int    `json:"num_predict,omitempty"`
	KeepAlive             string `json:"keep_alive,omitempty"`
	FallbackContextLength int    `json:"fallback_context_length,omitempty"`
	DisplayName           string `json:"display_name,omitempty"`
}

// Instance describes one Ollama endpoint. Version 1 supports exactly one.
type Instance struct {
	ID             string
	BaseURL        string
	APIKey         string
	ModelPrefix    string
	IncludeModels  []string
	ExcludeModels  []string
	RequestTimeout time.Duration
}

// Config is the validated plugin configuration.
type Config struct {
	Instance                Instance
	RefreshInterval         time.Duration
	DefaultPolicy           Policy
	DefaultNumCtx           int
	DefaultNumPredict       int
	DefaultKeepAlive        string
	FallbackContextLength   int
	VerifyManagedAllocation bool
	Models                  map[string]ModelOverride
}

// Default returns the configuration used when no plugin config is present.
func Default() Config {
	return Config{
		Instance: Instance{
			ID:             DefaultInstanceID,
			BaseURL:        DefaultBaseURL,
			ModelPrefix:    DefaultModelPrefix,
			RequestTimeout: DefaultRequestTimeout,
		},
		RefreshInterval:         DefaultRefreshInterval,
		DefaultPolicy:           PolicyObserve,
		VerifyManagedAllocation: defaultVerifyManagedEnabled,
		Models:                  map[string]ModelOverride{},
	}
}

// Issue is a validation problem. Fatal issues reject the whole config; model
// issues only drop that model's override.
type Issue struct {
	Field   string `json:"field"`
	Model   string `json:"model,omitempty"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

func (i Issue) String() string {
	if i.Model != "" {
		return fmt.Sprintf("%s[%q]: %s", i.Field, i.Model, i.Message)
	}
	return fmt.Sprintf("%s: %s", i.Field, i.Message)
}

// Result is the outcome of Parse.
type Result struct {
	Config Config
	Issues []Issue
}

// HasFatal reports whether any issue rejects the configuration.
func (r Result) HasFatal() bool {
	for _, issue := range r.Issues {
		if issue.Fatal {
			return true
		}
	}
	return false
}

// raw mirrors the YAML document. Loosely typed fields are decoded by hand so
// CPAMC JSON textareas (objects, arrays, or JSON strings) are all accepted.
type raw struct {
	Enabled                 *bool     `yaml:"enabled"`
	Priority                *int      `yaml:"priority"`
	BaseURL                 *string   `yaml:"base_url"`
	APIKey                  *string   `yaml:"api_key"`
	ModelPrefix             *string   `yaml:"model_prefix"`
	RefreshIntervalSeconds  yaml.Node `yaml:"refresh_interval_seconds"`
	RequestTimeoutSeconds   yaml.Node `yaml:"request_timeout_seconds"`
	DefaultContextPolicy    *string   `yaml:"default_context_policy"`
	DefaultNumCtx           yaml.Node `yaml:"default_num_ctx"`
	DefaultNumPredict       yaml.Node `yaml:"default_num_predict"`
	DefaultKeepAlive        yaml.Node `yaml:"default_keep_alive"`
	FallbackContextLength   yaml.Node `yaml:"fallback_context_length"`
	VerifyManagedAllocation *bool     `yaml:"verify_managed_allocation"`
	IncludeModels           yaml.Node `yaml:"include_models"`
	ExcludeModels           yaml.Node `yaml:"exclude_models"`
	Models                  yaml.Node `yaml:"models"`
	Store                   yaml.Node `yaml:"store"`
}

var knownTopLevelKeys = map[string]struct{}{
	"enabled": {}, "priority": {}, "base_url": {}, "api_key": {}, "model_prefix": {},
	"refresh_interval_seconds": {}, "request_timeout_seconds": {}, "default_context_policy": {},
	"default_num_ctx": {}, "default_num_predict": {}, "default_keep_alive": {},
	"fallback_context_length": {}, "verify_managed_allocation": {}, "include_models": {},
	"exclude_models": {}, "models": {}, "store": {},
}

// Parse decodes and validates the plugin YAML handed over by the host.
func Parse(data []byte) Result {
	cfg := Default()
	var issues []Issue
	fatal := func(field, format string, args ...any) {
		issues = append(issues, Issue{Field: field, Message: fmt.Sprintf(format, args...), Fatal: true})
	}

	if strings.TrimSpace(string(data)) == "" {
		return Result{Config: cfg}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		fatal("config", "invalid YAML: %v", err)
		return Result{Config: cfg, Issues: issues}
	}
	if len(doc.Content) == 0 {
		return Result{Config: cfg}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		fatal("config", "plugin config must be a mapping")
		return Result{Config: cfg, Issues: issues}
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		if _, ok := knownTopLevelKeys[key]; !ok {
			issues = append(issues, Issue{Field: key, Message: "unknown field (ignored)"})
		}
	}
	var r raw
	if err := root.Decode(&r); err != nil {
		fatal("config", "decode: %v", err)
		return Result{Config: cfg, Issues: issues}
	}

	if r.BaseURL != nil {
		baseURL, err := normalizeBaseURL(*r.BaseURL)
		if err != nil {
			fatal("base_url", "%v", err)
		} else {
			cfg.Instance.BaseURL = baseURL
		}
	}
	if r.APIKey != nil {
		key := strings.TrimSpace(*r.APIKey)
		if len(key) > maxAPIKeyLength || strings.ContainsAny(key, "\r\n") {
			fatal("api_key", "must be a single line of at most %d characters", maxAPIKeyLength)
		} else {
			cfg.Instance.APIKey = key
		}
	}
	if r.ModelPrefix != nil {
		prefix := NormalizePrefix(*r.ModelPrefix)
		if err := validatePrefix(prefix); err != nil {
			fatal("model_prefix", "%v", err)
		} else {
			cfg.Instance.ModelPrefix = prefix
		}
	}
	if v, ok, err := intNode(&r.RefreshIntervalSeconds); err != nil {
		fatal("refresh_interval_seconds", "%v", err)
	} else if ok {
		d := time.Duration(v) * time.Second
		if d < MinRefreshInterval || d > MaxRefreshInterval {
			fatal("refresh_interval_seconds", "must be between %d and %d", int(MinRefreshInterval.Seconds()), int(MaxRefreshInterval.Seconds()))
		} else {
			cfg.RefreshInterval = d
		}
	}
	if v, ok, err := intNode(&r.RequestTimeoutSeconds); err != nil {
		fatal("request_timeout_seconds", "%v", err)
	} else if ok {
		d := time.Duration(v) * time.Second
		if d < MinRequestTimeout || d > MaxRequestTimeout {
			fatal("request_timeout_seconds", "must be between %d and %d", int(MinRequestTimeout.Seconds()), int(MaxRequestTimeout.Seconds()))
		} else {
			cfg.Instance.RequestTimeout = d
		}
	}
	if r.DefaultContextPolicy != nil {
		p := Policy(strings.ToLower(strings.TrimSpace(*r.DefaultContextPolicy)))
		switch p {
		case "":
		case PolicyObserve, PolicyManaged:
			cfg.DefaultPolicy = p
		default:
			fatal("default_context_policy", "must be observe or managed")
		}
	}
	if v, ok, err := intNode(&r.DefaultNumCtx); err != nil {
		fatal("default_num_ctx", "%v", err)
	} else if ok && v != 0 {
		if err := validateNumCtx(v); err != nil {
			fatal("default_num_ctx", "%v", err)
		} else {
			cfg.DefaultNumCtx = v
		}
	}
	if v, ok, err := intNode(&r.DefaultNumPredict); err != nil {
		fatal("default_num_predict", "%v", err)
	} else if ok && v != 0 {
		if err := validateNumPredict(v); err != nil {
			fatal("default_num_predict", "%v", err)
		} else {
			cfg.DefaultNumPredict = v
		}
	}
	if ka, ok, err := keepAliveNode(&r.DefaultKeepAlive); err != nil {
		fatal("default_keep_alive", "%v", err)
	} else if ok {
		cfg.DefaultKeepAlive = ka
	}
	if v, ok, err := intNode(&r.FallbackContextLength); err != nil {
		fatal("fallback_context_length", "%v", err)
	} else if ok && v != 0 {
		if err := validateNumCtx(v); err != nil {
			fatal("fallback_context_length", "%v", err)
		} else {
			cfg.FallbackContextLength = v
		}
	}
	if r.VerifyManagedAllocation != nil {
		cfg.VerifyManagedAllocation = *r.VerifyManagedAllocation
	}
	if patterns, err := patternList(&r.IncludeModels); err != nil {
		fatal("include_models", "%v", err)
	} else {
		cfg.Instance.IncludeModels = patterns
	}
	if patterns, err := patternList(&r.ExcludeModels); err != nil {
		fatal("exclude_models", "%v", err)
	} else {
		cfg.Instance.ExcludeModels = patterns
	}

	models, modelIssues, err := parseModels(&r.Models)
	if err != nil {
		fatal("models", "%v", err)
	} else {
		cfg.Models = models
	}
	issues = append(issues, modelIssues...)

	if cfg.DefaultPolicy == PolicyManaged && cfg.DefaultNumCtx == 0 {
		issues = append(issues, Issue{Field: "default_num_ctx", Message: "default_context_policy is managed but default_num_ctx is unset; models without their own num_ctx fall back to observe"})
	}

	return Result{Config: cfg, Issues: issues}
}

func normalizeBaseURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("must not be empty")
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("host is required")
	}
	if u.User != nil {
		return "", fmt.Errorf("credentials in the URL are not allowed; use api_key")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("query strings and fragments are not allowed")
	}
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, "/v1")
	p = strings.TrimSuffix(p, "/api")
	u.Path = p
	return strings.TrimRight(u.String(), "/"), nil
}

// NormalizePrefix follows CLIProxyAPI's own model-prefix convention
// (internal/config normalizeModelPrefix): surrounding spaces and slashes are
// trimmed and the separating "/" is added when IDs are built, so "ollama",
// "ollama/" and "/ollama/" are equivalent.
func NormalizePrefix(prefix string) string {
	return strings.Trim(strings.TrimSpace(prefix), "/")
}

func validatePrefix(prefix string) error {
	if len(prefix) > maxPrefixLength {
		return fmt.Errorf("must be at most %d characters", maxPrefixLength)
	}
	if strings.Contains(prefix, "/") {
		return fmt.Errorf("must be a single segment without '/' (CLIProxyAPI adds the separator: prefix \"ollama\" gives ollama/<model>)")
	}
	for _, r := range prefix {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("may only contain letters, digits, '-', '_' and '.'")
		}
	}
	return nil
}

func validateNumCtx(v int) error {
	if v < MinNumCtx || v > MaxContextTokens {
		return fmt.Errorf("must be between %d and %d", MinNumCtx, MaxContextTokens)
	}
	return nil
}

func validateNumPredict(v int) error {
	if v < 1 || v > MaxContextTokens {
		return fmt.Errorf("must be between 1 and %d", MaxContextTokens)
	}
	return nil
}

// intNode decodes an integer from a YAML scalar, accepting numeric strings
// because CPAMC may submit form values as strings.
func intNode(node *yaml.Node) (int, bool, error) {
	if node == nil || node.Kind == 0 {
		return 0, false, nil
	}
	if node.Kind != yaml.ScalarNode {
		return 0, false, fmt.Errorf("must be an integer")
	}
	if node.Tag == "!!null" {
		return 0, false, nil
	}
	value := strings.TrimSpace(node.Value)
	if value == "" {
		return 0, false, nil
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil && f == float64(int64(f)) {
		return int(f), true, nil
	}
	return 0, false, fmt.Errorf("must be an integer, got %q", value)
}

// keepAliveNode accepts a Go duration ("5m", "1h30m"), an integer number of
// seconds, or "-1" (keep loaded indefinitely). It returns the value in the
// form Ollama accepts for keep_alive.
func keepAliveNode(node *yaml.Node) (string, bool, error) {
	if node == nil || node.Kind == 0 || node.Tag == "!!null" {
		return "", false, nil
	}
	if node.Kind != yaml.ScalarNode {
		return "", false, fmt.Errorf("must be a duration string or integer seconds")
	}
	ka, err := NormalizeKeepAlive(node.Value)
	if err != nil {
		return "", false, err
	}
	return ka, ka != "", nil
}

// NormalizeKeepAlive validates a keep_alive value.
func NormalizeKeepAlive(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maxKeepAliveStringLength {
		return "", fmt.Errorf("keep_alive is too long")
	}
	if n, err := strconv.Atoi(value); err == nil {
		if n < 0 {
			return keepAliveForeverSentinel, nil
		}
		return (time.Duration(n) * time.Second).String(), nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return "", fmt.Errorf("keep_alive must be a duration like 5m or integer seconds")
	}
	if d < 0 {
		return keepAliveForeverSentinel, nil
	}
	return d.String(), nil
}

// patternList accepts a YAML sequence, a JSON array string, or a comma
// separated string of glob patterns (path.Match syntax).
func patternList(node *yaml.Node) ([]string, error) {
	if node == nil || node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	var values []string
	switch node.Kind {
	case yaml.SequenceNode:
		if err := node.Decode(&values); err != nil {
			return nil, fmt.Errorf("must be a list of strings")
		}
	case yaml.ScalarNode:
		text := strings.TrimSpace(node.Value)
		if text == "" {
			return nil, nil
		}
		if strings.HasPrefix(text, "[") {
			if err := json.Unmarshal([]byte(text), &values); err != nil {
				return nil, fmt.Errorf("invalid JSON array: %v", err)
			}
		} else {
			values = strings.Split(text, ",")
		}
	default:
		return nil, fmt.Errorf("must be a list of strings")
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if len(v) > maxMatchPatternLength {
			return nil, fmt.Errorf("pattern %q is too long", v)
		}
		if _, err := path.Match(v, ""); err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %v", v, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// parseModels accepts a YAML mapping or a JSON object string.
func parseModels(node *yaml.Node) (map[string]ModelOverride, []Issue, error) {
	out := map[string]ModelOverride{}
	if node == nil || node.Kind == 0 || node.Tag == "!!null" {
		return out, nil, nil
	}
	var entries map[string]any
	switch node.Kind {
	case yaml.MappingNode:
		if err := node.Decode(&entries); err != nil {
			return nil, nil, fmt.Errorf("must be an object keyed by upstream model name: %v", err)
		}
	case yaml.ScalarNode:
		text := strings.TrimSpace(node.Value)
		if text == "" {
			return out, nil, nil
		}
		if err := json.Unmarshal([]byte(text), &entries); err != nil {
			return nil, nil, fmt.Errorf("must be a JSON object keyed by upstream model name: %v", err)
		}
	default:
		return nil, nil, fmt.Errorf("must be an object keyed by upstream model name")
	}

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	var issues []Issue
	for _, name := range names {
		key := strings.TrimSpace(name)
		if key == "" || len(key) > maxModelOverrideKeyLength || strings.ContainsAny(key, "\r\n\t ") {
			issues = append(issues, Issue{Field: "models", Model: name, Message: "invalid upstream model name"})
			continue
		}
		override, problems := parseOverride(entries[name])
		if len(problems) > 0 {
			for _, p := range problems {
				issues = append(issues, Issue{Field: "models", Model: key, Message: p + "; override ignored"})
			}
			continue
		}
		if _, dup := out[key]; dup {
			issues = append(issues, Issue{Field: "models", Model: key, Message: "duplicate override; later entry ignored"})
			continue
		}
		out[key] = override
	}
	return out, issues, nil
}

var knownOverrideKeys = map[string]struct{}{
	"policy": {}, "num_ctx": {}, "num_predict": {}, "keep_alive": {},
	"fallback_context_length": {}, "display_name": {},
}

func parseOverride(value any) (ModelOverride, []string) {
	var o ModelOverride
	var problems []string
	m, ok := value.(map[string]any)
	if !ok {
		if value == nil {
			return ModelOverride{Policy: PolicyInherit}, nil
		}
		return o, []string{"override must be an object"}
	}
	for key := range m {
		if _, known := knownOverrideKeys[key]; !known {
			problems = append(problems, fmt.Sprintf("unknown field %q", key))
		}
	}
	if v, present := m["policy"]; present && v != nil {
		s, isString := v.(string)
		p := Policy(strings.ToLower(strings.TrimSpace(s)))
		switch {
		case !isString:
			problems = append(problems, "policy must be a string")
		case p == "":
			o.Policy = PolicyInherit
		case p == PolicyInherit || p == PolicyObserve || p == PolicyManaged:
			o.Policy = p
		default:
			problems = append(problems, "policy must be inherit, observe, or managed")
		}
	} else {
		o.Policy = PolicyInherit
	}
	intField := func(name string, validate func(int) error) int {
		v, present := m[name]
		if !present || v == nil {
			return 0
		}
		n, err := anyInt(v)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s %v", name, err))
			return 0
		}
		if n == 0 {
			return 0
		}
		if err := validate(n); err != nil {
			problems = append(problems, fmt.Sprintf("%s %v", name, err))
			return 0
		}
		return n
	}
	o.NumCtx = intField("num_ctx", validateNumCtx)
	o.NumPredict = intField("num_predict", validateNumPredict)
	o.FallbackContextLength = intField("fallback_context_length", validateNumCtx)
	if v, present := m["keep_alive"]; present && v != nil {
		var text string
		switch t := v.(type) {
		case string:
			text = t
		case int, int64, float64:
			n, err := anyInt(t)
			if err != nil {
				problems = append(problems, "keep_alive "+err.Error())
			}
			text = strconv.Itoa(n)
		default:
			problems = append(problems, "keep_alive must be a duration string or integer seconds")
		}
		ka, err := NormalizeKeepAlive(text)
		if err != nil {
			problems = append(problems, err.Error())
		}
		o.KeepAlive = ka
	}
	if v, present := m["display_name"]; present && v != nil {
		s, isString := v.(string)
		s = strings.TrimSpace(s)
		if !isString || len(s) > maxDisplayNameLength || strings.ContainsAny(s, "\r\n") {
			problems = append(problems, fmt.Sprintf("display_name must be a single-line string of at most %d characters", maxDisplayNameLength))
		} else {
			o.DisplayName = s
		}
	}
	if o.Policy != PolicyManaged {
		if o.NumCtx != 0 || o.NumPredict != 0 || o.KeepAlive != "" {
			if o.Policy == PolicyObserve {
				problems = append(problems, "num_ctx, num_predict and keep_alive are only valid with policy managed (or inherit when the default policy is managed)")
			}
		}
	}
	if o.Policy == PolicyManaged && o.FallbackContextLength != 0 {
		problems = append(problems, "fallback_context_length only applies to observe")
	}
	return o, problems
}

func anyInt(v any) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		if t != float64(int64(t)) {
			return 0, fmt.Errorf("must be an integer")
		}
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("must be an integer")
		}
		return n, nil
	default:
		return 0, fmt.Errorf("must be an integer")
	}
}

// Effective is the resolved per-model policy after applying inheritance.
type Effective struct {
	Policy                Policy
	NumCtx                int
	NumPredict            int
	KeepAlive             string
	FallbackContextLength int
	DisplayName           string
	Problem               string
}

// EffectiveFor resolves the policy for one upstream model name.
func (c Config) EffectiveFor(upstream string) Effective {
	o, hasOverride := c.Models[upstream]
	policy := c.DefaultPolicy
	if hasOverride && o.Policy != "" && o.Policy != PolicyInherit {
		policy = o.Policy
	}
	eff := Effective{Policy: policy, FallbackContextLength: c.FallbackContextLength}
	if hasOverride {
		eff.DisplayName = o.DisplayName
		if o.FallbackContextLength != 0 {
			eff.FallbackContextLength = o.FallbackContextLength
		}
	}
	if policy != PolicyManaged {
		if hasOverride && (o.NumCtx != 0 || o.NumPredict != 0 || o.KeepAlive != "") {
			eff.Problem = "num_ctx/num_predict/keep_alive are ignored because the policy resolves to observe"
		}
		return eff
	}
	eff.NumCtx = c.DefaultNumCtx
	eff.NumPredict = c.DefaultNumPredict
	eff.KeepAlive = c.DefaultKeepAlive
	if hasOverride {
		if o.NumCtx != 0 {
			eff.NumCtx = o.NumCtx
		}
		if o.NumPredict != 0 {
			eff.NumPredict = o.NumPredict
		}
		if o.KeepAlive != "" {
			eff.KeepAlive = o.KeepAlive
		}
	}
	if eff.NumCtx == 0 {
		return Effective{
			Policy:                PolicyObserve,
			FallbackContextLength: eff.FallbackContextLength,
			DisplayName:           eff.DisplayName,
			Problem:               "managed policy requires num_ctx (per model or default_num_ctx); using observe",
		}
	}
	return eff
}

// Included reports whether the include/exclude rules admit an upstream model.
func (i Instance) Included(upstream string) bool {
	if len(i.IncludeModels) > 0 {
		matched := false
		for _, p := range i.IncludeModels {
			if globMatch(p, upstream) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, p := range i.ExcludeModels {
		if globMatch(p, upstream) {
			return false
		}
	}
	return true
}

func globMatch(pattern, name string) bool {
	if pattern == name {
		return true
	}
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// Namespace is the ID prefix including the separator ("ollama/"), or "" when
// no prefix is configured.
func (i Instance) Namespace() string {
	if i.ModelPrefix == "" {
		return ""
	}
	return i.ModelPrefix + "/"
}

// PublicID maps an upstream model name to the advertised model ID.
func (i Instance) PublicID(upstream string) string {
	return i.Namespace() + upstream
}

// Summary is a secret-free view for diagnostics.
func (c Config) Summary() map[string]any {
	return map[string]any{
		"instance_id":               c.Instance.ID,
		"base_url":                  c.Instance.BaseURL,
		"api_key_configured":        c.Instance.APIKey != "",
		"model_prefix":              c.Instance.ModelPrefix,
		"include_models":            c.Instance.IncludeModels,
		"exclude_models":            c.Instance.ExcludeModels,
		"request_timeout_seconds":   int(c.Instance.RequestTimeout.Seconds()),
		"refresh_interval_seconds":  int(c.RefreshInterval.Seconds()),
		"default_context_policy":    c.DefaultPolicy,
		"default_num_ctx":           c.DefaultNumCtx,
		"default_num_predict":       c.DefaultNumPredict,
		"default_keep_alive":        c.DefaultKeepAlive,
		"fallback_context_length":   c.FallbackContextLength,
		"verify_managed_allocation": c.VerifyManagedAllocation,
		"models":                    c.Models,
	}
}

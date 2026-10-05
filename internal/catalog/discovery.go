// Package catalog discovers Ollama models, resolves their context policy, and
// publishes immutable snapshots shared by listing, routing, and execution.
package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/config"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
)

// Facts is what discovery learned about one installed model.
type Facts struct {
	Upstream             string              `json:"upstream"`
	Digest               string              `json:"digest,omitempty"`
	ModifiedAt           string              `json:"modified_at,omitempty"`
	Details              ollama.ModelDetails `json:"details"`
	Capabilities         []string            `json:"capabilities,omitempty"`
	DeclaredMax          int                 `json:"declared_max_context,omitempty"`
	ConfiguredNumCtx     int                 `json:"configured_num_ctx,omitempty"`
	ConfiguredNumPredict int                 `json:"configured_num_predict,omitempty"`
	ThinkLevels          []string            `json:"think_levels,omitempty"`
	ThinkTrue            bool                `json:"think_true,omitempty"`
	ThinkFalse           bool                `json:"think_false,omitempty"`
	ShowOK               bool                `json:"show_ok"`
	ShowError            string              `json:"show_error,omitempty"`
	ShowAt               time.Time           `json:"show_at,omitempty"`
	FilteredByConfig     bool                `json:"filtered_by_config,omitempty"`
}

// State is the raw discovery state. It survives temporary outages.
type State struct {
	Facts         map[string]Facts        `json:"-"`
	Order         []string                `json:"-"`
	Observations  map[string]Observation  `json:"-"`
	Verifications map[string]Verification `json:"-"`
	RefreshedAt   time.Time               `json:"refreshed_at"`
	PSObservedAt  time.Time               `json:"ps_observed_at"`
	LastAttempt   time.Time               `json:"last_attempt"`
	LastError     string                  `json:"last_error,omitempty"`
	PSError       string                  `json:"ps_error,omitempty"`
	Refreshes     uint64                  `json:"refreshes"`
	ShowCalls     uint64                  `json:"show_calls"`
}

func (s State) clone() State {
	out := s
	out.Facts = make(map[string]Facts, len(s.Facts))
	for k, v := range s.Facts {
		out.Facts[k] = v
	}
	out.Order = append([]string(nil), s.Order...)
	out.Observations = make(map[string]Observation, len(s.Observations))
	for k, v := range s.Observations {
		out.Observations[k] = v
	}
	out.Verifications = make(map[string]Verification, len(s.Verifications))
	for k, v := range s.Verifications {
		out.Verifications[k] = v
	}
	return out
}

// Discoverer owns discovery state and the digest-keyed /api/show cache.
type Discoverer struct {
	refreshMu sync.Mutex
	mu        sync.Mutex
	state     State
	showCache map[string]Facts
	now       func() time.Time
}

// NewDiscoverer returns an empty Discoverer.
func NewDiscoverer(now func() time.Time) *Discoverer {
	if now == nil {
		now = time.Now
	}
	return &Discoverer{
		state:     State{Facts: map[string]Facts{}, Observations: map[string]Observation{}, Verifications: map[string]Verification{}},
		showCache: map[string]Facts{},
		now:       now,
	}
}

// State returns a copy of the current state.
func (d *Discoverer) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state.clone()
}

// Reset drops all state (used when the instance base URL changes).
func (d *Discoverer) Reset() {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = State{Facts: map[string]Facts{}, Observations: map[string]Observation{}, Verifications: map[string]Verification{}}
	d.showCache = map[string]Facts{}
}

func showCacheKey(name, digest string) string {
	return name + "@" + digest
}

// Refresh performs one discovery pass: /api/tags, /api/show for new or changed
// digests, then /api/ps. On a tags failure the previous facts are retained.
func (d *Discoverer) Refresh(ctx context.Context, client ollama.Client, inst config.Instance) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()

	started := d.now()
	tagsCtx, cancel := context.WithTimeout(ctx, inst.RequestTimeout)
	tags, err := client.Tags(tagsCtx)
	cancel()
	if err != nil {
		d.mu.Lock()
		d.state.LastAttempt = started
		d.state.LastError = fmt.Sprintf("list models: %v", err)
		d.mu.Unlock()
		return err
	}

	d.mu.Lock()
	cache := make(map[string]Facts, len(d.showCache))
	for k, v := range d.showCache {
		cache[k] = v
	}
	prevFacts := d.state.Facts
	d.mu.Unlock()

	facts := make(map[string]Facts, len(tags.Models))
	order := make([]string, 0, len(tags.Models))
	nextCache := make(map[string]Facts, len(tags.Models))
	var showCalls uint64
	for _, tag := range tags.Models {
		name := tag.UpstreamName()
		if name == "" {
			continue
		}
		if _, dup := facts[name]; dup {
			continue
		}
		order = append(order, name)
		base := Facts{
			Upstream:     name,
			Digest:       tag.Digest,
			ModifiedAt:   tag.ModifiedAt,
			Details:      tag.Details,
			Capabilities: append([]string(nil), tag.Capabilities...),
			DeclaredMax:  tag.Details.ContextLength,
		}
		if !inst.Included(name) {
			base.FilteredByConfig = true
			facts[name] = base
			continue
		}
		key := showCacheKey(name, tag.Digest)
		if cached, ok := cache[key]; ok && cached.ShowOK {
			facts[name] = cached
			nextCache[key] = cached
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		showCalls++
		showCtx, cancelShow := context.WithTimeout(ctx, inst.RequestTimeout)
		show, errShow := client.Show(showCtx, name)
		cancelShow()
		if errShow != nil {
			if prev, ok := prevFacts[name]; ok && prev.ShowOK && prev.Digest == tag.Digest {
				prev.ShowError = errShow.Error()
				facts[name] = prev
				continue
			}
			base.ShowError = errShow.Error()
			base.ShowAt = d.now()
			facts[name] = base
			continue
		}
		f := mergeShow(base, show)
		f.ShowAt = d.now()
		facts[name] = f
		nextCache[key] = f
	}

	psCtx, cancelPS := context.WithTimeout(ctx, inst.RequestTimeout)
	ps, errPS := client.PS(psCtx)
	cancelPS()
	now := d.now()

	d.mu.Lock()
	defer d.mu.Unlock()
	d.state.Facts = facts
	d.state.Order = order
	d.showCache = nextCache
	d.state.RefreshedAt = now
	d.state.LastAttempt = started
	d.state.LastError = ""
	d.state.Refreshes++
	d.state.ShowCalls += showCalls
	for name := range d.state.Verifications {
		if _, ok := facts[name]; !ok {
			delete(d.state.Verifications, name)
		}
	}
	if errPS != nil {
		d.state.PSError = fmt.Sprintf("list running models: %v", errPS)
	} else {
		d.applyPSLocked(ps, now)
	}
	return nil
}

func (d *Discoverer) applyPSLocked(ps ollama.PSResponse, now time.Time) {
	obs := make(map[string]Observation, len(ps.Models))
	for _, m := range ps.Models {
		o := Observation{ContextLength: m.ContextLength, ObservedAt: now, ExpiresAt: m.ExpiresAt}
		for _, name := range []string{m.Name, m.Model} {
			name = strings.TrimSpace(name)
			if name != "" {
				obs[name] = o
			}
		}
	}
	// Runners can be listed under a different alias of the same blob; match by digest.
	for name, f := range d.state.Facts {
		if _, ok := obs[name]; ok || f.Digest == "" {
			continue
		}
		for _, m := range ps.Models {
			if m.Digest == f.Digest {
				obs[name] = Observation{ContextLength: m.ContextLength, ObservedAt: now, ExpiresAt: m.ExpiresAt}
				break
			}
		}
	}
	d.state.Observations = obs
	d.state.PSObservedAt = now
	d.state.PSError = ""
}

// ObserveRunners refreshes only /api/ps.
func (d *Discoverer) ObserveRunners(ctx context.Context, client ollama.Client, inst config.Instance) error {
	psCtx, cancel := context.WithTimeout(ctx, inst.RequestTimeout)
	ps, err := client.PS(psCtx)
	cancel()
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.state.PSError = fmt.Sprintf("list running models: %v", err)
		return err
	}
	d.applyPSLocked(ps, d.now())
	return nil
}

// VerifyManaged refreshes /api/ps and records whether upstream is loaded with
// the expected managed num_ctx.
func (d *Discoverer) VerifyManaged(ctx context.Context, client ollama.Client, inst config.Instance, upstream string, expected int) (Verification, error) {
	if err := d.ObserveRunners(ctx, client, inst); err != nil {
		return Verification{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	v := Verification{Expected: expected, VerifiedAt: d.now()}
	if o, ok := d.state.Observations[upstream]; ok {
		v.Loaded = true
		v.Observed = o.ContextLength
	}
	d.state.Verifications[upstream] = v
	return v, nil
}

func mergeShow(base Facts, show ollama.ShowResponse) Facts {
	f := base
	f.ShowOK = true
	f.ShowError = ""
	if len(show.Capabilities) > 0 {
		f.Capabilities = append([]string(nil), show.Capabilities...)
	}
	if arch := ollama.ArchitectureContext(show.ModelInfo); arch > 0 {
		f.DeclaredMax = arch
	} else if show.Details.ContextLength > 0 && f.DeclaredMax == 0 {
		f.DeclaredMax = show.Details.ContextLength
	}
	if f.Details.Family == "" {
		f.Details = show.Details
	}
	if n, ok := ollama.ParseParameterInt(show.Parameters, "num_ctx"); ok && n > 0 {
		f.ConfiguredNumCtx = n
	}
	if n, ok := ollama.ParseParameterInt(show.Parameters, "num_predict"); ok && n > 0 {
		f.ConfiguredNumPredict = n
	}
	f.ThinkLevels, f.ThinkTrue, f.ThinkFalse = show.Thinking.ThinkValues()
	return f
}

// Classify decides whether a model is completion-capable and servable.
func Classify(f Facts) (bool, string) {
	if f.FilteredByConfig {
		return false, "excluded by include_models/exclude_models"
	}
	if !f.ShowOK {
		if f.ShowError != "" {
			return false, "model details unavailable: " + f.ShowError
		}
		return false, "model details not yet inspected"
	}
	caps := f.Capabilities
	if len(caps) > 0 {
		if ollama.HasCapability(caps, "completion") {
			return true, ""
		}
		if ollama.HasCapability(caps, "embedding") {
			return false, "embedding-only model"
		}
		return false, "no completion capability (" + strings.Join(caps, ", ") + ")"
	}
	family := strings.ToLower(f.Details.Family)
	name := strings.ToLower(f.Upstream)
	if strings.Contains(family, "bert") || strings.Contains(name, "embed") {
		return false, "embedding model (heuristic; Ollama reported no capabilities)"
	}
	return true, ""
}

// SortedNames returns the discovered names in a stable order.
func (s State) SortedNames() []string {
	names := append([]string(nil), s.Order...)
	sort.Strings(names)
	return names
}

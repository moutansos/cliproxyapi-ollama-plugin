package catalog

import (
	"sort"
	"strings"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/config"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
)

// Model is one servable model in a snapshot.
type Model struct {
	ID          string           `json:"id"`
	Upstream    string           `json:"upstream"`
	InstanceID  string           `json:"instance_id"`
	DisplayName string           `json:"display_name"`
	Created     int64            `json:"created"`
	Vision      bool             `json:"vision"`
	Tools       bool             `json:"tools"`
	Thinking    bool             `json:"thinking"`
	Facts       Facts            `json:"facts"`
	Effective   config.Effective `json:"-"`
	Resolution  Resolution       `json:"resolution"`
}

// Excluded is a discovered model that is not advertised.
type Excluded struct {
	Upstream string `json:"upstream"`
	Reason   string `json:"reason"`
}

// Snapshot is an immutable view used for listing, routing, and execution.
type Snapshot struct {
	Generation  uint64
	BuiltAt     time.Time
	Config      config.Config
	ConfigIssue []config.Issue
	State       State
	Models      []Model
	Excluded    []Excluded
	byID        map[string]int
}

// Build derives a snapshot from discovery state and configuration. It does no I/O.
func Build(state State, cfg config.Config, generation uint64, now time.Time) *Snapshot {
	s := &Snapshot{Generation: generation, BuiltAt: now, Config: cfg, State: state, byID: map[string]int{}}
	for _, name := range state.SortedNames() {
		f, ok := state.Facts[name]
		if !ok {
			continue
		}
		include, reason := Classify(f)
		if !include {
			s.Excluded = append(s.Excluded, Excluded{Upstream: name, Reason: reason})
			continue
		}
		id := cfg.Instance.PublicID(name)
		if _, dup := s.byID[id]; dup {
			s.Excluded = append(s.Excluded, Excluded{Upstream: name, Reason: "duplicate public model ID " + id})
			continue
		}
		eff := cfg.EffectiveFor(name)
		var obs *Observation
		if o, ok := state.Observations[name]; ok {
			o := o
			obs = &o
		}
		var ver *Verification
		if v, ok := state.Verifications[name]; ok {
			v := v
			ver = &v
		}
		display := DefaultDisplayName(cfg.Instance, name)
		if eff.DisplayName != "" {
			display = eff.DisplayName
		}
		created := ollama.ParseTime(f.ModifiedAt).Unix()
		if created < 0 {
			created = 0
		}
		m := Model{
			ID:          id,
			Upstream:    name,
			InstanceID:  cfg.Instance.ID,
			DisplayName: display,
			Created:     created,
			Vision:      ollama.HasCapability(f.Capabilities, "vision"),
			Tools:       ollama.HasCapability(f.Capabilities, "tools"),
			Thinking:    ollama.HasCapability(f.Capabilities, "thinking"),
			Facts:       f,
			Effective:   eff,
			Resolution:  Resolve(f, obs, eff, ver),
		}
		s.byID[id] = len(s.Models)
		s.Models = append(s.Models, m)
	}
	sort.SliceStable(s.Excluded, func(i, j int) bool { return s.Excluded[i].Upstream < s.Excluded[j].Upstream })
	return s
}

// DefaultDisplayName is the public ID when a prefix is configured, so clients
// that show display names (OpenCode's picker) still show and match the prefix
// the operator chose. Without a prefix it marks the model as coming from Ollama.
func DefaultDisplayName(inst config.Instance, upstream string) string {
	if inst.ModelPrefix != "" {
		return inst.PublicID(upstream)
	}
	return upstream + " (Ollama)"
}

// Lookup returns the model advertised under a public ID.
func (s *Snapshot) Lookup(id string) (Model, bool) {
	if s == nil {
		return Model{}, false
	}
	idx, ok := s.byID[strings.TrimSpace(id)]
	if !ok {
		return Model{}, false
	}
	return s.Models[idx], true
}

// Claims reports whether a requested model ID belongs to this plugin: either an
// advertised ID or any ID inside the configured prefix namespace.
func (s *Snapshot) Claims(id string) bool {
	if s == nil {
		return false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if _, ok := s.byID[id]; ok {
		return true
	}
	prefix := s.Config.Instance.Namespace()
	return prefix != "" && strings.HasPrefix(id, prefix) && len(id) > len(prefix)
}

// Ready reports whether at least one discovery pass has succeeded.
func (s *Snapshot) Ready() bool {
	return s != nil && !s.State.RefreshedAt.IsZero()
}

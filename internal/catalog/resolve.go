package catalog

import (
	"fmt"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/config"
)

// ContextSource records where an advertised context length came from.
type ContextSource string

const (
	// SourceObserved is the allocation reported by /api/ps for a loaded runner.
	SourceObserved ContextSource = "observed"
	// SourceConfigured is a num_ctx parameter baked into the model (/api/show).
	SourceConfigured ContextSource = "configured"
	// SourceFallback is the operator-provided fallback_context_length.
	SourceFallback ContextSource = "fallback"
	// SourceUnknown means no deployment-accurate value is available.
	SourceUnknown ContextSource = "unknown"
	// SourceManaged is the plugin-managed num_ctx injected into every request.
	SourceManaged ContextSource = "managed"
)

// Observation is a loaded-runner snapshot from /api/ps.
type Observation struct {
	ContextLength int       `json:"context_length"`
	ObservedAt    time.Time `json:"observed_at"`
	ExpiresAt     string    `json:"expires_at,omitempty"`
}

// Verification is the result of checking a managed allocation with /api/ps.
type Verification struct {
	Expected   int       `json:"expected_num_ctx"`
	Observed   int       `json:"observed_context_length"`
	Loaded     bool      `json:"loaded"`
	VerifiedAt time.Time `json:"verified_at"`
}

// Resolution is the effective, advertised context for one model.
type Resolution struct {
	Policy              config.Policy `json:"policy"`
	ContextLength       int           `json:"context_length"`
	Source              ContextSource `json:"context_source"`
	DeclaredMax         int           `json:"declared_max_context,omitempty"`
	ConfiguredNumCtx    int           `json:"configured_num_ctx,omitempty"`
	ObservedContext     int           `json:"observed_context_length,omitempty"`
	ObservedAt          *time.Time    `json:"observed_at,omitempty"`
	ManagedNumCtx       int           `json:"managed_num_ctx,omitempty"`
	RequestedNumCtx     int           `json:"requested_num_ctx,omitempty"`
	CappedToDeclaredMax bool          `json:"capped_to_declared_max,omitempty"`
	NumPredictCeiling   int           `json:"num_predict_ceiling,omitempty"`
	KeepAlive           string        `json:"keep_alive,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	Verified            bool          `json:"verified,omitempty"`
	Verification        *Verification `json:"verification,omitempty"`
	Discrepancy         string        `json:"discrepancy,omitempty"`
	Problem             string        `json:"problem,omitempty"`
}

func capTo(value, max int) (int, bool) {
	if max > 0 && value > max {
		return max, true
	}
	return value, false
}

// Resolve computes the advertised context for one model.
//
// observe: observed (/api/ps) > configured num_ctx (/api/show) > fallback > unknown.
// The architecture maximum is never advertised as a runtime allocation; it
// only caps the other values because Ollama clamps num_ctx to it.
//
// managed: the configured num_ctx capped to the declared maximum. A verified
// smaller allocation replaces the advertised value and is flagged.
func Resolve(f Facts, obs *Observation, eff config.Effective, ver *Verification) Resolution {
	r := Resolution{
		Policy:           eff.Policy,
		DeclaredMax:      f.DeclaredMax,
		ConfiguredNumCtx: f.ConfiguredNumCtx,
		Problem:          eff.Problem,
	}
	if obs != nil && obs.ContextLength > 0 {
		r.ObservedContext = obs.ContextLength
		at := obs.ObservedAt
		r.ObservedAt = &at
	}

	if eff.Policy == config.PolicyManaged {
		r.RequestedNumCtx = eff.NumCtx
		r.ManagedNumCtx, r.CappedToDeclaredMax = capTo(eff.NumCtx, f.DeclaredMax)
		r.ContextLength = r.ManagedNumCtx
		r.Source = SourceManaged
		r.KeepAlive = eff.KeepAlive
		r.NumPredictCeiling = eff.NumPredict
		if ver != nil && ver.Expected == r.ManagedNumCtx {
			v := *ver
			r.Verification = &v
			switch {
			case !ver.Loaded:
				r.Discrepancy = "model was not loaded when the managed allocation was checked"
			case ver.Observed == ver.Expected:
				r.Verified = true
			case ver.Observed > 0 && ver.Observed < ver.Expected:
				r.ContextLength = ver.Observed
				r.Discrepancy = fmt.Sprintf("Ollama allocated %d tokens instead of managed num_ctx %d; advertising the smaller value", ver.Observed, ver.Expected)
			case ver.Observed > ver.Expected:
				r.Discrepancy = fmt.Sprintf("Ollama reported %d tokens for managed num_ctx %d", ver.Observed, ver.Expected)
			}
		}
		if !r.Verified && r.ObservedContext == r.ManagedNumCtx {
			r.Verified = true
		}
		if r.Discrepancy == "" && r.ObservedContext > 0 && r.ObservedContext != r.ManagedNumCtx {
			r.Discrepancy = fmt.Sprintf("currently loaded with %d tokens (another client); the next managed request reloads it with num_ctx %d", r.ObservedContext, r.ManagedNumCtx)
		}
	} else {
		switch {
		case r.ObservedContext > 0:
			r.ContextLength = r.ObservedContext
			r.Source = SourceObserved
		case f.ConfiguredNumCtx > 0:
			r.ContextLength, r.CappedToDeclaredMax = capTo(f.ConfiguredNumCtx, f.DeclaredMax)
			r.Source = SourceConfigured
		case eff.FallbackContextLength > 0:
			r.ContextLength, r.CappedToDeclaredMax = capTo(eff.FallbackContextLength, f.DeclaredMax)
			r.Source = SourceFallback
		default:
			r.Source = SourceUnknown
		}
	}

	switch {
	case r.NumPredictCeiling > 0:
		r.MaxCompletionTokens = r.NumPredictCeiling
	case f.ConfiguredNumPredict > 0:
		r.MaxCompletionTokens = f.ConfiguredNumPredict
	}
	if r.ContextLength > 0 && r.MaxCompletionTokens > r.ContextLength {
		r.MaxCompletionTokens = r.ContextLength
	}
	return r
}

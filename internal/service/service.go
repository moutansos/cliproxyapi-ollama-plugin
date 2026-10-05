// Package service implements the plugin capabilities on top of the catalog.
package service

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/catalog"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/config"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/ollama"
	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/upstream"
)

// Provider is the executor/provider identifier and the owned_by value of
// advertised models.
const Provider = "ollama"

// Host is everything the service needs from CLIProxyAPI.
type Host interface {
	upstream.Transport
	// EmitStream sends one translated chunk to the host-owned output stream.
	EmitStream(streamID string, payload []byte) error
	// CloseOutput ends the host-owned output stream, optionally with an error.
	CloseOutput(streamID, errMessage string)
	// Log writes to the CLIProxyAPI log.
	Log(level, message string)
}

// Options tune timing for tests.
type Options struct {
	Now                 func() time.Time
	StaticModelsWait    time.Duration
	OnDemandMinInterval time.Duration
	VerifyDelay         time.Duration
}

// Service is the plugin runtime. All exported methods are safe for concurrent use.
type Service struct {
	host    Host
	version string
	opts    Options

	cfgMu     sync.Mutex
	cfg       config.Config
	issues    []config.Issue
	lastFatal []config.Issue

	disc       *catalog.Discoverer
	snap       atomic.Pointer[catalog.Snapshot]
	generation atomic.Uint64
	firstReady chan struct{}
	readyOnce  sync.Once

	loopMu     sync.Mutex
	loopCancel context.CancelFunc
	loopDone   chan struct{}
	trigger    chan struct{}

	lastOnDemand atomic.Int64
	staticWaited atomic.Bool
	lastIssues   string
	verifying    sync.Map
	lastLogged   atomic.Value
}

// New returns a configured-by-default service. Call Configure before use.
func New(host Host, version string, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.StaticModelsWait == 0 {
		opts.StaticModelsWait = 3 * time.Second
	}
	if opts.OnDemandMinInterval == 0 {
		opts.OnDemandMinInterval = 5 * time.Second
	}
	if opts.VerifyDelay == 0 {
		opts.VerifyDelay = 250 * time.Millisecond
	}
	s := &Service{
		host:       host,
		version:    version,
		opts:       opts,
		cfg:        config.Default(),
		disc:       catalog.NewDiscoverer(opts.Now),
		firstReady: make(chan struct{}),
		trigger:    make(chan struct{}, 1),
	}
	s.rebuild()
	return s
}

// Config returns the active configuration.
func (s *Service) Config() config.Config {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.cfg
}

// Snapshot returns the current immutable catalog snapshot.
func (s *Service) Snapshot() *catalog.Snapshot {
	return s.snap.Load()
}

func (s *Service) client(cfg config.Config) ollama.Client {
	return ollama.Client{BaseURL: cfg.Instance.BaseURL, APIKey: cfg.Instance.APIKey, Transport: s.host}
}

func (s *Service) logf(level, format string, args ...any) {
	if s.host == nil {
		return
	}
	s.host.Log(level, "cliproxyapi-ollama: "+fmt.Sprintf(format, args...))
}

// Configure applies plugin YAML. A configuration with fatal issues is rejected
// and the previous valid configuration stays active; the plugin never fails
// registration because of bad config, so a typo cannot unload the provider.
func (s *Service) Configure(configYAML []byte) []config.Issue {
	res := config.Parse(configYAML)

	s.cfgMu.Lock()
	issueText := fmt.Sprint(res.Issues)
	logIssues := issueText != s.lastIssues
	s.lastIssues = issueText
	s.cfgMu.Unlock()
	if logIssues {
		for _, issue := range res.Issues {
			level := "warn"
			if issue.Fatal {
				level = "error"
			}
			s.logf(level, "config %s", issue.String())
		}
	}

	s.cfgMu.Lock()
	prev := s.cfg
	if res.HasFatal() {
		s.lastFatal = res.Issues
		s.issues = res.Issues
		s.cfgMu.Unlock()
		if logIssues {
			s.logf("error", "configuration rejected; keeping the previous valid configuration")
		}
		s.rebuild()
		s.ensureLoop()
		return res.Issues
	}
	s.cfg = res.Config
	s.issues = res.Issues
	s.lastFatal = nil
	s.cfgMu.Unlock()

	if prev.Instance.BaseURL != res.Config.Instance.BaseURL || prev.Instance.APIKey != res.Config.Instance.APIKey {
		s.disc.Reset()
	}
	s.rebuild()
	if !reflect.DeepEqual(prev.Instance, res.Config.Instance) || prev.RefreshInterval != res.Config.RefreshInterval {
		s.TriggerRefresh()
	}
	s.ensureLoop()
	return res.Issues
}

// Issues returns the validation issues of the last Configure call.
func (s *Service) Issues() []config.Issue {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return append([]config.Issue(nil), s.issues...)
}

func (s *Service) rebuild() {
	cfg := s.Config()
	state := s.disc.State()
	snap := catalog.Build(state, cfg, s.generation.Add(1), s.opts.Now())
	snap.ConfigIssue = s.Issues()
	s.snap.Store(snap)
	if snap.Ready() {
		s.readyOnce.Do(func() { close(s.firstReady) })
	}
}

// RefreshNow runs one discovery pass and publishes a new snapshot.
func (s *Service) RefreshNow(ctx context.Context) error {
	cfg := s.Config()
	err := s.disc.Refresh(ctx, s.client(cfg), cfg.Instance)
	s.rebuild()
	prev, _ := s.lastLogged.Load().(string)
	msg := "ok"
	if err != nil {
		msg = "error: " + err.Error()
	}
	if msg != prev {
		s.lastLogged.Store(msg)
		if err != nil {
			s.logf("warn", "discovery failed (keeping last snapshot): %v", err)
		} else {
			snap := s.Snapshot()
			s.logf("info", "discovered %d models (%d excluded) from %s", len(snap.Models), len(snap.Excluded), cfg.Instance.BaseURL)
		}
	}
	return err
}

// TriggerRefresh asks the background loop to refresh soon.
func (s *Service) TriggerRefresh() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func (s *Service) ensureLoop() {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if s.loopCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.loopCancel = cancel
	s.loopDone = done
	go s.loop(ctx, done)
}

func (s *Service) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			s.logf("error", "refresh loop panic: %v", r)
		}
	}()
	for {
		_ = s.RefreshNow(ctx)
		timer := time.NewTimer(s.Config().RefreshInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.trigger:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Stop halts background work; Configure restarts it.
func (s *Service) Stop(wait time.Duration) {
	s.loopMu.Lock()
	cancel, done := s.loopCancel, s.loopDone
	s.loopCancel, s.loopDone = nil, nil
	s.loopMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

// WaitReady blocks until the first successful discovery or timeout.
func (s *Service) WaitReady(timeout time.Duration) bool {
	if s.Snapshot().Ready() {
		return true
	}
	select {
	case <-s.firstReady:
		return true
	case <-time.After(timeout):
		return false
	}
}

// lookupWithRefresh resolves a public ID, refreshing on demand (rate limited)
// when the model is unknown so newly installed models work without waiting.
func (s *Service) lookupWithRefresh(ctx context.Context, id string) (*catalog.Snapshot, catalog.Model, bool) {
	snap := s.Snapshot()
	if m, ok := snap.Lookup(id); ok {
		return snap, m, true
	}
	now := s.opts.Now().UnixNano()
	last := s.lastOnDemand.Load()
	if now-last >= int64(s.opts.OnDemandMinInterval) && s.lastOnDemand.CompareAndSwap(last, now) {
		cfg := s.Config()
		rctx, cancel := context.WithTimeout(ctx, 2*cfg.Instance.RequestTimeout)
		_ = s.RefreshNow(rctx)
		cancel()
		snap = s.Snapshot()
		if m, ok := snap.Lookup(id); ok {
			return snap, m, true
		}
	}
	return snap, catalog.Model{}, false
}

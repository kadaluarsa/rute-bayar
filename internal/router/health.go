package router

import (
	"sort"
	"sync"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
)

// GatewayStatus is the health label returned to callers (TRD v0.3 §3.1).
type GatewayStatus string

const (
	StatusHealthy  GatewayStatus = "HEALTHY"
	StatusDegraded GatewayStatus = "DEGRADED"
	StatusDown     GatewayStatus = "DOWN"
)

// BreakerState follows the usual closed / open / half-open circuit model.
type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half_open"
)

// BreakerConfig matches TRD v0.3 §5: 3 consecutive failures (5xx, network
// error, or a call slower than the latency budget) open the circuit for
// 5 minutes; after that one probe call is allowed.
type BreakerConfig struct {
	FailureThreshold int
	OpenFor          time.Duration
	SlowCall         time.Duration
	// Window bounds the samples used for p95 latency and success rate.
	Window     time.Duration
	MaxSamples int
	// DegradedSuccessRate and DegradedP95 mark a closed gateway DEGRADED.
	DegradedSuccessRate float64
	DegradedP95         time.Duration
}

func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		FailureThreshold:    3,
		OpenFor:             5 * time.Minute,
		SlowCall:            3000 * time.Millisecond,
		Window:              5 * time.Minute,
		MaxSamples:          200,
		DegradedSuccessRate: 0.95,
		DegradedP95:         2000 * time.Millisecond,
	}
}

type sample struct {
	at      time.Time
	latency time.Duration
	ok      bool
}

type gatewayHealth struct {
	samples       []sample
	consecutive   int
	state         BreakerState
	openedAt      time.Time
	probeInFlight bool
}

// Health tracks per-gateway outcomes. It is safe for concurrent use.
//
// State is per process. With several replicas each one learns on its own,
// which is acceptable for a breaker: a failing gateway trips every replica
// within a few calls.
type Health struct {
	mu       sync.Mutex
	cfg      BreakerConfig
	gateways map[domain.ProviderCode]*gatewayHealth
	now      func() time.Time
}

func NewHealth(cfg BreakerConfig) *Health {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = DefaultBreakerConfig().FailureThreshold
	}
	if cfg.OpenFor <= 0 {
		cfg.OpenFor = DefaultBreakerConfig().OpenFor
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultBreakerConfig().Window
	}
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = DefaultBreakerConfig().MaxSamples
	}
	if cfg.DegradedSuccessRate <= 0 {
		cfg.DegradedSuccessRate = DefaultBreakerConfig().DegradedSuccessRate
	}
	if cfg.DegradedP95 <= 0 {
		cfg.DegradedP95 = DefaultBreakerConfig().DegradedP95
	}
	return &Health{cfg: cfg, gateways: map[domain.ProviderCode]*gatewayHealth{}, now: time.Now}
}

func (h *Health) gateway(code domain.ProviderCode) *gatewayHealth {
	g, ok := h.gateways[code]
	if !ok {
		g = &gatewayHealth{state: BreakerClosed}
		h.gateways[code] = g
	}
	return g
}

// refresh moves an open breaker to half-open once its cool-down elapsed.
func (h *Health) refresh(g *gatewayHealth, now time.Time) {
	if g.state == BreakerOpen && now.Sub(g.openedAt) >= h.cfg.OpenFor {
		g.state = BreakerHalfOpen
		g.probeInFlight = false
	}
}

// Allow reports whether a call to the gateway may be attempted now. In the
// half-open state only one probe is admitted at a time.
func (h *Health) Allow(code domain.ProviderCode) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.gateway(code)
	h.refresh(g, h.now())
	switch g.state {
	case BreakerOpen:
		return false
	case BreakerHalfOpen:
		if g.probeInFlight {
			return false
		}
		g.probeInFlight = true
		return true
	default:
		return true
	}
}

// Available is Allow without reserving the half-open probe; the quote path
// uses it so quoting never consumes the probe slot.
func (h *Health) Available(code domain.ProviderCode) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.gateway(code)
	h.refresh(g, h.now())
	return g.state != BreakerOpen && !(g.state == BreakerHalfOpen && g.probeInFlight)
}

// Record stores one call outcome. A success slower than SlowCall counts as
// a failure for the breaker, but not for the success rate.
func (h *Health) Record(code domain.ProviderCode, latency time.Duration, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	g := h.gateway(code)
	h.refresh(g, now)

	g.samples = append(g.samples, sample{at: now, latency: latency, ok: ok})
	h.trim(g, now)

	breakerFailure := !ok || (h.cfg.SlowCall > 0 && latency > h.cfg.SlowCall)
	switch g.state {
	case BreakerHalfOpen:
		g.probeInFlight = false
		if breakerFailure {
			g.state = BreakerOpen
			g.openedAt = now
			g.consecutive = h.cfg.FailureThreshold
			return
		}
		g.state = BreakerClosed
		g.consecutive = 0
	default:
		if breakerFailure {
			g.consecutive++
			if g.consecutive >= h.cfg.FailureThreshold {
				g.state = BreakerOpen
				g.openedAt = now
			}
			return
		}
		g.consecutive = 0
	}
}

func (h *Health) trim(g *gatewayHealth, now time.Time) {
	cutoff := now.Add(-h.cfg.Window)
	start := 0
	for start < len(g.samples) && g.samples[start].at.Before(cutoff) {
		start++
	}
	if over := len(g.samples) - start - h.cfg.MaxSamples; over > 0 {
		start += over
	}
	if start > 0 {
		g.samples = append([]sample(nil), g.samples[start:]...)
	}
}

// Snapshot is the observable health of one gateway (PRD RP-3).
type Snapshot struct {
	Provider       domain.ProviderCode `json:"provider"`
	Status         GatewayStatus       `json:"status"`
	Breaker        BreakerState        `json:"breaker"`
	Samples        int                 `json:"samples"`
	SuccessRate    float64             `json:"success_rate"`
	P95LatencyMs   int64               `json:"p95_latency_ms"`
	ConsecutiveErr int                 `json:"consecutive_failures"`
	OpenUntil      *time.Time          `json:"open_until,omitempty"`
}

func (h *Health) Snapshot(code domain.ProviderCode) Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	g := h.gateway(code)
	h.refresh(g, now)
	h.trim(g, now)

	snap := Snapshot{Provider: code, Breaker: g.state, Samples: len(g.samples), SuccessRate: 1, ConsecutiveErr: g.consecutive}
	if len(g.samples) > 0 {
		okCount := 0
		latencies := make([]time.Duration, 0, len(g.samples))
		for _, s := range g.samples {
			if s.ok {
				okCount++
			}
			latencies = append(latencies, s.latency)
		}
		snap.SuccessRate = float64(okCount) / float64(len(g.samples))
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		idx := (len(latencies)*95+99)/100 - 1
		if idx < 0 {
			idx = 0
		}
		snap.P95LatencyMs = latencies[idx].Milliseconds()
	}

	switch {
	case g.state == BreakerOpen:
		snap.Status = StatusDown
		until := g.openedAt.Add(h.cfg.OpenFor)
		snap.OpenUntil = &until
	case g.state == BreakerHalfOpen,
		snap.SuccessRate < h.cfg.DegradedSuccessRate,
		time.Duration(snap.P95LatencyMs)*time.Millisecond > h.cfg.DegradedP95:
		snap.Status = StatusDegraded
	default:
		snap.Status = StatusHealthy
	}
	return snap
}

// SnapshotAll returns snapshots for the given providers in order.
func (h *Health) SnapshotAll(codes []domain.ProviderCode) []Snapshot {
	out := make([]Snapshot, 0, len(codes))
	for _, code := range codes {
		out = append(out, h.Snapshot(code))
	}
	return out
}

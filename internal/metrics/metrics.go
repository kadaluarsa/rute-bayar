// Package metrics is a small, dependency-free Prometheus text-format
// registry. It covers what Rute Bayar needs (counters, histograms and
// scrape-time gauges) without pulling client_golang into the module.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds metric families in registration order.
type Registry struct {
	mu       sync.Mutex
	families []family
	byName   map[string]family
}

type family interface {
	name() string
	write(w io.Writer)
}

func NewRegistry() *Registry { return &Registry{byName: map[string]family{}} }

func (r *Registry) register(f family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[f.name()]; exists {
		panic("metrics: duplicate metric " + f.name())
	}
	r.byName[f.name()] = f
	r.families = append(r.families, f)
}

// WriteText renders every family in Prometheus text exposition format.
func (r *Registry) WriteText(w io.Writer) {
	if r == nil {
		return
	}
	r.mu.Lock()
	families := append([]family(nil), r.families...)
	r.mu.Unlock()
	for _, f := range families {
		f.write(w)
	}
}

func labelKey(values []string) string { return strings.Join(values, "\x00") }

func renderLabels(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		parts = append(parts, n+`="`+escape(v)+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, extra[i]+`="`+escape(extra[i+1])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escape(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

func formatFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// CounterVec is a monotonically increasing counter with labels.
type CounterVec struct {
	metricName, help string
	labels           []string
	mu               sync.Mutex
	values           map[string]float64
	labelValues      map[string][]string
}

func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{metricName: name, help: help, labels: labels, values: map[string]float64{}, labelValues: map[string][]string{}}
	r.register(c)
	return c
}

func (c *CounterVec) name() string { return c.metricName }

// Inc adds one. A nil CounterVec is a no-op so callers need no guards.
func (c *CounterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

func (c *CounterVec) Add(delta float64, labelValues ...string) {
	if c == nil || delta < 0 {
		return
	}
	key := labelKey(labelValues)
	c.mu.Lock()
	c.values[key] += delta
	if _, ok := c.labelValues[key]; !ok {
		c.labelValues[key] = append([]string(nil), labelValues...)
	}
	c.mu.Unlock()
}

func (c *CounterVec) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.metricName, c.help, c.metricName)
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %s\n", c.metricName, renderLabels(c.labels, c.labelValues[k]), formatFloat(c.values[k]))
	}
}

// HistogramVec records observations into cumulative buckets.
type HistogramVec struct {
	metricName, help string
	labels           []string
	buckets          []float64
	mu               sync.Mutex
	series           map[string]*histSeries
}

type histSeries struct {
	labelValues []string
	counts      []uint64
	sum         float64
	count       uint64
}

// DefaultLatencyBuckets suit gateway calls (seconds).
var DefaultLatencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 20}

func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	if len(buckets) == 0 {
		buckets = DefaultLatencyBuckets
	}
	h := &HistogramVec{metricName: name, help: help, labels: labels, buckets: append([]float64(nil), buckets...), series: map[string]*histSeries{}}
	sort.Float64s(h.buckets)
	r.register(h)
	return h
}

func (h *HistogramVec) name() string { return h.metricName }

func (h *HistogramVec) Observe(value float64, labelValues ...string) {
	if h == nil {
		return
	}
	key := labelKey(labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		s = &histSeries{labelValues: append([]string(nil), labelValues...), counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	for i, b := range h.buckets {
		if value <= b {
			s.counts[i]++
		}
	}
	s.sum += value
	s.count++
}

func (h *HistogramVec) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.metricName, h.help, h.metricName)
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		for i, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.metricName, renderLabels(h.labels, s.labelValues, "le", formatFloat(b)), s.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.metricName, renderLabels(h.labels, s.labelValues, "le", "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.metricName, renderLabels(h.labels, s.labelValues), formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.metricName, renderLabels(h.labels, s.labelValues), s.count)
	}
}

// Sample is one labeled gauge value produced at scrape time.
type Sample struct {
	LabelValues []string
	Value       float64
}

type gaugeFunc struct {
	metricName, help string
	labels           []string
	collect          func() []Sample
}

// NewGaugeFunc registers a gauge computed on every scrape.
func (r *Registry) NewGaugeFunc(name, help string, collect func() []Sample, labels ...string) {
	r.register(&gaugeFunc{metricName: name, help: help, labels: labels, collect: collect})
}

func (g *gaugeFunc) name() string { return g.metricName }

func (g *gaugeFunc) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.metricName, g.help, g.metricName)
	for _, s := range g.collect() {
		fmt.Fprintf(w, "%s%s %s\n", g.metricName, renderLabels(g.labels, s.LabelValues), formatFloat(s.Value))
	}
}

// Set is the Rute Bayar metric set shared by services.
type Set struct {
	Registry          *Registry
	Quotes            *CounterVec
	TokensIssued      *CounterVec
	GatewayCalls      *CounterVec
	GatewayLatency    *HistogramVec
	Fallbacks         *CounterVec
	Executions        *CounterVec
	MandateRejections *CounterVec
	Webhooks          *CounterVec
	OutboxDeliveries  *CounterVec
	MCPCalls          *CounterVec
}

func NewSet() *Set {
	r := NewRegistry()
	return &Set{
		Registry:          r,
		Quotes:            r.NewCounterVec("rutebayar_route_quotes_total", "Route quotes by method family and result.", "family", "result"),
		TokensIssued:      r.NewCounterVec("rutebayar_routing_tokens_issued_total", "Routing tokens issued by method family.", "family"),
		GatewayCalls:      r.NewCounterVec("rutebayar_gateway_calls_total", "Gateway create-payment calls by provider and outcome.", "provider", "outcome"),
		GatewayLatency:    r.NewHistogramVec("rutebayar_gateway_call_duration_seconds", "Gateway create-payment latency.", DefaultLatencyBuckets, "provider"),
		Fallbacks:         r.NewCounterVec("rutebayar_gateway_fallbacks_total", "Fallbacks from one gateway to the next.", "from", "to"),
		Executions:        r.NewCounterVec("rutebayar_route_executions_total", "Routing token executions by final status.", "status"),
		MandateRejections: r.NewCounterVec("rutebayar_mandate_rejections_total", "Charges refused by the mandateCovers check.", "reason"),
		Webhooks:          r.NewCounterVec("rutebayar_webhooks_total", "Inbound gateway webhooks by provider and processing status.", "provider", "processing_status"),
		OutboxDeliveries:  r.NewCounterVec("rutebayar_outbox_deliveries_total", "Outbound Wishpull event deliveries by result.", "result"),
		MCPCalls:          r.NewCounterVec("rutebayar_mcp_tool_calls_total", "MCP tool calls by tool and result.", "tool", "result"),
	}
}

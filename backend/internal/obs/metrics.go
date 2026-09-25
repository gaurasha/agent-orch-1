package obs

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metrics holds the counters and histograms this system exposes.
//
// The set is deliberately small and chosen to answer the two questions an
// operator actually has: "what is agent X doing right now" and "what has it
// cost". Everything here rolls up to one of those.
type Metrics struct {
	mu sync.Mutex

	toolCalls    map[labelKey]uint64
	toolLatency  map[labelKey]*histogram
	modelCalls   map[labelKey]uint64
	modelTokens  map[labelKey]uint64
	modelCostUSD map[string]float64
	runStates    map[labelKey]int64
	sandboxRuns  map[labelKey]uint64
	quotaDenied  map[string]uint64
	auditFails   uint64
	leaseReaps   uint64
	stepLatency  *histogram
}

type labelKey struct{ a, b, c string }

func NewMetrics() *Metrics {
	return &Metrics{
		toolCalls: map[labelKey]uint64{}, toolLatency: map[labelKey]*histogram{},
		modelCalls: map[labelKey]uint64{}, modelTokens: map[labelKey]uint64{},
		modelCostUSD: map[string]float64{}, runStates: map[labelKey]int64{},
		sandboxRuns: map[labelKey]uint64{}, quotaDenied: map[string]uint64{},
		stepLatency: newHistogram(),
	}
}

func (m *Metrics) ToolCall(tenant, tool, decision string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := labelKey{tenant, tool, decision}
	m.toolCalls[k]++
	h, ok := m.toolLatency[k]
	if !ok {
		h = newHistogram()
		m.toolLatency[k] = h
	}
	h.observe(d.Seconds())
}

func (m *Metrics) ModelCall(tenant, model string, inTok, outTok int64, costUSD float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modelCalls[labelKey{tenant, model, ""}]++
	m.modelTokens[labelKey{tenant, model, "input"}] += uint64(inTok)
	m.modelTokens[labelKey{tenant, model, "output"}] += uint64(outTok)
	m.modelCostUSD[tenant] += costUSD
}

func (m *Metrics) RunState(tenant string, state string, delta int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runStates[labelKey{tenant, state, ""}] += delta
}

func (m *Metrics) SandboxRun(tenant, driver string) {
	m.inc(m.sandboxRuns, labelKey{tenant, driver, ""})
}
func (m *Metrics) QuotaDenied(tenant string) { m.mu.Lock(); m.quotaDenied[tenant]++; m.mu.Unlock() }
func (m *Metrics) AuditFailure()             { m.mu.Lock(); m.auditFails++; m.mu.Unlock() }
func (m *Metrics) LeaseReaped(n int)         { m.mu.Lock(); m.leaseReaps += uint64(n); m.mu.Unlock() }
func (m *Metrics) StepLatency(d time.Duration) {
	m.mu.Lock()
	m.stepLatency.observe(d.Seconds())
	m.mu.Unlock()
}

func (m *Metrics) inc(mp map[labelKey]uint64, k labelKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mp[k]++
}

// Handler exposes the Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(m.Render()))
	})
}

func (m *Metrics) Render() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder

	writeHeader(&b, "agentorch_tool_calls_total", "counter", "Tool calls by tenant, tool and authorization decision.")
	forEachKey(m.toolCalls, func(k labelKey, v uint64) {
		fmt.Fprintf(&b, "agentorch_tool_calls_total{tenant=%q,tool=%q,decision=%q} %d\n", k.a, k.b, k.c, v)
	})

	writeHeader(&b, "agentorch_tool_call_duration_seconds", "histogram", "Tool call latency.")
	keys := make([]labelKey, 0, len(m.toolLatency))
	for k := range m.toolLatency {
		keys = append(keys, k)
	}
	sortKeys(keys)
	for _, k := range keys {
		m.toolLatency[k].render(&b, "agentorch_tool_call_duration_seconds",
			fmt.Sprintf("tenant=%q,tool=%q,decision=%q", k.a, k.b, k.c))
	}

	writeHeader(&b, "agentorch_model_calls_total", "counter", "LLM calls by tenant and model.")
	forEachKey(m.modelCalls, func(k labelKey, v uint64) {
		fmt.Fprintf(&b, "agentorch_model_calls_total{tenant=%q,model=%q} %d\n", k.a, k.b, v)
	})

	writeHeader(&b, "agentorch_model_tokens_total", "counter", "LLM tokens by tenant, model and direction.")
	forEachKey(m.modelTokens, func(k labelKey, v uint64) {
		fmt.Fprintf(&b, "agentorch_model_tokens_total{tenant=%q,model=%q,direction=%q} %d\n", k.a, k.b, k.c, v)
	})

	writeHeader(&b, "agentorch_model_cost_usd_total", "counter", "Cumulative LLM spend by tenant.")
	tenants := make([]string, 0, len(m.modelCostUSD))
	for t := range m.modelCostUSD {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	for _, t := range tenants {
		fmt.Fprintf(&b, "agentorch_model_cost_usd_total{tenant=%q} %.6f\n", t, m.modelCostUSD[t])
	}

	writeHeader(&b, "agentorch_runs", "gauge", "Runs by tenant and state.")
	forEachKey2(m.runStates, func(k labelKey, v int64) {
		fmt.Fprintf(&b, "agentorch_runs{tenant=%q,state=%q} %d\n", k.a, k.b, v)
	})

	writeHeader(&b, "agentorch_sandbox_runs_total", "counter", "Sandbox executions by tenant and isolation driver.")
	forEachKey(m.sandboxRuns, func(k labelKey, v uint64) {
		fmt.Fprintf(&b, "agentorch_sandbox_runs_total{tenant=%q,driver=%q} %d\n", k.a, k.b, v)
	})

	writeHeader(&b, "agentorch_quota_denied_total", "counter", "Scheduling decisions deferred because a tenant was out of quota.")
	qt := make([]string, 0, len(m.quotaDenied))
	for t := range m.quotaDenied {
		qt = append(qt, t)
	}
	sort.Strings(qt)
	for _, t := range qt {
		fmt.Fprintf(&b, "agentorch_quota_denied_total{tenant=%q} %d\n", t, m.quotaDenied[t])
	}

	writeHeader(&b, "agentorch_audit_write_failures_total", "counter",
		"Audit records that could not be persisted. Should always be zero; alert on any increase.")
	fmt.Fprintf(&b, "agentorch_audit_write_failures_total %d\n", m.auditFails)

	writeHeader(&b, "agentorch_leases_reaped_total", "counter",
		"Runs reclaimed after a worker stopped renewing its lease.")
	fmt.Fprintf(&b, "agentorch_leases_reaped_total %d\n", m.leaseReaps)

	writeHeader(&b, "agentorch_step_duration_seconds", "histogram", "Agent step latency.")
	m.stepLatency.render(&b, "agentorch_step_duration_seconds", "")
	return b.String()
}

func writeHeader(b *strings.Builder, name, typ, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func forEachKey(m map[labelKey]uint64, fn func(labelKey, uint64)) {
	keys := make([]labelKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortKeys(keys)
	for _, k := range keys {
		fn(k, m[k])
	}
}

func forEachKey2(m map[labelKey]int64, fn func(labelKey, int64)) {
	keys := make([]labelKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortKeys(keys)
	for _, k := range keys {
		fn(k, m[k])
	}
}

func sortKeys(keys []labelKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].a != keys[j].a {
			return keys[i].a < keys[j].a
		}
		if keys[i].b != keys[j].b {
			return keys[i].b < keys[j].b
		}
		return keys[i].c < keys[j].c
	})
}

// histogram is a fixed-bucket cumulative histogram.
//
// Buckets span 1 ms to 2 minutes because that is the real range here: an fs
// tool returns in microseconds, a sandbox exec in milliseconds to seconds, and
// an LLM call in seconds to a minute. Prometheus' defaults top out at 10s and
// would put every model call in +Inf.
type histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	total  uint64
}

func newHistogram() *histogram {
	b := []float64{.001, .005, .01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}
	return &histogram{bounds: b, counts: make([]uint64, len(b))}
}

func (h *histogram) observe(v float64) {
	h.sum += v
	h.total++
	for i, ub := range h.bounds {
		if v <= ub {
			h.counts[i]++
		}
	}
}

func (h *histogram) render(b *strings.Builder, name, labels string) {
	sep := ""
	if labels != "" {
		sep = ","
	}
	var cum uint64
	for i, ub := range h.bounds {
		cum = h.counts[i]
		fmt.Fprintf(b, "%s_bucket{%s%sle=\"%g\"} %d\n", name, labels, sep, ub, cum)
	}
	fmt.Fprintf(b, "%s_bucket{%s%sle=\"+Inf\"} %d\n", name, labels, sep, h.total)
	if labels == "" {
		fmt.Fprintf(b, "%s_sum %f\n%s_count %d\n", name, h.sum, name, h.total)
		return
	}
	fmt.Fprintf(b, "%s_sum{%s} %f\n%s_count{%s} %d\n", name, labels, h.sum, name, labels, h.total)
}

# Observability

> **Prerequisite:** [Components](../02-architecture/01-components.md)
> **Read next:** [Runbook](04-runbook.md)
> **Source:** [`internal/obs/`](../../backend/internal/obs/)

The requirement is two questions:

> *"What is agent X doing right now, and what has it cost so far?"*

They have different shapes and are answered by different mechanisms.

---

## 1. "What is it doing right now" — the event log

Answered from the **event log itself**, not from a parallel telemetry pipeline.

```
GET /v1/runs/{id}/events     → the full history
GET /v1/runs/{id}/stream     → SSE, live
```

This matters more than it sounds. The console timeline is rebuilt from the same
events that rebuild the model's context, so **the operator sees what the agent
saw**. A separate telemetry path can drift from reality — a dropped span, a
sampled trace, a log line that was rate-limited — and then the operator is
debugging a different system from the one that ran.

The projections differ (operators see `NOTE`, quota waits and worker IDs; the
model does not), but they come from one source, so they cannot disagree about
what happened.

---

## 2. "What has it cost" — per-run accounting

```go
type Usage struct {
    Steps        int
    ToolCalls    int
    InputTokens  int64
    OutputTokens int64
    CostUSD      float64
    SandboxMS    int64
}
```

Updated on every commit, visible on the run, aggregated by tenant at
`/v1/overview`.

### Cost comes from a price table in code

```go
var priceTable = map[string]Pricing{
    "claude-opus-5":    {15.00, 75.00},   // $/Mtok in, $/Mtok out
    "claude-sonnet-5":  { 3.00, 15.00},
    "claude-haiku-4-5": { 0.80,  4.00},
    …
}
```

An **unknown model is priced at the most expensive known rate**, deliberately:

> Under-reporting spend is the failure mode that actually hurts, because nobody
> investigates a cheap-looking agent.

**Gap:** not reconciled against provider-reported usage. Production should
compare against the provider's billing API and alert on drift.

### Budget meters, not just totals

The console shows usage **against the limit**, with colour crossing to warn at
70% and error at 90%. An operator should see a run approaching its budget, not
learn about it from the failure.

---

## 3. Metrics

Eleven series. Small on purpose — each rolls up to one of the two questions.

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `agentorch_tool_calls_total` | counter | `tenant`, `tool`, `decision` | What are agents doing? **What is being refused?** |
| `agentorch_tool_call_duration_seconds` | histogram | `tenant`, `tool`, `decision` | Which tools are slow? |
| `agentorch_model_calls_total` | counter | `tenant`, `model` | Provider request rate vs quota |
| `agentorch_model_tokens_total` | counter | `tenant`, `model`, `direction` | Token consumption |
| `agentorch_model_cost_usd_total` | counter | `tenant` | **Spend, by tenant** |
| `agentorch_runs` | gauge | `tenant`, `state` | Fleet state; queue depth |
| `agentorch_sandbox_runs_total` | counter | `tenant`, `driver` | Sandbox volume, **and which boundary was used** |
| `agentorch_quota_denied_total` | counter | `tenant` | Who is being throttled |
| `agentorch_audit_write_failures_total` | counter | — | **Should always be 0** |
| `agentorch_leases_reaped_total` | counter | — | Worker deaths |
| `agentorch_step_duration_seconds` | histogram | — | Orchestration latency |

### Two that deserve attention

**`agentorch_tool_calls_total{decision="DENY"}`** is the security signal. A rise
for one run means an agent is trying things it is not allowed to do — the
highest-value alert in the system, because it is what a contained prompt
injection looks like from the outside.

**`agentorch_audit_write_failures_total`** should never be non-zero. Any increase
means **we executed a side effect we could not record**. Page immediately.

### Histogram buckets are chosen, not defaulted

```go
b := []float64{.001, .005, .01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}
```

Prometheus' defaults top out at 10 s, which would put **every model call** in
`+Inf`. The real range here spans an `fs` tool at microseconds to a model call at
a minute.

### Hand-rolled exposition

~150 lines of text formatting instead of `client_golang` and its dependency
tree — a deliberate trade for a system whose purpose is containment, argued in
[DEEP_DIVE D16](../../DEEP_DIVE.md#d16--dependency-policy). It flips the moment
native histograms or exemplars are needed.

---

## 4. Logs

Structured JSON via `log/slog`:

```json
{"time":"2026-09-22T11:00:49Z","level":"WARN","service":"all",
 "component":"tool-gateway","run_id":"run_01a0…","tenant_id":"acme",
 "msg":"tool call rejected at authentication","err":"jwt: token expired"}
```

JSON rather than text for a specific reason: **agent tool arguments routinely
contain newlines**, which would corrupt a line-oriented format.

| Field | Use |
|---|---|
| `service` | which binary |
| `component` | which subsystem |
| `run_id` | **follow one run across every component** |
| `tenant_id` | per-tenant investigation |
| `trace_id` | present in plumbing; spans not emitted (gap) |

```bash
kubectl logs -l app.kubernetes.io/part-of=agentorch --tail=-1 | grep '"run_id":"run_01a0…"'
```

> `service` and `component` are separate keys because using one key at both
> levels produced duplicate JSON keys — a small bug, fixed.

---

## 5. The audit log is not observability

Related but different, and conflating them is a mistake:

| | Metrics/logs | Audit log |
|---|---|---|
| Purpose | operate the system | **prove what happened** |
| Retention | days–weeks | months–years |
| Tamper-evident | no | **yes** (hash chain) |
| Sampling | acceptable | **never** |
| Completeness | best-effort | **every decision, including denials** |

See [Audit](../01-concepts/07-audit.md).

---

## 6. Suggested alerts

### Page

| Alert | Expression | Why |
|---|---|---|
| Audit writes failing | `increase(agentorch_audit_write_failures_total[5m]) > 0` | Executing without recording |
| Audit chain broken | `chain_verified == false` from `/v1/audit` | Tampering, or a bug in our own hashing |
| No runs progressing | `sum(rate(agentorch_step_duration_seconds_count[5m])) == 0` and queue depth > 0 | Workers wedged or DB down |
| Queue depth climbing | `sum(agentorch_runs{state="QUEUED"}) > 500` for 10m | Under-scaled or quota-starved |

### Ticket

| Alert | Expression | Why |
|---|---|---|
| Denial spike for one run | `increase(agentorch_tool_calls_total{decision="DENY"}[5m]) > 10` | Probable prompt injection |
| Tenant throttled persistently | `increase(agentorch_quota_denied_total[15m]) > 100` | Under-provisioned or abusive |
| Lease reaping elevated | `increase(agentorch_leases_reaped_total[10m]) > 20` | Workers dying |
| Spend anomaly | `increase(agentorch_model_cost_usd_total[1h]) > <budget>` | Runaway cost |
| Sandbox driver unexpected | `agentorch_sandbox_runs_total{driver!="kubernetes"} > 0` in prod | **Running with a weaker boundary than intended** |

That last one is worth having: it catches a misconfiguration that silently
downgrades isolation while everything else looks healthy.

---

## 7. Dashboard

Four panels answer almost every question:

```
┌──────────────────────────┬──────────────────────────┐
│ FLEET STATE              │ SPEND (24h, by tenant)   │
│ runs by state, stacked   │ increase(cost_usd[24h])  │
│ ⚠ watch QUEUED growth    │ ⚠ watch for a step change│
├──────────────────────────┼──────────────────────────┤
│ TOOL CALLS BY DECISION   │ QUOTA PRESSURE           │
│ ALLOW vs DENY, by tenant │ available_now / capacity │
│ ⚠ DENY spike = injection │ ⚠ near 0 = throttled     │
└──────────────────────────┴──────────────────────────┘
```

---

## 8. Gaps

| Gap | Impact |
|---|---|
| **No distributed tracing** | `trace_id` plumbing exists in `internal/obs`; spans are not emitted. Latency attribution across worker → gateway → sandbox is manual |
| **No provider usage reconciliation** | Cost is computed, not verified |
| **SSE is poll-and-push** | 400 ms tick, one goroutine per open tab. Needs `LISTEN/NOTIFY` at scale |
| **No SIEM shipping** | Audit lives only in Postgres |
| **No per-tool SLOs** | No error-budget tracking |
| **No content provenance** | Cannot say "this call followed ingesting untrusted content" — the highest-signal injection heuristic |

---

**Next:** [Runbook](04-runbook.md).

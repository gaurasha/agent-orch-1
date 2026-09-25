from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE, AMBER = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf", "#b8860b"

def build():
    d = Diagram(1560, 1150, "10 · Observability and audit — 'what is agent X doing, and what has it cost?'",
                "Four signals, one source of truth. The console and the model read the same event log, so the operator sees exactly what the agent saw.")

    d.zone(40, 86, 480, 620, "SOURCES · written on the hot path", "state", "every write is part of the transaction it describes", subtitle_inside=True)
    d.zone(560, 86, 440, 620, "SURFACES · how it leaves the process", "platform", "no agent, no sidecar, no sampling", subtitle_inside=True)
    d.zone(1040, 86, 480, 620, "QUESTIONS · what each surface answers", "neutral", "and the alert that should page", subtitle_inside=True)

    S = 70; W = 420
    d.box("events", S, 132, W, 118, "1 · event log (events table)", kind="state",
          lines=["RUN_CREATED · MODEL_REQUEST/RESPONSE · TOOL_CALL · TOOL_RESULT", "TOOL_DENIED · APPROVAL_NEEDED/GIVEN · HUMAN_PAUSE/RESUME",
                 "LEASE_LOST · RUN_FINISHED · NOTE (worker id, yields, quota waits)", "the model's context is Rebuild(events) — same rows",
                 "payload carries tokens, cost, duration_ms, worker, idem_key"])
    d.box("usage", S, 270, W, 92, "2 · per-run usage (runs.usage JSONB)", kind="state", mono=True,
          lines=["steps · tool_calls · input_tokens · output_tokens", "cost_usd (price table in code) · sandbox_ms",
                 "updated in the SAME commit as the events it reflects"])
    d.box("audit", S, 382, W, 118, "3 · audit_log — per-tenant hash chain", kind="state",
          lines=["two records per allowed tool call (pre/post), one per denial", "who (triggering_user) · which agent · which tool · decision · rule",
                 "args redacted · result meta (bytes, truncated, exit code, credential_id)", "prev_hash → hash chain; ts at µs; VerifyChain() detects edits"])
    d.box("metrics", S, 520, W, 70, "4 · metrics registry (in-process, hand-rolled)", kind="platform",
          lines=["counters/gauges/histograms keyed by tenant · tool · decision · model · state", "no client library: 11 series rendered as Prometheus text on demand"])
    d.box("logs", S, 610, W, 70, "5 · structured logs (log/slog JSON)", kind="platform",
          lines=["every line: service · run_id · tenant_id (WithRun) · trace id when present", "levels via AGENTORCH_LOG_LEVEL; no secrets can appear (type Secret)"])

    X = 590; XW = 380
    d.box("sse", X, 132, XW, 92, "GET /v1/runs/{id}/stream — SSE", kind="platform",
          lines=["ListEvents(since) every 400 ms; resume from last seq", "the console timeline IS the event log",
                 "operators additionally see NOTE, worker ids, yields"])
    d.box("api", X, 244, XW, 92, "GET /v1/runs · /v1/runs/{id} · /v1/overview", kind="platform",
          lines=["state, status_reason, usage, budget, lease owner", "overview: runs by state, cost by tenant, quota snapshot",
                 "tenant-scoped; operators may read across tenants"])
    d.box("auditapi", X, 356, XW, 92, "GET /v1/audit (+ verify)", kind="platform",
          lines=["filter by tenant · run · time range (idx_audit_tenant_ts)", "VerifyChain recomputes and reports the first broken seq",
                 "'every command agent X ran last Tuesday' is one index scan"])
    d.box("prom", X, 468, XW, 92, "GET /metrics — Prometheus exposition", kind="platform", mono=True,
          lines=["agentorch_tool_calls_total{tenant,tool,decision}", "agentorch_model_tokens_total{tenant,model,direction}",
                 "agentorch_runs{tenant,state} · _leases_reaped_total …"])
    d.box("stdout", X, 580, XW, 92, "stdout → your log pipeline", kind="platform",
          lines=["JSON lines; ship with the cluster's collector", "join on run_id across controlplane, agentd, gateway",
                 "(designed) OpenTelemetry spans keyed by run_id"])

    Q = 1070; QW = 420
    d.box("q1", Q, 132, QW, 92, "What is agent X doing right now?", kind="neutral",
          lines=["→ SSE / events: the last event and its worker", "state RUNNING + lease_owner tells you WHICH pod",
                 "WAITING_* tells you WHO it is waiting for"])
    d.box("q2", Q, 244, QW, 92, "What has it cost so far?", kind="neutral",
          lines=["→ runs.usage: tokens, USD, steps, tool calls, sandbox ms", "→ /v1/overview: by tenant; → metrics: by model",
                 "cost is committed with the step — never estimated after the fact"])
    d.box("q3", Q, 356, QW, 92, "Who did what, and can I prove it?", kind="neutral",
          lines=["→ audit chain: user · agent · tool · decision · rule · args (redacted)", "→ VerifyChain: tamper-evidence per tenant",
                 "denials are recorded as thoroughly as successes"])
    d.box("q4", Q, 468, QW, 92, "Is the platform healthy? (page on these)", kind="external",
          lines=["audit_write_failures_total > 0 — the chain is falling behind reality", "rate(leases_reaped_total) — workers are dying or stalling",
                 "quota_denied_total rising + runs{state=QUEUED} growing — starvation"])
    d.box("q5", Q, 580, QW, 92, "Is a tenant abusing the platform?", kind="external",
          lines=["tool_calls_total{decision=DENY} by tenant — injection attempts surface here", "model_cost_usd_total by tenant vs plan",
                 "sandbox_runs_total by driver — is anything running outside gVisor?"])

    d.arrow("events", "sse", "", src_side="e", dst_side="w", color=GREEN)
    d.arrow("events", "api", "", src_side="e", dst_side="w", src_off=30, dst_off=-20, via=[(555, 221), (555, 270)], color=GREEN)
    d.arrow("usage", "api", "", src_side="e", dst_side="w", dst_off=20, color=GREEN)
    d.arrow("audit", "auditapi", "", src_side="e", dst_side="w", color=GREEN)
    d.arrow("metrics", "prom", "", src_side="e", dst_side="w", dst_off=-20, via=[(555, 555), (555, 494)], color=BLUE)
    d.arrow("logs", "stdout", "", src_side="e", dst_side="w", dst_off=10, via=[(555, 645), (555, 636)], color=BLUE)
    d.arrow("sse", "q1", "", src_side="e", dst_side="w")
    d.arrow("api", "q1", "", src_side="e", dst_side="w", src_off=-20, dst_off=30, via=[(1035, 270), (1035, 208)])
    d.arrow("api", "q2", "", src_side="e", dst_side="w", src_off=20, dst_off=0)
    d.arrow("auditapi", "q3", "", src_side="e", dst_side="w")
    d.arrow("prom", "q4", "", src_side="e", dst_side="w")
    d.arrow("prom", "q5", "", src_side="e", dst_side="w", src_off=30, dst_off=-20, via=[(1035, 544), (1035, 606)])
    d.arrow("stdout", "q5", "", src_side="e", dst_side="w", dst_off=20, via=[(1035, 626), (1035, 646)])

    d.table(40, 730, [380, 480, 620], [
        [["Series (all prefixed agentorch_)"], ["Labels"], ["Why it exists"]],
        [["tool_calls_total"], ["tenant, tool, decision (ALLOW/DENY/NEEDS_APPROVAL/ERROR)"], ["the security signal: denials per tenant per tool"]],
        [["tool_call_duration_seconds (histogram)"], ["tool"], ["sandbox and third-party latency; p99 drives the wall-clock defaults"]],
        [["model_calls_total · model_tokens_total · model_cost_usd_total"], ["tenant, model, direction"], ["the cost line; reconciles against the provider invoice"]],
        [["runs (gauge)"], ["tenant, state"], ["queue depth and stuck states at a glance; the HPA input the design wants"]],
        [["sandbox_runs_total"], ["tenant, driver"], ["isolation posture: anything not 'kubernetes' in production is a finding"]],
        [["quota_denied_total"], ["tenant"], ["fairness at work: scheduling decisions deferred for lack of quota"]],
        [["audit_write_failures_total · leases_reaped_total"], ["—"], ["the two 'page now' counters: audit integrity and worker health"]],
        [["step_duration_seconds (histogram)"], ["—"], ["replay + model + tools per step; growth over a run's life shows context cost"]],
    ], kind="platform", size=10)

    d.note(40, 1020, 1480, kind="neutral", title="Trade-offs taken",
           lines=["hand-rolled metrics (no client_golang) keep the dependency policy (one external module) — cost: no exemplars, no native histograms, no OpenMetrics",
                  "SSE by polling Postgres (400 ms) is simple and correct — cost: read load scales with open consoles; LISTEN/NOTIFY or a replica is the designed fix",
                  "no distributed tracing yet — the run_id in every log line and every event is the correlation key; OTel spans would attach to it without schema changes"])

    d.legend = [("state", "durable source"), ("platform", "process / API"), ("neutral", "operator question"), ("external", "alerting")]
    return d

if __name__ == "__main__":
    build().save("../svg/10-observability.svg")

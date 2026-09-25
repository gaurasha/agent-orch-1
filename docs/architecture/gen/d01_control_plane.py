from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf"

def build():
    d = Diagram(1500, 1100, "01 · Control plane — the API server and console",
                "Where identity and intent enter the system. Holds no credentials, executes nothing, never talks to a model or a tool.")

    d.zone(40, 86, 1420, 118, "CALLERS", "untrusted", "everything here authenticates with a bearer API key; production swaps in OIDC")
    d.zone(40, 250, 940, 640, "CONTROL PLANE PROCESS · agentorch serve · :8080 · 2 replicas · PDB minAvailable=1", "platform",
           "stateless — any replica answers any request; SSE clients reconnect and resume from the last seq", subtitle_inside=True)
    d.zone(1020, 250, 440, 640, "STATE", "state", "all reads and writes are tenant-scoped", subtitle_inside=True)

    # callers
    d.box("ci", 70, 114, 300, 74, "CI / automation", kind="human",
          lines=["POST /v1/runs with agent_digest or agent_name", "polls GET /v1/runs/{id} or streams events"])
    d.box("human", 410, 114, 300, 74, "Human via the console", kind="human",
          lines=["React SPA, served from the same binary", "resume · approve · cancel · watch"])
    d.box("operator", 750, 114, 330, 74, "Platform operator", kind="human",
          lines=["principal with Operator=true", "may read ACROSS tenants (?tenant=) — never write on their behalf"])
    d.box("scraper", 1120, 114, 310, 74, "Prometheus / probes", kind="human",
          lines=["GET /metrics (text exposition, hand-rolled)", "GET /healthz for readiness + liveness"])

    # the request path (left column)
    d.box("auth", 70, 300, 400, 96, "1 · Authenticate + resolve principal", kind="platform",
          lines=["Authorization: Bearer <api-key>  →  Principal{TenantID, User, Operator}",
                 "unknown key → 401 · the key table is in memory in the PoC",
                 "every handler receives the Principal; there is no ambient tenant"])
    d.box("scope", 70, 426, 400, 96, "2 · Tenant scoping (the multi-tenancy boundary)", kind="platform",
          lines=["scope(p) = p.TenantID, or ?tenant= when p.Operator",
                 "every store call takes the tenant → rows of other tenants are unreachable",
                 "a foreign run id returns 404 NOT 403: existence is itself information"])
    d.box("router", 70, 552, 400, 150, "3 · Route (net/http ServeMux, method-aware patterns)", kind="platform", mono=True,
          lines=["POST /v1/runs                 create (admission-controlled)",
                 "GET  /v1/runs · /v1/runs/{id}  list · get   (tenant-scoped)",
                 "GET  /v1/runs/{id}/events      full history (seq-ordered)",
                 "GET  /v1/runs/{id}/stream      SSE, live",
                 "POST /v1/runs/{id}/{resume|approve|cancel}",
                 "GET  /v1/audit · /v1/quota · /v1/tools · /v1/overview",
                 "GET|POST /v1/agents            immutable definitions"])
    d.box("ui", 70, 732, 400, 58, "Static console", kind="neutral",
          lines=["embedded Vite build served at / — one binary, one image, no CDN"])

    # handlers (middle column)
    d.box("create", 500, 300, 450, 160, "createRun — admission control, then ONE transaction", kind="platform",
          lines=["resolve agent → definition; def.TenantID must equal the caller's tenant (403)",
                 "CountActiveRuns(tenant) ≥ tenant.max_concurrent_runs → 429 + retry_after_seconds",
                 "pin def.Digest on the run: later edits to the agent cannot widen this run",
                 "INSERT runs(state=QUEUED, next_seq=1) + events(seq 0, RUN_CREATED) — one txn",
                 "no worker is contacted: creating a run is writing a row",
                 "returns 201 with the run; the dispatcher will find it within one poll (100 ms)"])
    d.box("stream", 500, 484, 450, 108, "streamRun — SSE built on the event log itself", kind="platform",
          lines=["poll ListEvents(run, since) every 400 ms; emit each event as `data:` JSON",
                 "since = last seq + 1 → a reconnecting client never sees a gap or a duplicate",
                 "the console sees exactly the events the model's context was rebuilt from",
                 "no pub/sub, no fan-out service: Postgres is the bus (designed: LISTEN/NOTIFY)"])
    d.box("human_actions", 500, 616, 450, 118, "resume · approve · cancel — AppendSystemEvents", kind="platform",
          lines=["precondition on state: resume needs WAITING_HUMAN, approve needs WAITING_APPROVAL",
                 "append HUMAN_RESUME / APPROVAL_GIVEN / RUN_FINISHED and flip state in one txn",
                 "no fencing token: by construction no worker holds a lease on a parked run",
                 "approve re-queues the run; the worker retries the SAME idem key with Approved=true"])
    d.box("reads", 500, 758, 450, 100, "Read models", kind="platform",
          lines=["/v1/overview: runs by state, cost by tenant — from runs.usage, no separate ledger",
                 "/v1/quota: limiter.Snapshot() — guaranteed rate, tokens, throttled flag per tenant",
                 "/v1/audit: hash-chained records + VerifyChain on demand (operators see all tenants)",
                 "/v1/tools: Tool.MarshalJSON emits the model-facing schema ONLY — never the backend"])

    # state
    d.box("pg", 1050, 300, 380, 330, "PostgreSQL", kind="state", mono=True,
          lines=["tenants          weight · tpm · max_concurrent_runs",
                 "agent_definitions digest PK (canonical JSON)",
                 "runs             state · next_seq · lease_*",
                 "events           (run_id, seq) append-only",
                 "audit_log        (tenant_id, seq) hash chain",
                 "",
                 "idx_runs_tenant_state   ← CountActiveRuns",
                 "idx_runs_dispatch       ← workers, not the API",
                 "events PK (run_id,seq)  ← SSE `since` scan",
                 "",
                 "the API never touches tool_calls:",
                 "the journal belongs to the gateway"])
    d.box("limiter", 1050, 660, 380, 96, "Fairness limiter (in-process)", kind="platform",
          lines=["read-only from the API: Snapshot() for /v1/quota",
                 "the API cannot reserve or settle — only workers do",
                 "gap: per-replica view; Redis-backed in the multi-replica design"])
    d.box("metrics", 1050, 786, 380, 72, "Metrics registry (in-process)", kind="platform",
          lines=["agentorch_runs{tenant,state} gauge moved on create/finish",
                 "11 series total; rendered on demand by GET /metrics"])

    # flows
    d.arrow("ci", "auth", "", src_side="s", dst_side="n", src_off=-50, dst_off=-100)
    d.arrow("human", "auth", "", src_side="s", dst_side="n", src_off=-60, dst_off=0,
            via=[(500, 216), (270, 216)])
    d.arrow("operator", "auth", "", src_side="s", dst_side="n", dst_off=100,
            via=[(915, 228), (370, 228)])
    d.arrow("scraper", "metrics", "", src_side="s", dst_side="n", style="dotted",
            via=[(1275, 230), (1470, 230), (1470, 770), (1240, 770)], src_off=0, dst_off=0)
    d.arrow("auth", "scope", "", src_side="s", dst_side="n")
    d.arrow("scope", "router", "", src_side="s", dst_side="n")
    d.arrow("router", "create", "", src_side="e", dst_side="w", src_off=-60, dst_off=0, color=BLUE)
    d.arrow("router", "stream", "", src_side="e", dst_side="w", src_off=-30, dst_off=0, color=BLUE)
    d.arrow("router", "human_actions", "", src_side="e", dst_side="w", src_off=0, dst_off=0, color=BLUE)
    d.arrow("router", "reads", "", src_side="e", dst_side="w", src_off=30, dst_off=0, color=BLUE)
    d.arrow("create", "pg", "one INSERT txn", src_side="e", dst_side="w", dst_off=-100, color=GREEN, width=2)
    d.arrow("stream", "pg", "ListEvents(since)", src_side="e", dst_side="w", dst_off=-40, style="dotted", color=GREEN)
    d.arrow("human_actions", "pg", "append + state flip", src_side="e", dst_side="w", dst_off=20, color=GREEN, width=2)
    d.arrow("reads", "pg", "tenant-scoped SELECTs", src_side="e", dst_side="w", dst_off=100, style="dotted", color=GREEN, label_at=(985, 745))
    d.arrow("reads", "limiter", "Snapshot()", src_side="e", dst_side="w", src_off=30, style="dotted",
            via=[(1000, 838), (1000, 708)])

    d.table(40, 912, [230, 380, 390, 420], [
        [["Failure"], ["Detected by"], ["What happens"], ["Invariant kept / trade-off"]],
        [["API replica dies mid-request"], ["client gets a reset; k8s readiness /healthz"],
         ["retry hits the other replica; a half-done create is impossible (one txn)"],
         ["no partial run rows · cost: clients must retry (no client idem key yet — gap)"]],
        [["Postgres unreachable"], ["store error → 500/503 to the caller"],
         ["nothing is queued anywhere else, so nothing is lost or duplicated"],
         ["the API is a thin projection of the DB; it has no state to reconcile"]],
        [["tenant floods POST /v1/runs"], ["CountActiveRuns ≥ max_concurrent_runs"],
         ["429 with retry_after_seconds; the queue never fills with unrunnable work"],
         ["a burst cannot consume the worker pool · trade-off: rejected work is the tenant's problem"]],
    ], kind="platform")

    d.legend = [("human", "callers"), ("platform", "control-plane code"), ("state", "durable state"),
                ("neutral", "static assets")]
    return d

if __name__ == "__main__":
    build().save("../svg/01-control-plane.svg")

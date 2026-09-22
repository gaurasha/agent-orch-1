# API reference

> **Prerequisite:** [Data model](02-data-model.md)
> **Read next:** [Configuration](04-configuration.md)
> **Source:** [`internal/api/api.go`](../../backend/internal/api/api.go) · [`internal/gateway/gateway.go`](../../backend/internal/gateway/gateway.go)

Two HTTP surfaces with different audiences and different threat models.

| Surface | Port | Callers | Auth |
|---|---|---|---|
| **Control plane** | 8080 | humans, CI, the console | API key → `Principal` |
| **Tool gateway** | 8081 | agent workers **only** | run-scoped JWT + NetworkPolicy |

---

## Authentication

### Control plane

```http
Authorization: Bearer <api-key>
```

Resolved to:

```go
type Principal struct {
    TenantID string
    User     string
    Operator bool   // may read across tenants; audited like everything else
}
```

Demo keys (seeded; **not** a production auth model — production uses OIDC with
IdP group mapping):

| Key | Principal |
|---|---|
| `demo-operator-key` | `acme` / `operator@agentorch.dev` / **operator** |
| `acme-key` | `acme` / `alice@acme.example` |
| `globex-key` | `globex` / `bob@globex.example` |

**Tenancy is enforced on every route.** `?tenant=` is honoured only for
operators; everyone else is pinned to their own. Cross-tenant reads return
**404, not 403** — a 403 confirms the resource exists.

> **⚠ Known gap:** `/v1/runs/{id}/stream` also accepts `?api_key=` because
> `EventSource` cannot set headers, so keys land in access logs. Production needs
> a short-lived signed stream token.

### Tool gateway

```http
Authorization: Bearer <run-scoped JWT>     # aud=tool-gateway, ≤5 min
```

```json
{ "sub": "run_…", "ten": "acme", "agn": "report-writer",
  "dig": "sha256:…", "usr": "alice@acme.example",
  "aud": "tool-gateway", "iat": 1700000000, "exp": 1700000300 }
```

The gateway verifies the signature, expiry and **audience**, then **reloads the
run from the store** and cross-checks that the token's tenant matches the run's.
Nothing in the request is trusted.

`alg` is pinned to `HS256`; anything else — including `none` — is rejected
outright.

---

## Control plane

### `GET /healthz`

Pings the store. `200` or `503`. Used by readiness and liveness probes.

### `GET /metrics`

Prometheus text exposition. See [Observability](../03-operations/03-observability.md).

---

### `GET /v1/tenants`

Non-operators see only their own.

```json
{"tenants":[{"id":"acme","name":"Acme Corp","weight":2,
             "tokens_per_minute":300000,"max_concurrent_runs":500,
             "created_at":"2026-09-22T10:44:00Z"}]}
```

---

### `GET /v1/agents`

```json
{"agents":[{"digest":"sha256:a1b2…","tenant_id":"acme","name":"report-writer",
            "spec":{"system_prompt":"…","model":"fake:report-writer",
                    "tools":["fs.write","fs.read","fs.list","doc.convert","exec.bash"],
                    "budget":{…},"priority":"normal"},
            "created_at":"…"}]}
```

### `POST /v1/agents`

```json
{ "name": "report-writer",
  "spec": { "system_prompt": "You write business reports.",
            "model": "fake:report-writer",
            "tools": ["fs.write", "doc.convert"],
            "tool_params": { "github.cli": { "denied_arg_patterns": ["auth token"] } },
            "budget": { "max_steps": 10, "max_tool_calls": 20, "max_cost_usd": 1.0 },
            "priority": "normal" } }
```

**Validation, in order:**

1. `name` required
2. `spec.Validate()` — model and system prompt present, priority recognised
3. **Every tool in `tools` must exist in the registry** — a typo fails here,
   while a human is looking at it, rather than at 3am inside a run
4. Defaults filled: `priority=normal`, `budget=DefaultBudget()`

`201` with the definition, including its computed `digest`. **Idempotent by
content** — registering the same spec returns the same digest and writes nothing.

`400` lists the available tools when one is unknown.

---

### `GET /v1/runs`

| Param | Meaning |
|---|---|
| `state` | comma-separated, e.g. `QUEUED,RUNNING` |
| `tenant` | operators only |
| `limit` | default 100, **clamped to the maximum (1000), not to the default** |

> That clamping behaviour is deliberate and was a bug: asking for 2000 originally
> returned 200 — the *default* — so a caller believed it had seen everything when
> it had seen a fifth. See [Bugs found](../04-evidence/03-bugs-found.md).

### `POST /v1/runs`

```json
{ "agent_digest": "sha256:a1b2…", "input": "Write the Q3 report." }
```

or, for humans, `{"agent_name": "report-writer", "input": "…"}` — which resolves
to the newest definition of that name **once, at creation**, and pins the digest.

| Status | Meaning |
|---|---|
| `201` | Created; body is the run |
| `400` | Unknown agent, or malformed |
| `403` | That agent belongs to a different tenant |
| **`429`** | **Admission control** — tenant at `MaxConcurrentRuns` |

```json
{"error":"tenant acme already has 500 active runs (limit 500)",
 "active_runs":500,"limit":500,"retry_after_seconds":30}
```

Rejecting at admission with a clear message is far kinder than accepting work the
platform cannot get to.

---

### `GET /v1/runs/{id}`

Returns the run. `404` if it does not exist **or belongs to another tenant**.

```json
{"id":"run_01a0…","tenant_id":"acme","agent_name":"report-writer",
 "def_digest":"sha256:…","triggering_user":"alice@acme.example",
 "state":"SUCCEEDED","status_reason":"completed",
 "next_seq":12,"step":4,"priority":"normal",
 "budget":{"max_steps":10,"max_tool_calls":20,"max_cost_usd":1},
 "usage":{"steps":4,"tool_calls":3,"input_tokens":10453,
          "output_tokens":312,"cost_usd":0.16915},
 "lease_owner":"","created_at":"…","updated_at":"…"}
```

`lease_owner: ""` on a parked or finished run is the visible evidence that it
holds no worker.

### `GET /v1/runs/{id}/events?since=N`

The full event log. This is what *"what is agent X doing right now"* is answered
from — and it is the same log that rebuilds the model's context, so the operator
view cannot drift from what the agent experienced.

### `GET /v1/runs/{id}/stream`

Server-Sent Events.

```
event: run
data: {"id":"run_…","state":"RUNNING",…}

event: event
data: {"seq":7,"type":"TOOL_CALL","payload":{…}}

event: done
data: {"state":"SUCCEEDED"}
```

SSE rather than WebSocket: one-way, text, automatic browser reconnection, no
handshake, no framing library, no dependency.

**Implementation honesty:** this is a poll-and-push bridge (400 ms tick), not a
true change feed, and the stream is capped at 30 minutes so a forgotten tab does
not hold a connection forever. At 10× it should be Postgres `LISTEN/NOTIFY` —
one polling goroutine per open browser tab does not scale.

---

### `POST /v1/runs/{id}/resume`

```json
{"input": "Approved. Please continue."}
```

`409` unless the run is `WAITING_HUMAN`. Appends `HUMAN_RESUME`, clears `wake_at`,
returns the run to `QUEUED`.

### `POST /v1/runs/{id}/approve`

`409` unless `WAITING_APPROVAL`. Appends `APPROVAL_GIVEN` **with the approving
human's identity** — the approval is itself an audited event.

### `POST /v1/runs/{id}/cancel`

`409` if already terminal. Sets `CANCELLED` and releases the lease.

**Cancellation is cooperative-but-enforced:** the state change makes the gateway
refuse any further tool call from this run *immediately*, even if a worker is
mid-step and has not noticed.

---

### `GET /v1/audit`

| Param | |
|---|---|
| `tenant` | operators only |
| `run_id` | filter to one run |
| `limit` | default 500, clamped to 5000 |

```json
{"records":[{"tenant_id":"acme","seq":7,"ts":"…","run_id":"run_…",
             "agent_name":"report-writer","triggering_user":"alice@acme.example",
             "tool":"exec.bash","decision":"ALLOW","reason":"default.granted",
             "args_redacted":{"script":"ls -la /work"},
             "result_meta":{"phase":"post","driver":"namespace","exit_code":0,
                            "duration_ms":35,"def_digest":"sha256:…"},
             "prev_hash":"…","hash":"…"}],
 "count":14,
 "chain_verified":true}
```

**`chain_verified` is only present for a complete, tenant-scoped query** (a
`tenant` with no `run_id`). A filtered slice legitimately has sequence gaps, and
reporting those as tampering would train operators to ignore the warning.

On failure:

```json
{"chain_verified":false,
 "chain_error":"audit record seq 3 was modified after it was written (hash mismatch)"}
```

---

### `GET /v1/quota`

```json
{"tenants":[{"tenant_id":"acme","weight":2,"guaranteed_tpm":266666.7,
             "available_now":41234.5,"capacity_tokens":44444.4,
             "utilization_pct":7.2,"in_flight":3,"throttled":false}],
 "spare_tokens":1234.5}
```

### `GET /v1/overview`

One call for the dashboard: run counts by state, per-tenant cost/token/tool-call
totals, the quota snapshot, and the spare pool.

### `GET /v1/tools`

Model-facing **schemas only**. `tools.Tool` marshals to its schema, so backend
configuration (credential references, URL templates) cannot be exposed here even
by mistake — see [the registry split](../01-concepts/04-authorization.md).

---

## Tool gateway

### `POST /v1/toolcalls`

```http
POST /v1/toolcalls
Authorization: Bearer <run-scoped JWT>
Content-Type: application/json

{"tool":"exec.bash","args":{"script":"ls -la /work"},
 "idem_key":"run_01a0…:2:0","approved":false}
```

Body is capped at 1 MiB.

| Status | Meaning | `decision` |
|---|---|---|
| `200` | Executed, or replayed from the journal | `ALLOW` |
| `202` | **Needs human approval** — the worker parks the run | `NEEDS_APPROVAL` |
| `400` | Malformed body | `DENY` |
| `401` | Invalid or expired token (deliberately terse) | `DENY` |
| `403` | Policy denied | `DENY` |
| `404` | Unknown run | `DENY` |
| **`409`** | **A call with this idempotency key is still `IN_FLIGHT`** | `ALLOW` |
| `424` | The pinned agent definition is unavailable | `DENY` |
| `500` | Workspace unavailable | `DENY` |
| `502` | Invocation failed; outcome may be unknown | `ALLOW` |
| `503` | Could not journal the call — **refused to execute** | `DENY` |

```json
{"result":"…stdout…","is_error":false,"decision":"ALLOW",
 "duration_ms":35,"truncated":false,"replayed":false}
```

**What is absent from every response:** no credential, no URL template, no
internal hostname, no policy internals beyond the human-readable `reason`.

### The three interesting statuses

**`409`** — another worker (or our former self) is mid-call. The caller waits
rather than duplicating the side effect. The reaper eventually resolves a
genuinely abandoned call.

**`503`** — we could not write the idempotency journal, so we **refuse to
execute**. Never do something you cannot record.

**`replayed: true`** on a `200` means the side effect was **not** repeated:

```json
{"result":"Wrote 120 bytes to report.md.","decision":"ALLOW","replayed":true,
 "reason":"replayed from the idempotency journal; the side effect was not repeated"}
```

---

## Error philosophy

| Audience | Style | Example |
|---|---|---|
| **Unauthenticated caller** | Terse — learns nothing | `"invalid or expired run token"` |
| **The agent** | Specific — so it stops retrying | `"tool \"http.post\" is not in this agent's granted tool set (granted: fs.write, fs.read, exec.bash)"` |
| **The operator** | Full context in logs and audit | rule name, digest, idempotency key |

Telling the model exactly why it was denied is deliberate: it measurably stops
the loop where an agent retries the same forbidden call twenty times. The
disclosed information is the agent's **own** configuration, which it could infer
from which calls succeed anyway.

---

## Worked example

```bash
API=http://localhost:8080

DIGEST=$(curl -s -H "Authorization: Bearer acme-key" $API/v1/agents \
  | python3 -c "import sys,json;print([a['digest'] for a in json.load(sys.stdin)['agents'] if a['name']=='report-writer'][0])")

RUN=$(curl -s -X POST -H "Authorization: Bearer acme-key" -H 'Content-Type: application/json' \
  -d "{\"agent_digest\":\"$DIGEST\",\"input\":\"Write the Q3 report.\"}" $API/v1/runs \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")

curl -s -H "Authorization: Bearer acme-key" $API/v1/runs/$RUN
# → state=SUCCEEDED steps=4 tools=3 cost=$0.16880

# tenancy
curl -s -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer globex-key" $API/v1/runs/$RUN   # 404
curl -s -o /dev/null -w "%{http_code}\n"                                        $API/v1/runs/$RUN   # 401

curl -s -H "Authorization: Bearer acme-key" "$API/v1/audit?limit=500" | grep -o '"chain_verified":[a-z]*'
# → "chain_verified":true
```

---

**Next:** [Configuration](04-configuration.md).

# Data model

> **Prerequisite:** [Components](01-components.md) · [Durable execution](../01-concepts/02-durable-execution.md)
> **Read next:** [API reference](03-api.md)
> **Source:** [`internal/store/schema.sql`](../../backend/internal/store/schema.sql)

Six tables. Every column exists for a stated reason; where a column encodes a
design decision, that decision is named.

```
tenants ──┬─► agent_definitions ──┐
          │                        │  (digest pinned by the run)
          └─► runs ◄───────────────┘
                │
                ├─► events        (append-only; the agent IS this)
                ├─► tool_calls    (idempotency journal)
                └─► audit_log     (per-tenant hash chain)
```

---

## `tenants`

```sql
CREATE TABLE tenants (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    weight              INT  NOT NULL CHECK (weight > 0),
    tokens_per_minute   BIGINT NOT NULL CHECK (tokens_per_minute > 0),
    max_concurrent_runs INT  NOT NULL CHECK (max_concurrent_runs > 0),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

| Column | Purpose |
|---|---|
| `id` | The isolation unit. Appears in every other table |
| `name` | **Human label only — never used for authorization** |
| `weight` | Fair share of LLM quota: `weight/Σweights × provider_rate` |
| `tokens_per_minute` | The tenant's *own* contractual ceiling. A small plan does not get a large share just because the cluster is quiet |
| `max_concurrent_runs` | Admission control. Bounds blast radius from a runaway tenant |

The `CHECK` constraints are deliberate: a weight of 0 would mean division by
zero in the fairness calculation, and a `max_concurrent_runs` of 0 would silently
disable a tenant. Both are caught at the database rather than at first use.

---

## `agent_definitions`

```sql
CREATE TABLE agent_definitions (
    digest     TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    spec       JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_defs_tenant ON agent_definitions(tenant_id, name);
```

**`digest` is the primary key**, and that is the whole design:
`sha256(canonical_json(spec))`.

```sql
INSERT … ON CONFLICT (digest) DO NOTHING
```

A second write of the same digest is by definition a no-op — the digest *is* the
content, so attempting to overwrite would mean a hash collision, not an update.
Definitions are therefore immutable, and a run that pins a digest has pinned its
permissions for its lifetime. See [Authorization §3](../01-concepts/04-authorization.md#3-content-addressing-the-grant-set-cannot-move).

### What lives inside `spec`

```jsonc
{
  "system_prompt": "…",
  "model": "fake:report-writer",
  "tools": ["fs.write", "fs.read", "doc.convert", "exec.bash"],   // ← the capability set
  "tool_params": {
    "github.cli": { "denied_arg_patterns": ["auth token", "secret"] }
  },
  "budget":   { "max_steps": 10, "max_tool_calls": 20, "max_tokens": 100000,
                "max_cost_usd": 1.0, "max_wall_seconds": 600 },
  "priority": "normal"
}
```

`spec` is `JSONB` rather than normalised columns because it is **hashed as a
unit** and read as a unit. Normalising it would require reassembling it
byte-identically to re-derive the digest — a reliable source of bugs.

---

## `runs` — the agent itself

```sql
CREATE TABLE runs (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    agent_name       TEXT NOT NULL,
    def_digest       TEXT NOT NULL,
    triggering_user  TEXT NOT NULL,
    state            TEXT NOT NULL,
    status_reason    TEXT NOT NULL DEFAULT '',
    next_seq         INT  NOT NULL DEFAULT 0,
    step             INT  NOT NULL DEFAULT 0,
    priority         TEXT NOT NULL DEFAULT 'normal',
    budget           JSONB NOT NULL,
    usage            JSONB NOT NULL,
    lease_owner      TEXT,
    lease_expires_at TIMESTAMPTZ,
    wake_at          TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Note what is absent

**No pod name. No node. No process ID. No connection handle.** A run is not
bound to any compute. That absence is the architecture.

### The load-bearing columns

| Column | Why it matters |
|---|---|
| `def_digest` | Permissions come from *this*, not from the current definition |
| `triggering_user` | Rides into every audit record. "The agent did it" is not an answer to "who did this" |
| `next_seq` | Doubles as an **optimistic-concurrency version**. `Commit` requires it to match |
| `lease_owner` + `lease_expires_at` | At-most-one-active-worker **and** the entire failure detector. A dead worker stops renewing; there is no detector to get wrong |
| `wake_at` | Parks a run until a deadline (quota backoff, provider backoff, human timeout) without occupying anything |
| `budget` / `usage` | JSONB because they evolve together and are always read as a unit |

### Indexes, and the query each serves

```sql
CREATE INDEX idx_runs_dispatch
    ON runs (priority DESC, created_at)
    WHERE state = 'QUEUED';
```

The dispatcher's hot query. A **partial** index means it stays small regardless
of how much terminal history accumulates — at a million finished runs, the index
still contains only what is queued.

```sql
CREATE INDEX idx_runs_tenant_state ON runs (tenant_id, state);   -- console listings
CREATE INDEX idx_runs_lease ON runs (lease_expires_at) WHERE lease_owner IS NOT NULL;  -- reaper
```

### State machine

```
QUEUED ──► RUNNING ──► SUCCEEDED | FAILED
   ▲          │
   │          ├──► WAITING_HUMAN ────┐  (no worker, no sandbox, no quota)
   │          ├──► WAITING_APPROVAL ─┤
   │          └──► QUEUED (quota / provider / step-budget / lease lapsed)
   └──────────────────────────────────┘
        ──► CANCELLED (from any non-terminal state)
```

Full diagram: [lifecycle](../diagrams/lifecycle.md).

---

## `events` — append-only, and the agent's memory

```sql
CREATE TABLE events (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq        INT  NOT NULL,
    type       TEXT NOT NULL,
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);
```

**There is no `UPDATE` or `DELETE` path in the code.** The composite primary key
`(run_id, seq)` gives densely-ordered history per run and makes a duplicate `seq`
a constraint violation rather than silent corruption.

### The 14 event types

| Type | Written by | In the model's context? |
|---|---|---|
| `RUN_CREATED` | API | ✓ |
| `USER_MESSAGE` | API | ✓ |
| `MODEL_RESPONSE` | worker | ✓ |
| `TOOL_CALL` | worker | ✓ (attached to the assistant turn) |
| `TOOL_RESULT` | worker | ✓ |
| `TOOL_DENIED` | worker | ✓ (the reason only) |
| `APPROVAL_NEEDED` | worker | ✓ |
| `APPROVAL_GIVEN` | API | ✓ |
| `HUMAN_RESUME` | API | ✓ |
| `HUMAN_PAUSE` | worker | ✗ operator-facing |
| `RUN_FINISHED` | worker | ✗ |
| `LEASE_LOST` | worker | ✗ |
| `NOTE` | worker | ✗ |
| `MODEL_REQUEST` | *(reserved)* | ✗ |

The right-hand column is a **security and cost decision**. `NOTE` events (quota
waits, provider retries, lease churn) cost money on every subsequent turn if
included, and give a prompt-injected agent material to reason about the platform
with. The operator sees everything; the model sees a curated projection. Both
are built from the same log, so the operator view cannot drift from reality.

### `payload` is a flat union

```go
type EventPayload struct {
    Text string
    // model turn
    Model string; InputTokens, OutputTokens int64; CostUSD float64; StopReason string
    // tool turn
    ToolCallID, Tool string; Args map[string]any; IdemKey string
    Result string; IsError bool; DurationMS int64; Decision, Reason string
    // bookkeeping
    Worker string
}
```

Only the fields relevant to `type` are set. A closed struct rather than
`map[string]any` keeps the log self-describing, queryable from SQL, and typed in
the UI — at the cost of some unused fields per row, which JSONB compresses away.

---

## `tool_calls` — the idempotency journal

```sql
CREATE TABLE tool_calls (
    idem_key    TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL,
    tenant_id   TEXT NOT NULL,
    tool        TEXT NOT NULL,
    args_hash   TEXT NOT NULL,
    state       TEXT NOT NULL,      -- IN_FLIGHT | DONE | FAILED | DENIED
    result      TEXT NOT NULL DEFAULT '',
    is_error    BOOLEAN NOT NULL DEFAULT false,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX idx_toolcalls_inflight ON tool_calls (started_at) WHERE state = 'IN_FLIGHT';
```

`idem_key` is the primary key and is **deterministic**: `run:step:index`. A
replaying worker reaches the same position and computes the same key,
necessarily.

```sql
INSERT … ON CONFLICT (idem_key) DO NOTHING;
-- rows affected == 1  ⟹  you own the side effect
-- rows affected == 0  ⟹  re-SELECT; use the recorded outcome
```

Exactly one caller is told to execute, verified with 12 concurrent racers.

**`IN_FLIGHT` is the honest state.** It means *"we told the outside world to do
something and do not yet know whether it happened"*. A partial index keeps the
reaper's scan tiny. See
[Durable execution §6](../01-concepts/02-durable-execution.md#6-idempotency-making-replay-safe).

`args_hash` lets an auditor verify the full arguments without the journal storing
them.

---

## `audit_log` — per-tenant hash chain

```sql
CREATE TABLE audit_log (
    tenant_id       TEXT NOT NULL,
    seq             BIGINT NOT NULL,
    ts              TIMESTAMPTZ NOT NULL DEFAULT now(),
    run_id          TEXT NOT NULL,
    agent_name      TEXT NOT NULL,
    triggering_user TEXT NOT NULL,
    tool            TEXT NOT NULL,
    decision        TEXT NOT NULL,      -- ALLOW | DENY | NEEDS_APPROVAL | ERROR
    reason          TEXT NOT NULL DEFAULT '',
    args_redacted   JSONB,
    result_meta     JSONB,
    prev_hash       TEXT NOT NULL,
    hash            TEXT NOT NULL,
    PRIMARY KEY (tenant_id, seq)
);
CREATE INDEX idx_audit_run       ON audit_log (run_id, seq);
CREATE INDEX idx_audit_tenant_ts ON audit_log (tenant_id, ts);
```

`PRIMARY KEY (tenant_id, seq)` makes the chain **per tenant**: appends do not
serialise across the fleet, and a per-tenant export is self-verifying.

`idx_audit_tenant_ts` is the index that answers *"every command agent X ran last
Tuesday"* as a range scan.

**Two records per call** (`result_meta.phase` = `pre` / `post`) so that a crash
mid-call still leaves evidence the attempt was made.

> **The timestamp bug.** `ts` is inside the hash, so backdating breaks the chain
> — which also means Go's nanosecond precision must be truncated to Postgres's
> microseconds *before* hashing, or the chain never re-verifies after a round
> trip. See [Audit §6](../01-concepts/07-audit.md).

---

## Transaction boundaries

The reason all of this is in one database:

```go
func (p *PG) Commit(ctx, runID, worker string, expectedNextSeq int, evs []Event, up RunUpdate) error {
    return p.tx(ctx, func(t *sql.Tx) error {
        // 1. lock the run row; verify the lease is OURS and unexpired   ← fencing
        // 2. verify next_seq matches                                    ← OCC
        // 3. INSERT the events
        // 4. UPDATE the run
    })
}
```

All four succeed or none do. Split across two systems — say a log in Kafka and
state in Postgres — and you get a window where the events landed but the state
did not, or vice versa. That window is where the hard bugs live. This is the
core argument for Postgres over a workflow engine plus a separate log
([DEEP_DIVE D4](../../DEEP_DIVE.md#d4--durable-execution-substrate)).

---

## Growth and retention

| Table | Rows per 1000 runs | At 10× peak |
|---|---|---|
| `runs` | 1,000 (~2 KB each) | bounded by run rate |
| `events` | ~15,000 (~1–2 KB each) | **~288 GB/day** ⚠ |
| `tool_calls` | ~5,000 | grows with tool calls |
| `audit_log` | ~10,000 (2 per call) | grows with tool calls |

**`events` is the first thing that breaks**, and nothing is implemented to
manage it. The plan:

1. Partition `events` by month (`PARTITION BY RANGE (created_at)`)
2. Tier partitions older than the retention window to object storage as Parquet
3. Keep run *state* in Postgres indefinitely — it is small
4. Retain `audit_log` per the compliance requirement, which is usually longer
   than the operational need for `events`

See [Capacity planning](../03-operations/05-capacity.md).

---

**Next:** [API reference](03-api.md).

-- Schema for the agent orchestration control plane.
--
-- Design notes that matter:
--  * runs.next_seq doubles as an optimistic-concurrency version.
--  * events is append-only; there is no UPDATE or DELETE path in the code.
--  * the lease columns on runs replace a separate failure detector.
--  * audit_log is a per-tenant hash chain (see store.AuditRecord).

CREATE TABLE IF NOT EXISTS tenants (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    weight              INT  NOT NULL CHECK (weight > 0),
    tokens_per_minute   BIGINT NOT NULL CHECK (tokens_per_minute > 0),
    max_concurrent_runs INT  NOT NULL CHECK (max_concurrent_runs > 0),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS agent_definitions (
    digest     TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    spec       JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_defs_tenant ON agent_definitions(tenant_id, name);

CREATE TABLE IF NOT EXISTS runs (
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

-- The dispatcher's hot query: "give me the next runnable row". A partial index
-- keeps it tiny regardless of how much terminal history has accumulated.
CREATE INDEX IF NOT EXISTS idx_runs_dispatch
    ON runs (priority DESC, created_at)
    WHERE state = 'QUEUED';
CREATE INDEX IF NOT EXISTS idx_runs_tenant_state ON runs (tenant_id, state);
CREATE INDEX IF NOT EXISTS idx_runs_lease ON runs (lease_expires_at) WHERE lease_owner IS NOT NULL;

CREATE TABLE IF NOT EXISTS events (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq        INT  NOT NULL,
    type       TEXT NOT NULL,
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS tool_calls (
    idem_key    TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL,
    tenant_id   TEXT NOT NULL,
    tool        TEXT NOT NULL,
    args_hash   TEXT NOT NULL,
    state       TEXT NOT NULL,
    result      TEXT NOT NULL DEFAULT '',
    is_error    BOOLEAN NOT NULL DEFAULT false,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_toolcalls_inflight
    ON tool_calls (started_at) WHERE state = 'IN_FLIGHT';
CREATE INDEX IF NOT EXISTS idx_toolcalls_run ON tool_calls (run_id);

CREATE TABLE IF NOT EXISTS audit_log (
    tenant_id       TEXT NOT NULL,
    seq             BIGINT NOT NULL,
    ts              TIMESTAMPTZ NOT NULL DEFAULT now(),
    run_id          TEXT NOT NULL,
    agent_name      TEXT NOT NULL,
    triggering_user TEXT NOT NULL,
    tool            TEXT NOT NULL,
    decision        TEXT NOT NULL,
    reason          TEXT NOT NULL DEFAULT '',
    args_redacted   JSONB,
    result_meta     JSONB,
    prev_hash       TEXT NOT NULL,
    hash            TEXT NOT NULL,
    PRIMARY KEY (tenant_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_audit_run ON audit_log (run_id, seq);
-- "every command agent X ran last Tuesday" is a range scan on this index.
CREATE INDEX IF NOT EXISTS idx_audit_tenant_ts ON audit_log (tenant_id, ts);

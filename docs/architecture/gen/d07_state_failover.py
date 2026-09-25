from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE, AMBER = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf", "#b8860b"

def build():
    d = Diagram(1560, 1230, "07 · State and failover — six tables, four transactions, every failure converges on the same row",
                "Postgres is the queue, the lock service, the event store, the idempotency journal and the audit log. That is a deliberate choice, and this is what it buys.")

    d.zone(40, 86, 900, 560, "SCHEMA · internal/store/schema.sql", "state",
           "append-only where it matters; every hot query has a partial index", subtitle_inside=True)
    d.zone(980, 86, 540, 560, "THE FOUR TRANSACTIONS", "tcb", "each is one BEGIN…COMMIT; none spans a network call", subtitle_inside=True)

    d.box("tenants", 70, 130, 260, 92, "tenants", kind="state", mono=True,
          lines=["id PK · name", "weight >0 · tokens_per_minute >0", "max_concurrent_runs >0"])
    d.box("defs", 70, 250, 260, 92, "agent_definitions", kind="state", mono=True,
          lines=["digest PK  = sha256(canonical JSON)", "tenant_id FK · name · spec JSONB", "immutable: ON CONFLICT DO NOTHING"])
    d.box("runs", 360, 130, 300, 212, "runs — the row", kind="state", mono=True,
          lines=["id PK · tenant_id FK · def_digest", "state · status_reason · priority",
                 "next_seq   ← OCC version", "step · budget JSONB · usage JSONB",
                 "lease_owner · lease_expires_at", "wake_at · created_at · updated_at",
                 "", "idx_runs_dispatch (priority DESC, created_at)", "   WHERE state='QUEUED'   ← the queue",
                 "idx_runs_lease (lease_expires_at)", "   WHERE lease_owner IS NOT NULL ← reaper"])
    d.box("events", 690, 130, 230, 120, "events — the agent", kind="state", mono=True,
          lines=["run_id FK · seq", "PK (run_id, seq)", "type · payload JSONB", "created_at", "", "no UPDATE/DELETE path exists", "dotted lines = foreign keys"])
    d.box("toolcalls", 690, 278, 230, 120, "tool_calls — the journal", kind="state", mono=True,
          lines=["idem_key PK  run:step:i", "run_id (no FK: outlives runs)", "tenant_id · tool · args_hash", "state · result · is_error",
                 "started_at · finished_at", "idx (started_at) WHERE IN_FLIGHT"])
    d.box("audit", 70, 370, 590, 130, "audit_log — the tenant hash chain", kind="state", mono=True,
          lines=["PK (tenant_id, seq) · ts (µs) · run_id · agent_name · triggering_user · tool · decision · reason",
                 "args_redacted JSONB · result_meta JSONB · prev_hash · hash",
                 "hash = sha256(prev_hash ‖ canonical(record))     genesis prev_hash for seq 1",
                 "idx_audit_run (run_id, seq) · idx_audit_tenant_ts (tenant_id, ts) ← 'everything agent X ran last Tuesday'",
                 "VerifyChain(records) recomputes and reports the first broken seq"])
    d.arrow("defs", "tenants", "", src_side="n", dst_side="s", style="dotted")
    d.arrow("runs", "tenants", "", src_side="w", dst_side="e", src_off=-60, style="dotted")
    d.arrow("runs", "defs", "", src_side="w", dst_side="e", src_off=40, dst_off=0, style="dotted", via=[(345, 276), (345, 296)])
    d.arrow("events", "runs", "", src_side="w", dst_side="e", dst_off=-46, style="dotted")
    d.arrow("toolcalls", "runs", "", src_side="w", dst_side="e", dst_off=100, style="dotted")
    d.arrow("audit", "runs", "", src_side="n", dst_side="s", src_off=200, dst_off=0, style="dotted")
    d.note(70, 520, 850, kind="state", title="Why one database and not five systems",
           lines=["the lease, the event append, the state flip and the usage update happen in ONE transaction — there is no window where two of them disagree",
                  "a broker + a KV lock + an event store + a cache would each add a consistency seam that the fencing argument would have to cross",
                  "cost: write throughput is one Postgres primary (measured 102 → 5 394 runs/s from 2 → 24 workers on a 4-core VM with a stubbed model; 10k tool calls/min needs partitioning + tiering)"])

    T, TW = 1010, 480
    d.box("t1", T, 130, TW, 96, "AcquireLease(worker, ttl, eligible)", kind="tcb", mono=True,
          lines=["SELECT id FROM runs WHERE state='QUEUED' AND wake_at≤now()",
                 "  AND tenant_id = ANY($eligible)",
                 "  ORDER BY priority DESC, created_at FOR UPDATE SKIP LOCKED LIMIT 1;",
                 "UPDATE runs SET state='RUNNING', lease_owner=$w, lease_expires_at=now()+ttl"])
    d.box("t2", T, 250, TW, 110, "Commit(run, worker, expectedNextSeq, events, update)", kind="tcb", mono=True,
          lines=["SELECT next_seq, lease_owner, lease_expires_at FROM runs WHERE id=$1 FOR UPDATE;",
                 "owner≠worker OR expired    → ROLLBACK ErrLeaseLost   (fence)",
                 "next_seq ≠ expected        → ROLLBACK ErrConflict    (stale replay)",
                 "INSERT events (run_id, seq, type, payload) × n;",
                 "UPDATE runs SET next_seq=seq+n, step, usage, [state], [lease NULL], [wake_at]"])
    d.box("t3", T, 384, TW, 96, "BeginToolCall / FinishToolCall", kind="tcb", mono=True,
          lines=["INSERT tool_calls(idem_key,…,'IN_FLIGHT') ON CONFLICT DO NOTHING;",
                 "SELECT * FROM tool_calls WHERE idem_key=$1   → fresh? existing?",
                 "… side effect happens outside any transaction …",
                 "UPDATE tool_calls SET state, result, is_error, finished_at WHERE idem_key=$1"])
    d.box("t4", T, 504, TW, 110, "AppendAudit(record)", kind="tcb", mono=True,
          lines=["SELECT seq, hash FROM audit_log WHERE tenant_id=$1",
                 "  ORDER BY seq DESC LIMIT 1 FOR UPDATE;      ← serialises one tenant's chain",
                 "rec.ts = now().Truncate(µs); rec.prev_hash = last.hash (or genesis)",
                 "rec.hash = sha256(prev ‖ canonical(rec));",
                 "INSERT audit_log(…, prev_hash, hash)"])

    d.zone(40, 680, 1480, 260, "FAILOVER · what breaks, how it is noticed, where it converges", "untrusted",
           "there is exactly one recovery path: the row's lease expires and the reaper re-queues it; everything else is a special case of that", subtitle_inside=True)
    d.table(60, 722, [270, 300, 420, 240, 210], [
        [["Failure"], ["Noticed by"], ["Recovery"], ["Lost"], ["Bound"]],
        [["agentd SIGKILL mid-step"], ["lease stops renewing"], ["reaper: RUNNING+expired → QUEUED; another worker replays the log; idem keys make tool calls exactly-once"],
         ["the in-memory partial step (never written)"], ["≤ TTL 30 s + reaper 5 s"]],
        [["agentd SIGTERM (rolling deploy)"], ["ctx cancelled"], ["deferred fenced YieldRun → QUEUED immediately; TestDurability_RollingDeployLosesNoWork"],
         ["nothing"], ["≈ 0 s"]],
        [["agentd paused (GC / partition)"], ["its own Commit"], ["fence: owner≠me → ErrLeaseLost → abandon; the replacement's writes stand"],
         ["nothing; no duplicate side effect"], ["at Commit time"]],
        [["gateway dies mid tool call"], ["tool_calls row stays IN_FLIGHT"], ["reaper (stuck_after) → FAILED 'MAY OR MAY NOT have taken effect'; the model is told the truth"],
         ["certainty about one side effect"], ["stuck_after"]],
        [["Postgres primary failover"], ["every client's txn aborts"], ["managed HA promotes a replica; leases are judged by DB now(), so expiry math survives; clients reconnect and retry"],
         ["uncommitted txns only (by definition)"], ["provider RTO (~30–60 s)"]],
    ], kind="untrusted", size=10)

    d.table(40, 960, [370, 560, 550], [
        [["Designed, not built"], ["Why the design needs it"], ["What the PoC does instead"]],
        [["object storage checkpoints of /work at tool-call boundaries"], ["a replacement worker on another node must see the same files, not just the same log"],
         ["single-node bind mount; WorkspaceManager owns the path"]],
        [["cold tiering of events/audit partitions to Parquet"], ["10k tool calls/min ≈ 4 audit rows/s/tenant forever; hot Postgres should hold days, not years"],
         ["one table each; indexes keep the hot queries small"]],
        [["LISTEN/NOTIFY or a read replica for the console"], ["SSE polling every 400 ms × N open consoles is a read load the primary should not carry"],
         ["poll ListEvents(since) on the primary"]],
        [["Redis-backed limiter buckets"], ["N gateway/worker replicas each admit independently today"], ["per-process buckets, documented as a gap"]],
    ], kind="state", size=10)

    d.legend = [("state", "table / durable"), ("tcb", "transaction"), ("external", "failure")]
    return d

if __name__ == "__main__":
    build().save("../svg/07-state-failover.svg")

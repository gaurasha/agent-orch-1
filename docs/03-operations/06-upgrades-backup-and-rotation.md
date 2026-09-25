# Upgrades, schema evolution, backup, restore and rotation

> The procedures the earlier operations docs assume but do not spell out. Each
> one is written against how the code actually behaves (single binary, leases
> judged by the database clock, additive schema, short-lived tokens), so the
> reader can predict what happens during the procedure rather than hope.
>
> Related: [deployment](02-deployment.md) · [runbook](04-runbook.md) · [state and failover](../architecture/07-state-failover.md) · [limits and defaults](../05-reference/05-limits-and-defaults.md)

## 1. Rolling upgrades

**What makes them safe.** One image runs every role, so the components never
disagree about wire formats. Workers stop gracefully: on `SIGTERM` the
deferred fenced `YieldRun` puts every in-flight run back to `QUEUED`
immediately (`TestDurability_RollingDeployLosesNoWork`); a worker killed
harder simply lets its lease expire (≤ 30 s + 3 s). The gateway is stateless
between calls; the journal is in Postgres. The control plane is stateless;
SSE clients reconnect with `since`.

**Order.** With additive schema changes (below) the order does not matter for
correctness. The order that minimises visible effect is: schema → gateway →
workers → control plane. Argo CD applies the whole kustomization; Deployments
roll independently under their PDBs.

**Compatibility contract between versions** (what a change must not break
within one rollout window):

| Interface | Must stay compatible across N and N+1 |
|---|---|
| run JWT (`jwtmini`): claims `sub`, `tenant`, `aud=tool-gateway`, `exp` | a gateway on N+1 must verify tokens signed by a worker on N (≤ 5 min old) |
| `POST /v1/toolcalls` body/response | additive fields only |
| `events.payload` | additive fields only; `Rebuild()` must tolerate unknown fields and missing new ones |
| `runs.state`, `tool_calls.state`, rule names | never renamed; a new value is a new row in the reference docs |
| `agent_definitions.spec` | additive with `omitempty`, or existing digests change meaning — never |

**Rollback** is a git revert; Argo syncs it. Because runs are durable and
replayed from the log, a rollback mid-run continues the run on the older code,
provided the contract above held.

## 2. Schema evolution

`internal/store/schema.sql` is applied at startup and is idempotent
(`CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`). There is no
migration tool; the rule set that makes this safe:

1. **Additive only, idempotent.** New column: `ALTER TABLE … ADD COLUMN IF NOT
   EXISTS … DEFAULT …` appended to `schema.sql`; new index:
   `CREATE INDEX CONCURRENTLY IF NOT EXISTS` — run it out of band on large
   tables, since startup runs it in a transaction.
2. **Expand → migrate → contract.** Add the new column, deploy code that
   writes both, backfill, deploy code that reads the new column, drop the old
   one in a later release. Never rename in place.
3. **Never `UPDATE`/`DELETE` events or audit rows** in a migration; a
   backfill that touches `audit_log` breaks the chain by definition. Add a
   column, leave the hashed fields alone.
4. **Test against Postgres**, not only the in-memory store: the conformance
   suite exists because the two differ (B4, B5).

Production adds a migration runner (`golang-migrate`, `atlas`, `sqitch`) with
a version table and CI that applies migrations to a copy of production before
merge; the rules above do not change.

## 3. Backup

**What is in Postgres:** everything durable except workspaces — runs,
events, definitions, tenants, the tool-call journal, the audit chain.
**What is not:** `/var/lib/agentorch/workspaces/<tenant>/<run>/` on the
gateway's node (the object-storage checkpoint gap), and in-memory state that
is designed to be lost (limiter buckets, API-key map in the PoC).

Use the managed provider's continuous backup (WAL archiving) with
point-in-time recovery; `pg_dump` nightly as a second, portable copy. Verify
restores on a schedule — a backup that has never been restored is a hope.

## 4. Restore — what the system does after `restore to T`

Because every actor judges time by the database's `now()` and every
transition is a row, a restored database is *consistent* and *self-healing*:

| State at T | After restore, at wall-clock now ≫ T | Why |
|---|---|---|
| runs `RUNNING` with `lease_expires_at ≈ T + 30 s` | the reaper's first tick re-queues them (`lease expired; reclaimed`); a worker replays from the log | leases are compared with `now()`, which is now far past T |
| `tool_calls` `IN_FLIGHT` | after `stuck_after` (2 min) they are `FAILED` "MAY OR MAY NOT have taken effect"; the replaying agent is told so | the side effect may have happened between T and the crash |
| tool calls `DONE` after T (lost) | the replaying agent re-issues the same `run:step:i`; the journal has no row → **the side effect is repeated** | this is the RPO cost — a duplicate PR or POST for every allowed call in the lost window |
| `events` after T (lost) | the run replays from the last surviving event; steps after T are redone | model cost is re-incurred |
| audit chain | intact up to T; run `VerifyChain` per tenant after restore to prove it | hashes are in the rows |
| workspaces | whatever is on the node's disk — may be *ahead* of the database (files from steps after T) | the agent will see files it has "not yet" written; benign for idempotent scripts, confusing otherwise — a reason for checkpoints tied to tool-call boundaries |
| `runs` with `wake_at` | honoured relative to `now()` — effectively immediate | |

So: **RPO = the WAL shipping interval**, and the concrete cost of losing a
window is *duplicated allowed side effects for calls completed in that
window*. Keep the window seconds, not minutes, for tenants running
`Dangerous` tools.

**Procedure:** stop workers and the gateway (scale to 0) → restore → verify
chains → scale the gateway up → scale workers up → watch
`leases_reaped_total` jump once and settle → inform tenants whose runs had
allowed side effects in the lost window (the audit `pre` records that survived
tell you which).

## 5. Disaster recovery

| Scenario | RTO driver | Notes |
|---|---|---|
| primary fails, replica promoted | provider failover (~30–60 s) + reaper 3 s | clients reconnect; no data loss beyond replication lag |
| region loss with a cross-region replica | promotion + DNS + the restore table above for the lag window | workspaces on the lost nodes are gone; runs replay |
| total loss, restore from backup | restore time + the table above | |

Nothing outside Postgres needs restoring: images are in the registry,
manifests in git, credentials are minted from roots held by the broker
(production: a KMS/Vault whose own DR applies).

## 6. Rotation

| Secret | Lifetime of what it signs/protects | Procedure today | Production step |
|---|---|---|---|
| `AGENTORCH_SECRET` (run-token HMAC) | tokens live **5 min** | change the Kubernetes Secret; roll gateway **and** workers together. Tokens signed with the old key fail with 401 for up to 5 min; the worker records the call as a transport error ("outcome unknown"), the model retries, the journal prevents duplicates | dual-key verification (`kid` in the header, accept old+new for 5 min) makes rotation invisible |
| credential roots (`github/token`, `http/default` per tenant) | minted tokens live **≤ 60 s** | `AddRoot` with a new key; tokens minted from the old root fail verification within 60 s; a call in flight at the moment of rotation fails once and is retried | roots in KMS with key versions; `Verify` accepts the previous version for 60 s |
| tenant API keys | until rotated | replace the mapping; the old key is refused immediately | OIDC — rotation is the IdP's problem |
| Postgres password | connection lifetime | rotate at the provider, update the Secret, roll all three Deployments | IAM/database auth without passwords |
| sandbox image | per deploy | pin by digest; roll via git | Sigstore signing + admission verification |

Rotation never requires draining runs: nothing an agent does depends on a
long-lived secret.

## 7. Retention and deletion

* `events` and `audit_log` grow without bound; partition by month and tier
  cold partitions to object storage (designed). Chains verify across tiers
  because hashes are in the rows.
* Deleting a tenant cascades to definitions, runs and events; `audit_log` and
  `tool_calls` are retained deliberately ([offboarding](../06-guides/06-onboarding-a-tenant.md)).
* Workspaces are removed when a run reaches a terminal state; a crash between
  finish and removal leaves a directory — a periodic sweep of directories
  whose run is terminal is the housekeeping job.

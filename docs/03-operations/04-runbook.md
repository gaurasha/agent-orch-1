# Incident runbook

> **Prerequisite:** [Observability](03-observability.md)
> **Read next:** [Capacity planning](05-capacity.md)

One page per failure mode: symptom, diagnosis, action, and the prevention that
should already have fired.

---

## Triage

```
Is agentorch_audit_write_failures_total > 0?     → INC-1  (page)
Is the audit chain failing to verify?             → INC-2  (page)
Are DENY tool calls spiking for one run?          → INC-3
Is queue depth climbing with no progress?         → INC-4
Is one tenant persistently throttled?             → INC-5
Is a run stuck in RUNNING with no worker?         → INC-6
Is spend anomalous?                               → INC-7
Do you suspect a sandbox escape?                  → INC-8  (page, escalate)
```

---

## INC-1 — Audit writes are failing 🔴

**Severity: page.** We are executing side effects we cannot record.

```bash
kubectl -n agentorch logs -l app.kubernetes.io/name=toolgateway | grep "AUDIT WRITE FAILED"
kubectl -n agentorch exec sts/postgres -- pg_isready -U agentorch -d agentorch
kubectl -n agentorch exec sts/postgres -- df -h /var/lib/postgresql/data
```

| Cause | Action |
|---|---|
| Postgres down | Restore it; the gateway recovers automatically |
| Disk full | Extend the PVC; consider emergency `events` truncation (**never** `audit_log`) |
| Connection pool exhausted | Raise `maxConns`; check for leaked transactions |
| Schema drift | Compare against `schema.sql` |

**Why the call is not failed:** hiding a completed side effect from the operator
is worse than a gap we shouted about. The metric is the compensating control.

**Follow-up:** reconstruct the gap from `events` and `tool_calls`, which have
overlapping information.

---

## INC-2 — Audit chain verification failed 🔴

```bash
curl -s -H "Authorization: Bearer $OP_KEY" "$API/v1/audit?tenant=acme&limit=5000" | jq '.chain_error'
# → "audit record seq 1247 was modified after it was written (hash mismatch)"
```

**Two possibilities, and you must distinguish them:**

| | Evidence | Action |
|---|---|---|
| **Tampering** | Postgres audit logs show an out-of-band `UPDATE`/`DELETE` | **Security incident.** Freeze the tenant, preserve the database, escalate |
| **A bug in our hashing** | The break is at a *type* of record, or after a deploy | Compare `HashAudit` inputs before/after; check for precision or encoding changes |

> The second is not hypothetical: a Go-nanoseconds vs Postgres-microseconds
> mismatch once made **every** chain fail to verify. See
> [Audit §6](../01-concepts/07-audit.md). A break at a *single* sequence number
> points to tampering; a break *everywhere* points to us.

```bash
# Which sequence, and what is around it?
psql -c "SELECT seq, ts, run_id, tool, decision FROM audit_log
         WHERE tenant_id='acme' AND seq BETWEEN 1245 AND 1250 ORDER BY seq;"
```

---

## INC-3 — Denied tool calls spiking

```promql
topk(5, sum by (tenant, tool) (increase(agentorch_tool_calls_total{decision="DENY"}[15m])))
```

```bash
curl -s -H "Authorization: Bearer $OP_KEY" "$API/v1/audit?tenant=acme&limit=200" \
  | jq '.records[] | select(.decision=="DENY") | {ts, run_id, agent_name, tool, reason}'
```

| Pattern | Likely cause | Action |
|---|---|---|
| One run, several **different** tools, after a `fs.read` of external content | **Prompt injection** | Inspect the ingested document; cancel the run; consider freezing the tenant |
| Many runs, **one** tool, same reason | Misconfigured agent definition | Fix the grant set; this is an own-goal, not an attack |
| One run, **same** tool repeatedly | The model is not reading the denial | Check the reason is being surfaced; the budget will stop it |

**This is working as designed.** A denial means a control fired. The question is
*why the agent tried*, not whether the platform held.

```bash
curl -X POST -H "Authorization: Bearer $OP_KEY" "$API/v1/runs/$RUN/cancel"
```

---

## INC-4 — Queue depth climbing, nothing progressing

```promql
sum(agentorch_runs{state="QUEUED"})
sum(rate(agentorch_step_duration_seconds_count[5m]))     # ← 0 is the smoking gun
```

Work through in order:

```bash
# 1. Are workers alive?
kubectl -n agentorch get pods -l app.kubernetes.io/name=agentd

# 2. Is the database reachable from them?
kubectl -n agentorch logs -l app.kubernetes.io/name=agentd | grep -i "store\|postgres"

# 3. Is EVERY tenant out of quota?  (workers correctly do nothing)
curl -s -H "Authorization: Bearer $OP_KEY" $API/v1/quota | jq '.tenants[] | {tenant_id, throttled}'

# 4. Are leases being reaped abnormally? (workers dying mid-step)
kubectl -n agentorch logs -l app.kubernetes.io/name=agentd | grep "reclaimed runs"

# 5. Is the HPA stuck?
kubectl -n agentorch get hpa agentd
```

| Cause | Action |
|---|---|
| Workers crash-looping | Read the logs; likely config (`AGENTORCH_SECRET`, DSN) |
| DB unreachable | Workers **fail closed by design**. Fix the DB |
| All tenants throttled | Raise `--provider-tpm` if the contract allows |
| HPA at `maxReplicas` | Raise it; CPU is a poor proxy — see the queue-depth gap |
| Gateway unreachable | Check NetworkPolicy and the gateway's health |

---

## INC-5 — One tenant persistently throttled

```bash
curl -s -H "Authorization: Bearer $OP_KEY" $API/v1/quota | jq
```

```json
{"tenant_id":"acme","weight":2,"guaranteed_tpm":266666,
 "available_now":0,"in_flight":47,"throttled":true}
```

**This is the fairness layer working.** Decide which case it is:

| Case | Action |
|---|---|
| Legitimately busy; others idle | Check `spare_tokens` — they should be bursting into it |
| Under-weighted for their plan | Raise `weight`; it takes effect within 5 s |
| Runaway agents | Find them: `GET /v1/runs?state=RUNNING&tenant=acme`; cancel |
| Provider quota genuinely exhausted | Raise `--provider-tpm` only if the contract allows |

> **Do not** "fix" this by removing the limiter. Without it, this tenant would be
> starving everyone else instead.

---

## INC-6 — A run is stuck in RUNNING

```sql
SELECT id, state, lease_owner, lease_expires_at, updated_at, status_reason
FROM runs WHERE state='RUNNING' AND updated_at < now() - interval '5 minutes';
```

| `lease_owner` | `lease_expires_at` | Meaning | Action |
|---|---|---|---|
| set | in the future | A worker is genuinely working | Check for a long tool call |
| set | **past** | Worker died | Reaper should fix within one interval. Is it running? |
| **empty** | `NULL` | **Ownerless RUNNING** — the bug class | Reaper's 60 s orphan clause catches it; investigate why |

```bash
kubectl -n agentorch logs -l app.kubernetes.io/name=agentd | grep -i reaper
```

> This state should be impossible. It occurred once when the worker's cleanup
> path called a bare `ReleaseLease` (which nulls the lease but leaves
> `state='RUNNING'`), making the run invisible to **both** the dispatcher and the
> reaper. Fixed with a fenced `YieldRun` plus the orphan clause as belt and
> braces. See [Durable execution §5](../01-concepts/02-durable-execution.md).

---

## INC-7 — Spend anomaly

```promql
topk(5, increase(agentorch_model_cost_usd_total[1h]))
```

```bash
curl -s -H "Authorization: Bearer $OP_KEY" $API/v1/overview | jq '.by_tenant'
curl -s -H "Authorization: Bearer $OP_KEY" "$API/v1/runs?tenant=acme&limit=100" \
  | jq '[.runs[] | {id, agent_name, cost: .usage.cost_usd}] | sort_by(-.cost) | .[0:5]'
```

| Cause | Action |
|---|---|
| A few expensive runs | Check their budgets; lower `max_cost_usd` in the definition |
| Many cheap runs | Legitimate growth, or a loop across runs |
| One run at its cap | The budget worked. Was the cap right? |
| Context growth | Long conversations are quadratic — check tool result sizes |

**Prevention:** budgets are per-run. A *tenant-level* daily spend cap is a gap.

---

## INC-8 — Suspected sandbox escape 🔴

**Severity: page, escalate immediately.**

### Contain first

```bash
# 1. Freeze the tenant (prevent new runs)
psql -c "UPDATE tenants SET max_concurrent_runs = 0 WHERE id = 'acme';"

# 2. Cordon the sandbox node - do NOT drain yet, preserve state
kubectl cordon <node>

# 3. Snapshot the suspect pod before it is deleted
kubectl -n agentorch-sandboxes get pod <pod> -o yaml > /forensics/pod.yaml
kubectl -n agentorch-sandboxes logs <pod>                > /forensics/logs.txt
```

### Then establish scope

```bash
# Everything that agent did, with full attribution
curl -s -H "Authorization: Bearer $OP_KEY" "$API/v1/audit?tenant=acme&run_id=$RUN" | jq

# Every credential minted for it, from result_meta
… | jq '.records[] | select(.result_meta.credential_id) | {ts, tool, credential_ref, credential_id}'
```

### Why the blast radius is bounded

| Control | Effect |
|---|---|
| Credentials ≤60 s, revoked after use | A stolen one is almost certainly already dead |
| `automountServiceAccountToken: false` | No Kubernetes API identity on the pod |
| Tainted node pool | No control-plane credentials on that node |
| Empty netns | No exfiltration path from the agent sandbox |
| Per-tenant host uids | Other tenants' files unreadable at the filesystem layer |

### Recovery

1. Revoke every credential reference the tenant used (rotate at the provider)
2. Verify the audit chain for the tenant
3. `kubectl drain` the node and replace it
4. Root-cause: gVisor CVE? A gap in our seccomp list? A policy hole?
5. Add a regression test for the specific path

---

## Routine operations

### Rolling deploy

```bash
kubectl -n agentorch set image deployment/agentd agentd=agentorch/platform:v2
kubectl -n agentorch rollout status deployment/agentd
```

**No draining needed.** In-flight runs yield back to the queue; if a worker is
SIGKILLed the lease lapses and another picks it up. Verified: 12 runs survived a
hard generation swap with **zero duplicated side effects**.

### Node drain

```bash
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
```

Agent workers: safe. **Sandbox pods: their `emptyDir` workspace is lost** — the
run recovers but re-does file work. That is the missing workspace-checkpointing
gap.

### Scaling for a known burst

```bash
kubectl -n agentorch scale deployment/agentd --replicas=20
# raise the floor so the HPA does not scale back down mid-burst
kubectl -n agentorch patch hpa agentd --type=merge -p '{"spec":{"minReplicas":10}}'
```

### Emergency: stop all agent execution

```bash
kubectl -n agentorch scale deployment/agentd --replicas=0
```

Runs stay `QUEUED` and resume when workers return. **Nothing is lost.** This is
the cheapest big red button in the system, and it exists because state is not in
the workers.

---

## What to check after any incident

- [ ] Audit chain verifies for every affected tenant
- [ ] No runs stuck in `RUNNING` with an expired or absent lease
- [ ] No `tool_calls` left `IN_FLIGHT` beyond the reaper window
- [ ] `agentorch_audit_write_failures_total` is 0
- [ ] Queue depth returned to baseline
- [ ] Spend per tenant matches expectation
- [ ] A regression test exists for the specific failure

---

**Next:** [Capacity planning](05-capacity.md).

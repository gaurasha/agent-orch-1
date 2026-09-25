# Production hardening checklist — every "designed, not built" item in one list

> The documentation names many gaps, production steps and "would reverse if"
> conditions. This is the consolidated, ordered list, so that nobody has to
> collect them from thirty documents. Order is by risk reduced per unit of
> work. Each row links to where the reasoning lives.

| # | Item | Why it matters | Where it is argued | Effort | Status |
|---|---|---|---|---|---|
| 1 | **Workload identity for run tokens** — SPIFFE/SPIRE mTLS or per-service keys; today one HMAC secret proves which *run* a caller claims, not which *service* is calling | a compromised worker can mint a token for any run | [secrets §7](../01-concepts/05-secrets.md), [credentials](../reasoning/06-credentials.md) | M | gap #1 in README |
| 2 | **Egress proxy for the broker sandbox** with per-run domain allowlist; wire `NetworkProxy` to it | today the broker sandbox uses the host network namespace | [credential plane](../architecture/05-credential-plane.md), [isolation](../reasoning/02-isolation.md) | M | designed; NetworkPolicy already references it |
| 3 | **Audit write failure refuses the call** (fail closed) instead of counting | a call that ran without a record is the worst outcome for an audit system | [tool gateway](../architecture/03-tool-gateway.md) failure table | S | counter only |
| 4 | **Add human actions (resume/approve/cancel) to the audit chain** | they are in the event log but not the tamper-evident record | [audit](../reasoning/08-audit.md) | S | — |
| 5 | **Anchor chain heads externally** (WORM bucket / transparency log / customer store), per tenant per interval | binds the database owner, which the chain alone does not | [audit](../reasoning/08-audit.md) | S | — |
| 6 | **OIDC for callers**; remove the in-memory API-key map; operator role from a group claim | production identity | [control plane](../architecture/01-control-plane.md) | M | seam exists (`Principal`) |
| 7 | **Client idempotency key on `POST /v1/runs`** (`Idempotency-Key` header, unique index) | a retried create makes two runs | [control plane](../architecture/01-control-plane.md) | S | — |
| 8 | **Redis-backed fairness buckets with a local lease** | N worker replicas over-admit N× | [fairness](../reasoning/07-fairness.md) | M | per-process today |
| 9 | **Policy linter / admission for agent definitions** — reject `http.*` grants without `allowed_hosts`, wildcard suffixes, zero budgets | policy quality is the tenant author's burden today | [authoring guide](../06-guides/01-authoring-agent-definitions.md) | S | — |
| 10 | **Denied-connection probe in `kind-test.sh`** (a pod in the sandbox namespace must fail to reach the gateway) | the NetworkPolicy claim is verified by object presence, not behaviour | [Kubernetes topology](../architecture/08-network-topology.md) | S | — |
| 11 | **Checkpoint events** summarising older context; replay from the last checkpoint | replay and context cost grow with run length | [scheduling](../reasoning/04-scheduling.md) | M | designed |
| 12 | **Object-storage checkpoints of `/work`** at tool-call boundaries | a replacement worker on another node must see the same files; workspaces are not in backups | [state and failover](../architecture/07-state-failover.md) | M | designed |
| 13 | **Partition `events`/`audit_log` by month; tier cold partitions to Parquet** | unbounded growth on the primary | [state and failover](../architecture/07-state-failover.md) | M | designed |
| 14 | **LISTEN/NOTIFY or a read replica for the console** | SSE polling × consoles loads the primary | [control plane](../architecture/01-control-plane.md) | S | polling |
| 15 | **Managed HA Postgres with PITR**, connection pooling (PgBouncer), verified restores | single StatefulSet today | [upgrades and backup](06-upgrades-backup-and-rotation.md) | M | — |
| 16 | **Dual-key verification for `AGENTORCH_SECRET` rotation** (`kid`) | rotation causes ≤ 5 min of 401s today | [upgrades and backup §6](06-upgrades-backup-and-rotation.md) | S | — |
| 17 | **Real credential roots**: GitHub App installation tokens / STS / Vault behind `Mint`; KMS-held roots with versions | HMAC-derived tokens and in-memory roots are the PoC stand-in | [credentials](../reasoning/06-credentials.md) | M | interface exists |
| 18 | **Real model provider adapter behind `--provider`**, price-table cache columns, invoice reconciliation | fake provider only | [adding a provider](../06-guides/04-adding-a-model-provider.md) | M | — |
| 19 | **gVisor everywhere sandboxes run** (kind overlay currently disables the RuntimeClass) — or Kata/Firecracker where KVM exists | shared-kernel isolation in non-production is a documented downgrade | [isolation](../reasoning/02-isolation.md) | S/M | production manifests set it |
| 20 | **Warm per-run sandbox pods for interactive agents** under the Kubernetes driver | 1–3 s pod start per call | [sandbox](../architecture/04-sandbox.md) | M | per-call |
| 21 | **KEDA / queue-depth autoscaling** for workers (`runs{state=QUEUED}`) | CPU-based HPA lags bursts | [Kubernetes](../reasoning/11-kubernetes.md) | S | CPU HPA |
| 22 | **Kyverno/Gatekeeper policies**: every pod in `agentorch-sandboxes` must set `runtimeClassName: gvisor`; no pod may mount a ServiceAccount token there | PSA restricted does not express these | [Kubernetes](../reasoning/11-kubernetes.md) | S | — |
| 23 | **Secrets manager / External Secrets** for the two platform secrets; image signing (Sigstore) + admission verification; SBOM + `govulncheck` in CI | supply chain | [language and dependencies](../reasoning/12-language-and-dependencies.md) | S | — |
| 24 | **OpenTelemetry spans keyed by `run_id`**; GenAI semantic conventions | cross-process latency today is joined by hand from logs | [observability](../architecture/10-observability.md) | S | designed |
| 25 | **Alert rules** for `audit_write_failures_total > 0`, `rate(leases_reaped_total)`, `quota_denied_total` + `runs{state=QUEUED}` growth, `tool_calls_total{decision=DENY}` spikes | the runbook's triggers, as code | [runbook](04-runbook.md) | S | documented, not shipped |
| 26 | **Migration runner** with a version table and CI dry-run against a production copy | `schema.sql` at startup is additive-only by discipline | [upgrades §2](06-upgrades-backup-and-rotation.md) | S | — |
| 27 | **Workspace housekeeping sweep** for directories whose run is terminal | a crash between finish and removal leaks a directory | [upgrades §7](06-upgrades-backup-and-rotation.md) | S | — |
| 28 | **Chaos and soak tests**: kill the Postgres primary under load; 24 h soak for bloat/leaks; fuzz `checkParams` URL/host parsing | the failure modes not yet provoked | [evals and testing](../reasoning/13-evals-and-testing.md) | M | — |
| 29 | **Agent evaluation harness** on top of the API (task suites with programmatic checks, AgentDojo-style safety runs scored from the audit chain, CIs across seeds) | the platform makes agents safe to run, not good | [evals and testing](../reasoning/13-evals-and-testing.md) | M | recommended |
| 30 | **Second CLI tool generalisation** (`Backend.CredEnv`) and MCP client in the gateway | only `gh` today; MCP tool servers are the ecosystem | [adding a tool](../06-guides/03-adding-a-tool.md), [state of the art](../reasoning/15-state-of-the-art.md) | M | — |
| 31 | **Per-tenant sandbox concurrency cap** (or per-tenant node pools for regulated tenants) | the sandbox ResourceQuota is shared | [onboarding a tenant](../06-guides/06-onboarding-a-tenant.md) | S | shared ceiling |
| 32 | **Ageing for batch priority** or a dedicated batch pool, if batch SLAs become contractual | batch can be starved by sustained interactive load | [scheduling](../reasoning/04-scheduling.md) | S | policy |

Effort: S = a day or two, M = a week or two for one engineer. "Status"
describes the repository at the time of writing.

## Reading this list against the threat model

Items 1–5 close the residual risks the [threat model](../00-problem/04-threat-model.md)
ranks highest (token forgery by a compromised worker, broker-sandbox egress,
audit integrity). Items 6–10 are the multi-tenant production floor. Items
11–15 are the scale path from thousands to tens of thousands of tool calls
per minute. The rest are quality-of-operation. Nothing on this list changes a
decision in [reasoning](../reasoning/README.md); each is the "production step"
those documents already name.

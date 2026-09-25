# Limits and defaults — every constant, in one place

> Each value below is the number the running system uses, with where it is set
> and what changes it. Two kinds appear: **wired** values set in `serve.go` or
> a manifest (change by configuration or a one-line edit), and **package
> defaults** that apply when a caller leaves a field zero. Where the two differ
> the wired value is what production runs and is listed first.
>
> Related: [configuration](../02-architecture/04-configuration.md) (flags and env vars) · [capacity](../03-operations/05-capacity.md) (how to size them) · [errors and codes](06-errors-and-codes.md)

## 1. Scheduling and leases (`internal/runtime`)

| Constant | Value | Where | Why this value | Changing it |
|---|---|---|---|---|
| lease TTL | **30 s** | `runtime.Config.LeaseTTL` default (`worker.go`) | recovery bound after a worker dies; long enough that a slow step + one missed heartbeat does not fence a live worker | shorter = faster recovery, more false fencing; keep ≥ 3× heartbeat |
| heartbeat interval | **TTL / 3 = 10 s** | `HeartbeatInterval` default | two missed renewals before expiry | derived; set explicitly if TTL changes |
| steps per lease | **4** | `MaxStepsPerLease` default | a long run yields so the pool can rebalance; 4 extra commits per ~16 steps | raise for cheap steps, lower for fairness under contention |
| dispatcher poll | **100 ms** | `PollInterval` default | latency floor for a run's first step; negligible idle load | LISTEN/NOTIFY is the designed replacement |
| typical call estimate | **2 000 tokens** | `TypicalTokens` default | what `Eligible()` asks the limiter a tenant can afford | tune to the fleet's median `EstimateTokens` |
| reaper interval | **3 s** wired (`serve.go`); package default 5 s | `NewReaper(store, 3*time.Second, 2*time.Minute, …)` | recovery bound ≈ TTL + interval | operational preference |
| stuck tool call (`stuck_after`) | **2 min** wired; package default 5 min | same call | must exceed the longest tool timeout (30 s) with margin, or live calls are marked "unknown" | raise if a tool's `Timeout` grows |
| ownerless-RUNNING repair | **60 s** | SQL in `ReapExpiredLeases` | belt and braces for a bug that leaves `RUNNING` with `lease_owner NULL` (B6) | — |
| quota-wait requeue | **+2 s** | `worker.go` (`ErrQuotaUnavailable`) | a bucket refills continuously; 2 s is a cheap retry | — |
| provider-outage requeue | **+10 s** | `worker.go` | after 3 failed attempts; avoids hammering a degraded provider | — |
| tenant-set refresh | **5 s** | `serve.go` goroutine → `limiter.SetTenants` | a new tenant gets its share without a restart | — |
| worker goroutines per process | **4** | `--workers` / `AGENTORCH_WORKERS` | I/O-bound; total = replicas × workers | see capacity §3 |
| durability test overrides | TTL 2 s, heartbeat 500 ms, 8 steps, poll 50 ms | `test/durability` | make the failure scenarios fast | tests only |
| load test overrides | TTL 15 s, heartbeat 5 s, 8 steps, poll 5 ms | `test/load` | | tests only |

## 2. Model plane (`internal/llm`, `internal/fairness`)

| Constant | Value | Where | Notes |
|---|---|---|---|
| attempts per model call | **3** | `Gateway.maxAttempts` | then error → worker requeues +10 s |
| backoff base | **250 ms**, ×2 per attempt, **full jitter** (`rand(0, backoff)`) | `Gateway.baseBackoff` | attempt 2 waits ≤ 250 ms, attempt 3 ≤ 500 ms |
| retryable errors | `ErrRateLimited`, `ErrOverloaded` | `isRetryable` | everything else fails the call immediately |
| fallback provider | on the **last** attempt only, if configured and retryable | `Gateway.WithFallback` | none configured in the PoC |
| max output tokens per call | **2 048** | `worker.go` `llm.Request.MaxTokens` | bounds a single call's cost and the estimate error |
| minimum estimate | **64 tokens** | `EstimateTokens` | never reserve zero |
| provider rate | **400 000 tokens/min** default | `--provider-tpm` / `AGENTORCH_PROVIDER_TPM` | set slightly **below** the contractual limit |
| burst window | **10 s** (`capacity = rate × 10`) | `fairness.Config.BurstSeconds` (wired 10; default 10) | how far a tenant may burst after idling |
| interactive reserve | **20 %** of each bucket | `InteractiveReservePct` (wired 20; default 20) | batch/normal cannot draw below it |
| spare bucket | rate = provider − Σ shares; starts **empty** | `SetTenants` | only plan-capped tenants leave spare |
| price table | claude-opus-5 $15/$75 · claude-sonnet-5 $3/$15 · claude-haiku-4-5 $0.80/$4 · fake-small $0.25/$1.25 · fake-large $3/$15 per Mtok in/out; **unknown model priced at the most expensive rate** | `llm.priceTable`, `Price()` | under-reporting spend is the failure that hurts |
| fake provider latency | **300 ms** ± jitter (latency/2) | `--model-latency` | tests use 2 ms |

## 3. Tool gateway and credentials (`internal/gateway`, `internal/creds`, `internal/tools`)

| Constant | Value | Where | Notes |
|---|---|---|---|
| run token lifetime | **5 min** | `toolcaller.go` (`tokenTTL`) | signed per call by the worker; HS256; `aud=tool-gateway` |
| worker → gateway HTTP timeout | **120 s** | `HTTPToolCaller` client | must exceed the longest tool timeout |
| credential TTL | **60 s** per mint; hard cap **10 min** | `invoke.go` (60 s) · `creds.maxTTL` | revoked in a `defer` after every call |
| tool timeouts | `exec.bash` 30 s · `exec.python` 30 s · `doc.convert` 30 s · `fs.*` 5 s · `http.get`/`http.post` 20 s · `github.cli` 20 s | `tools.DefaultRegistry` | per tool `Backend.Timeout`; becomes the sandbox wall-clock |
| audit argument cap | **2 000 bytes** per string argument, then `…[truncated, N bytes total]` | `gateway.redactArgs` | keys containing `token`, `secret`, `password`, `authorization`, `api_key`, `apikey` are stored as `[REDACTED]` |
| result to the model | capped by the sandbox output limit (below); `Truncated=true` when hit | `sandbox.Limits.MaxOutputBytes` | |
| idempotency key | `run:step:i` from the worker; fallback `run:step:sha256(args)[:16]` | `gateway.process` | deterministic across replays |
| audit timestamp precision | **microseconds** (truncated before hashing) | `AppendAudit` | Postgres `timestamptz` resolution (B5) |

## 4. Sandbox limits (`sandbox.DefaultLimits`, all drivers)

| Limit | Default | Enforced by | Overridable per tool? |
|---|---|---|---|
| memory | **256 MiB** (swap = memory, i.e. none) | cgroup `memory.max` / `--memory` / pod limits | via `Spec.Limits` |
| CPU | **500 m** (half a core) | cgroup `cpu.max` / `--cpus` / pod limits | yes |
| processes | **64** pids | `pids.max` / `--pids-limit` | yes |
| wall clock | **30 s** | parent timer → `killCgroup`; k8s `activeDeadlineSeconds = wall + 5 s` | yes (`Backend.Timeout`) |
| combined output | **256 KiB** (128 KiB stdout + 128 KiB stderr) | `capWriter` | yes |
| workspace size | **64 MiB** | tmpfs size (docker) / quota (designed for namespace) | yes |
| `/tmp` size | **16 MiB** | tmpfs `size=` | yes |
| open files | **256** | `RLIMIT_NOFILE` | yes |
| file size | **64 MiB** | `RLIMIT_FSIZE` | yes |
| core dumps | **0** | `RLIMIT_CORE` | no |
| user inside / outside | uid **1000** → host uid **100 999** (map `0→0 ×1`, `1→100000 ×65535`) | user namespace | no |
| hostname | `sandbox` | UTS namespace | no |
| PATH | `/opt/agentorch/bin:/usr/local/bin:/usr/bin:/bin`; `HOME=/work` | `sandbox.SafeEnv` | tools may add variables |
| seccomp denylist | **35** syscalls → `EPERM` + `clone3` → `ENOSYS` (36) | `seccomp_linux.go` | no ([syscalls](02-syscalls.md)) |
| setup-failure exit code | **125** + stderr marker `agentorch-sandbox-setup-error:` | `jail_linux.go` | no |

## 5. Store and API (`internal/store`, `internal/api`)

| Constant | Value | Where |
|---|---|---|
| Postgres pool | **20** max connections per process | `postgres.go` (`maxConns`) |
| `ListRuns` limit | default **200**, maximum **1 000** (clamped, never reset — B7) | `postgres.go` |
| `ListAudit` limit | default **500**, maximum **5 000** | `postgres.go` |
| SSE poll | **400 ms** | `api.streamRun` |
| admission control | `active_runs ≥ tenants.max_concurrent_runs` → 429 with `retry_after_seconds: 30` | `api.createRun` |
| default budget (`types.DefaultBudget`) | 25 steps · 50 tool calls · 200 000 tokens · $5.00 · 3 600 s | used by seeded agents that do not set one |
| health check | `GET /healthz` → 503 if the store does not answer | `api.handleHealth` |

## 6. Kubernetes (`deploy/k8s/base`)

| Object | Value |
|---|---|
| controlplane | 2 replicas · PDB `minAvailable: 1` · `:8080` |
| toolgateway | 2 replicas · PDB `minAvailable: 1` · `:8081` |
| agentd | 3 replicas · HPA **2–40** on CPU 70 % · scale-up 100 %/30 s (stabilisation 30 s) · scale-down 25 %/60 s (stabilisation 300 s) |
| postgres | StatefulSet ×1 · `:5432` |
| sandbox ResourceQuota `sandbox-ceiling` | pods **500** · cpu **200** (requests = limits) · memory **400 Gi** · ephemeral-storage requests **500 Gi** |
| sandbox LimitRange `sandbox-limits` | default & request `cpu 500m, memory 256Mi, ephemeral-storage 1Gi`; max `cpu 4, memory 8Gi, ephemeral-storage 20Gi` |
| RuntimeClass | `gvisor` (handler `runsc`), nodeSelector `agentorch.io/workload=sandbox`, toleration for its `NoSchedule` taint |
| NetworkPolicy ports | postgres 5432 · toolgateway 8081 · egress-proxy 3128 · kube-dns 53 · gateway egress 443 + 6443 except `169.254.0.0/16` |
| k8s client timeout | 30 s per API call | `internal/k8s` |
| sandbox pod | `restartPolicy: Never` · `automountServiceAccountToken: false` · `activeDeadlineSeconds: wall+5` · labels carry tenant and run |

## 7. Seeded demo data (`cmd/agentorch/seed.go`)

| Tenant | Weight | tokens/min | max concurrent runs | API key → user |
|---|---|---|---|---|
| `acme` | 2 | 300 000 | 500 | `acme-key` → `alice@acme.example` |
| `globex` | 1 | 150 000 | 300 | `globex-key` → `bob@globex.example` |
| (operator) | — | — | — | `demo-operator-key` → reads all tenants |

Each tenant gets two credential roots: `github/token` and `http/default`
(random 32-byte HMAC keys, in memory). Six agent definitions are seeded; see
[authoring agent definitions](../06-guides/01-authoring-agent-definitions.md) for
each one's policy.

## 8. Diagram and documentation tooling

| Item | Value |
|---|---|
| `svgkit` label width estimate | 6.0 px/char at 10.5 px; box text ≈ 5.3 px/char |
| link checker | `python3 docs/tools/check-links.py [--external]`; 16 parallel fetches, 25 s timeout |

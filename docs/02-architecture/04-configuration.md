# Configuration reference

> **Prerequisite:** [Components](01-components.md)
> **Read next:** [Code map](05-code-map.md)
> **Source:** [`cmd/agentorch/serve.go`](../../backend/cmd/agentorch/serve.go)

Every flag has an environment-variable equivalent (`--sandbox-driver` ⟷
`AGENTORCH_SANDBOX_DRIVER`), so the same binary is configured by flags locally
and by a `ConfigMap` in Kubernetes. Flags win over environment.

---

## All flags

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--role` | `AGENTORCH_ROLE` | `all` | `all` · `controlplane` · `worker` · `gateway` |
| `--dsn` | `AGENTORCH_DSN` | *(empty)* | Postgres DSN. **Empty ⟹ in-memory store, no durability** |
| `--addr` | `AGENTORCH_ADDR` | `:8080` | Control plane listen address |
| `--gateway-addr` | `AGENTORCH_GATEWAY_ADDR` | `:8081` | Tool gateway listen address |
| `--gateway-url` | `AGENTORCH_GATEWAY_URL` | *(empty)* | Gateway URL for workers. Empty ⟹ call in-process |
| `--github-api` | `AGENTORCH_GITHUB_API` | *(empty)* | GitHub API base. Empty ⟹ start the built-in fake |
| `--workspace-dir` | `AGENTORCH_WORKSPACE_DIR` | `/var/lib/agentorch/workspaces` | Run workspace root |
| `--sandbox-dir` | `AGENTORCH_SANDBOX_DIR` | `/var/lib/agentorch/sandboxes` | Sandbox scratch root |
| `--sandbox-driver` | `AGENTORCH_SANDBOX_DRIVER` | `namespace` | `namespace` · `docker` · `kubernetes` |
| `--sandbox-image` | `AGENTORCH_SANDBOX_IMAGE` | `agentorch/sandbox:dev` | Image for docker/kubernetes drivers |
| `--runtime-class` | `AGENTORCH_RUNTIME_CLASS` | *(empty)* | **`gvisor` in production.** Empty ⟹ default runtime |
| `--workers` | `AGENTORCH_WORKERS` | `4` | Worker goroutines per process |
| `--provider-tpm` | `AGENTORCH_PROVIDER_TPM` | `400000` | Total LLM tokens/minute across all tenants |
| `--log-level` | `AGENTORCH_LOG_LEVEL` | `info` | `debug` · `info` · `warn` · `error` |
| `--secret` | `AGENTORCH_SECRET` | *(empty)* | HMAC key for run tokens. **Must be shared across replicas** |
| `--ui-dir` | `AGENTORCH_UI_DIR` | *(empty)* | Directory of built console assets to serve at `/` |
| `--model-latency` | — | `300ms` | Simulated LLM latency (fake provider) |
| `--model-fail-every` | — | `0` | Fail every Nth model call, to exercise backoff |
| `--seed` | — | `true` | Seed demo tenants and agents on startup |

---

## The four settings that actually matter

### `--dsn` — durability

```go
if o.DSN == "" {
    log.Warn("no --dsn given: using the in-memory store. " +
        "Runs will NOT survive a restart, which defeats the durability guarantee. " +
        "Use Postgres for anything you intend to believe.")
}
```

The warning is loud because a silent fallback would make the durability
demonstration meaningless. The in-memory store exists so unit tests run with no
infrastructure, not as a deployment mode.

### `--secret` — run token signing

**Every replica must share this value.** If they do not, workers mint tokens the
gateway rejects and every tool call fails with `401`.

```go
if len(secret) == 0 {
    secret = []byte(randomSecret())
    if o.Role != "all" {
        log.Error("AGENTORCH_SECRET is not set. Each replica will generate a " +
            "different signing key and reject the others' run tokens. Set it explicitly.")
    }
}
```

Generating one is fine for a single process and catastrophic across replicas, so
the error fires only when the role implies a multi-process deployment.

> **Production:** rotate regularly, source from a secret manager, and replace the
> whole mechanism with SPIFFE/SPIRE mTLS plus asymmetric run tokens —
> [Secrets §7](../01-concepts/05-secrets.md#7-workload-identity--the-gap).

### `--runtime-class` — the strongest isolation layer

Empty means the default runtime, i.e. **a shared host kernel**. `gvisor` means
the syscall boundary moves into user space.

If the RuntimeClass is not installed, sandbox pods stay `Pending` with a clear
message. That is correct: failing to start a sandbox is safe, silently falling
back to `runc` is not.

### `--sandbox-driver` — which boundary you actually get

| Driver | Requires | Boundary | Cold start |
|---|---|---|---|
| `namespace` | Linux + root/`CAP_SYS_ADMIN` | namespaces + cgroups + seccomp | **8.4 ms** |
| `docker` | a Docker/Podman daemon | container (optionally `runsc`) | ~150–400 ms |
| `kubernetes` | in-cluster SA | Pod + RuntimeClass + NetworkPolicy | ~1–3 s |

`Result.Driver` records which one ran, so the audit log and the UI never imply a
stronger boundary than was used.

---

## Deployment topologies

### Single process (demo, CI)

```bash
agentorch serve --role=all --dsn "$DSN" --workers 4 --ui-dir ui/dist
```

Everything in one process; the worker calls the gateway **in-process** through
the real HTTP handler, so authentication and decoding are exercised identically
to the deployed path — only the transport differs.

### Split (Kubernetes)

```bash
agentorch serve --role=controlplane                       # Deployment, 2 replicas
agentorch serve --role=gateway                            # Deployment, 2 replicas
agentorch serve --role=worker --workers=8 \
  --gateway-url http://toolgateway:8081                   # Deployment, HPA 2–40
```

Setting `--gateway-url` switches the worker from `LocalToolCaller` to
`HTTPToolCaller`. **The policy, credential and audit code is byte-for-byte the
same** — so a property demonstrated locally is the same property in production.

---

## Tuning

### Worker concurrency

Total goroutines = `replicas × --workers`. Because the work is I/O-bound, this is
a throughput dial and is **unrelated to the number of agents**.

| Concurrent agents | Suggested total | Rationale |
|---|---|---|
| 100 | 8 | measured p99 ≈ 1 s at 500/16 |
| 1,000 | 24–32 | ~100 turns/s ≈ 0.3 cores of real work |
| 10,000 | 200+ | shard the lease query by tenant first |

### Runtime constants

These are in [`runtime.Config`](../../backend/internal/runtime/worker.go) rather
than flags, because changing them changes correctness properties and should be a
code review:

| Constant | Default | Trade-off |
|---|---|---|
| `LeaseTTL` | 30 s | Shorter ⟹ faster recovery, higher risk of fencing a merely-slow worker |
| `HeartbeatInterval` | `LeaseTTL/3` | Must stay comfortably below the TTL — 3 chances before expiry |
| `MaxStepsPerLease` | 4 | Higher ⟹ less lease churn; lower ⟹ fairer rebalancing across workers |
| `PollInterval` | 100 ms | Lower ⟹ snappier pickup, more database load |
| Reaper `interval` | 3 s | How fast a dead worker's run is reclaimed |
| Reaper `stuckAfter` | 2 min | **Must exceed the longest tool timeout**, or live calls are declared abandoned |

### Sandbox limits

[`sandbox.DefaultLimits()`](../../backend/internal/sandbox/sandbox.go):

```go
MemoryBytes:     256 << 20,        // 256 MiB
CPUMillis:       500,              // half a core
PIDs:            64,
Wall:            30 * time.Second,
MaxOutputBytes:  256 << 10,        // 256 KiB combined
WorkspaceBytes:  64 << 20,
TmpfsBytes:      16 << 20,
MaxOpenFiles:    256,
MaxFileSizeByte: 64 << 20,
```

**Zero never means unlimited.** `WithDefaults()` fills any zero field, because an
omitted limit must not become a production incident:

```go
func (l Limits) WithDefaults() Limits {
    d := DefaultLimits()
    if l.MemoryBytes <= 0 { l.MemoryBytes = d.MemoryBytes }
    …
}
```

The same principle applies to [`types.DefaultBudget()`](../../backend/internal/types/types.go):
25 steps / 50 tool calls / 200k tokens / $5 / 1 hour.

---

## Kubernetes configuration

```yaml
# ConfigMap agentorch-config
AGENTORCH_LOG_LEVEL:        "info"
AGENTORCH_SANDBOX_DRIVER:   "kubernetes"
AGENTORCH_SANDBOX_IMAGE:    "agentorch/sandbox:dev"
AGENTORCH_RUNTIME_CLASS:    "gvisor"
AGENTORCH_PROVIDER_TPM:     "400000"
AGENTORCH_GATEWAY_URL:      "http://toolgateway:8081"
```

Secrets come from `Secret` objects and are interpolated into the DSN:

```yaml
env:
  - name: AGENTORCH_DSN
    value: postgres://agentorch:$(POSTGRES_PASSWORD)@postgres:5432/agentorch?sslmode=disable
  - name: POSTGRES_PASSWORD
    valueFrom: {secretKeyRef: {name: agentorch-postgres, key: POSTGRES_PASSWORD}}
```

> **Production:** `sslmode=disable` is wrong. Use `verify-full` with the CA
> mounted. The demo value is in-cluster only.

### The local overlay's deliberate downgrade

```yaml
# SECURITY DOWNGRADE, DELIBERATE AND LOCAL ONLY
- op: replace
  path: /data/AGENTORCH_RUNTIME_CLASS
  value: ""
```

kind has no gVisor shim, so the alternative is every sandbox pod stuck
`Pending`. Everything else still applies — no network, read-only rootfs,
non-root, caps dropped, seccomp, limits — but the top layer is gone, and **the
comment lives in the file that removes it**.

---

## Setting a sane provider quota

`--provider-tpm` should be set **slightly below** your real contractual limit.
Two reasons:

1. Our 429 is graceful — the run stays `QUEUED` and retries with backoff. The
   provider's 429 costs a round trip and may carry a penalty.
2. It leaves headroom for retries and for estimate error.

A reasonable starting point is 85–90% of the contract.

---

**Next:** [Code map](05-code-map.md) — a guided tour of the source.

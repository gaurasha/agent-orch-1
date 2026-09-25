# Code map

> **Prerequisite:** [Components](01-components.md)
> **Read next:** [Running locally](../03-operations/01-running.md)

A guided tour: what is where, what to read first, and which files carry the most
design weight.

---

## Layout

```
backend/
  cmd/agentorch/
    main.go          personality dispatch (busybox pattern)
    serve.go         wiring: Build() constructs the whole object graph
    demo.go          the scripted end-to-end acceptance test
    seed.go          demo tenants, credentials, agent definitions
    gh.go            the `gh` personality — runs INSIDE the broker sandbox
    convert.go       the `aoconvert` personality — runs INSIDE the agent sandbox

  internal/
    types/           the domain model. Read this first
    store/           durability: Postgres + memory + ONE conformance suite
    runtime/         the stateless worker, replay, the reaper
    gateway/         THE choke point: authz + credentials + audit
    authz/           policy engine, fails closed
    creds/           the Secret type; the broker
    tools/           registry (model schema vs backend config), invoker, workspace
    sandbox/         namespaces, cgroups, seccomp + docker & k8s drivers
    fairness/        weighted max-min LLM quota
    llm/             provider interface, scripted provider, model gateway
    api/             control plane, tenancy-scoped, SSE
    obs/             structured logs + hand-rolled Prometheus exposition
    k8s/             ~150-line REST client (no client-go)
    canon/           canonical JSON for content addressing and hashing
    jwtmini/         ~60-line HS256 JWT
    id/              sortable identifiers
    fakegithub/      a third party that actually verifies credentials
    testsupport/     per-package Postgres schema isolation

  test/
    durability/      kill a worker mid-tool-call
    load/            500 concurrent agents

ui/src/              React 19 + TypeScript console (~890 lines)
deploy/              docker · compose · kind · k8s{base,overlays/local} · argocd
scripts/             preflight · kind-up/test · argocd-up/test
docs/                this documentation set
```

---

## Reading paths

### "I have 15 minutes"

| # | File | Why |
|---|---|---|
| 1 | [`internal/types/types.go`](../../backend/internal/types/types.go) | The domain model, and the package comment states the central design claim |
| 2 | [`internal/sandbox/init_linux.go`](../../backend/internal/sandbox/init_linux.go) | The entire isolation argument in one file |
| 3 | [`internal/gateway/gateway.go`](../../backend/internal/gateway/gateway.go) | The choke point: `process()` is the whole security path |

### "I want to understand durability"

```
types/types.go              Run, Event, ToolCallRecord — note what is ABSENT from Run
store/store.go              the contract: AcquireLease, Commit, YieldRun, BeginToolCall
store/postgres.go           FOR UPDATE SKIP LOCKED; the fencing checks in Commit
runtime/worker.go           the loop; heartbeat; the yield path
runtime/context.go          Rebuild() — the pure function from log to context
runtime/reaper.go           the entire failure detector, two SQL statements
test/durability/            the tests that SIGKILL a worker mid-call
```

### "I want to understand the security model"

```
sandbox/jail_linux.go       parent: namespaces, cgroups, the release handshake
sandbox/init_linux.go       child: rootfs, pivot_root, THE PRIVILEGE-DROP ORDER
sandbox/seccomp_linux.go    the hand-written BPF filter, with reasons per syscall
authz/authz.go              the decision order; fail-closed; SSRF checks
gateway/gateway.go          decide → journal → audit → mint → execute → scrub
creds/secret.go             the type that refuses to print itself
tools/invoke.go             invokeCLI() — the two-sandbox credential pattern
```

### "I want to understand scaling"

```
fairness/limiter.go         weighted max-min; reserve-then-settle
llm/gateway.go              quota + bounded retries + full jitter
store/postgres.go           the dispatch query and its partial index
test/load/                  500 agents; the worker-scaling assertion
```

---

## The files that carry the most weight

### `internal/sandbox/init_linux.go`

The single most important file. Builds the jail and execs the payload, and the
comment above the privilege-drop sequence explains an ordering bug that surfaced
as an opaque `SIGABRT` inside glibc:

> *"Each step removes a privilege that the NEXT step needs. Getting it wrong
> does not fail open, but it does fail confusingly."*

### `internal/gateway/gateway.go`

Every tool call passes through `process()`. The security argument is that there
is exactly **one** such place, so authorization, credential minting and audit
cannot drift apart — and reviewing them means reading one file.

### `internal/store/postgres.go`

`AcquireLease` (the scheduling model) and `Commit` (the fencing token). The
comments cite the reasoning, including the `pq.Array(nil)` → SQL `NULL` bug.

### `internal/creds/secret.go`

Small, and it converts a discipline problem into a type problem.

### `internal/tools/registry.go`

The split that makes "credentials never enter the model context" structural:

```go
// MarshalJSON emits only the model-facing schema.
//
// This is the load-bearing line of the package: any code path that serialises
// a Tool - building an LLM request, rendering the UI, writing an event - gets
// the schema and cannot get the backend, even by mistake.
func (t Tool) MarshalJSON() ([]byte, error) { return json.Marshal(t.Schema) }
```

---

## Conventions

**Comments say *why*, not *what*.** `// increment i` is absent; `// Postgres
timestamptz has microsecond resolution…` is present.

**Errors are wrapped with context** and compared with `errors.Is`:

```go
return fmt.Errorf("commit rejected for worker %s (owner=%q): %w",
    worker, owner.String, types.ErrLeaseLost)
```

**Fail closed.** Unknown tool → deny. Unknown tenant in the limiter → deny.
Unparsable URL → deny. No path turns an error into an allow.

**Interfaces at the boundaries**, concrete types inside: `store.Store`,
`sandbox.Driver`, `creds.Broker`, `llm.Provider`, `runtime.ToolCaller`.

**Dependency injection through `Build()`** — the whole object graph is
constructed in one function, so tests can construct the same graph with a fake
clock, an in-memory store or a stub provider.

---

## Dependencies

**One:** `github.com/lib/pq`.

Hand-rolled instead: the HTTP router (stdlib `ServeMux` has method and wildcard
patterns since Go 1.22), JWT (~60 lines), Prometheus exposition (~150 lines),
the Kubernetes client (~150 lines), UUIDs, and the entire sandbox.

This is a deliberate trade with a real cost — `client_golang` handles exposition
edge cases I have not thought about; `libseccomp` has a better BPF compiler. The
argument for taking it anyway is that the component executing untrusted code is
where supply-chain risk is least acceptable, and `client-go` alone pulls in ~200
modules. One dependency means `go.sum` is reviewable in a minute.

It flips the moment we need informers, leader election, CRD codegen, native
histograms, or a real policy language. Argued in
[DEEP_DIVE D16](../../DEEP_DIVE.md#d16--dependency-policy).

---

## Testing conventions

| Suite | Location | Needs |
|---|---|---|
| Store conformance | `internal/store/conformance_test.go` | runs against **both** stores |
| Sandbox safety | `internal/sandbox/sandbox_linux_test.go` | Linux + root |
| Secret redaction | `internal/creds/secret_test.go` | nothing |
| Fairness | `internal/fairness/limiter_test.go` | nothing |
| Durability | `test/durability/` | optional Postgres |
| Load | `test/load/` | optional Postgres |

Three properties every test here tries to have:

1. **A negative control where one is possible.** The network test skips if the
   *host* has no network, because otherwise it proves only that this machine is
   offline.
2. **Assert the mechanism, not the outcome.** The fork bomb asserts the refusal
   happened at ≈`pids.max`, not merely that the test finished.
3. **Run against the real thing.** The conformance suite exists because a test
   that passes on a fake while the real store is broken is worse than no test.

### Sandbox tests need `TestMain`

```go
func TestMain(m *testing.M) {
    if sandbox.MaybeRunInit() { return }   // /proc/self/exe IS the test binary
    os.Exit(m.Run())
}
```

Without this, the re-exec'd child would run the whole test suite inside the
namespace.

---

## Adding things

### A new tool

1. Add a `tools.Tool` to `DefaultRegistry` — `Schema` for the model, `Backend`
   for the gateway.
2. If it needs a new `Kind`, add a case to `Invoker.Invoke`.
3. Decide `Dangerous` (⟹ human approval) and `UnsafeRetry` (⟹ the agent is told
   an ambiguous failure may have taken effect).
4. Add `ParamPolicy` defaults to any seeded agent that should be constrained.
5. **Write a test for what the tool must *not* do.**

### A new sandbox driver

Implement `sandbox.Driver`, add it to `sandbox.New`, and **add a row to the
[parity table](../01-concepts/01-linux-isolation.md#12-driver-parity-table)** —
an undocumented driver is a driver whose boundary nobody knows.

### A new store method

Take a tenant ID, or be explicitly operator-scoped. Add it to the conformance
suite, which runs it against both implementations.

---

**Next:** [Running locally](../03-operations/01-running.md).

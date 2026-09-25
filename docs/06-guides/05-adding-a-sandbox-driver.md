# Adding a sandbox driver

> A driver turns one `Spec` into one `Result` under the platform's isolation
> contract. The gateway never knows which driver it has; the safety suite is
> the definition of "good enough". This guide is the contract, the obligations,
> and how to prove a new driver (say, Firecracker via Kata, or a remote sandbox
> vendor) meets them.
>
> Code: [`backend/internal/sandbox/sandbox.go`](../../backend/internal/sandbox/sandbox.go) (`Spec`, `Limits`, `Result`, `Driver`, `New`), [`driver_docker.go`](../../backend/internal/sandbox/driver_docker.go), [`driver_k8s.go`](../../backend/internal/sandbox/driver_k8s.go), [`jail_linux.go`](../../backend/internal/sandbox/jail_linux.go) · Related: [sandbox](../architecture/04-sandbox.md) · [isolation reasoning](../reasoning/02-isolation.md)

## 1. The contract

```go
type Driver interface {
    Name() string                                       // appears in Result.Driver, audit meta and sandbox_runs_total{driver}
    Run(ctx context.Context, spec Spec) (Result, error) // one execution; error ONLY for the platform's own failure
    Close() error
}

type Spec struct {
    RunID, TenantID string            // for naming, labels and forensics
    Argv            []string          // the payload; Argv[0] resolved on the sandbox PATH
    Env             []string          // from sandbox.SafeEnv(...) — PATH, HOME=/work, LANG, plus tool extras
    WorkspaceDir    string            // host path to mount read-write at /work
    Network         NetworkMode       // NetworkNone (agent) | NetworkProxy (broker: egress via proxy only)
    ExtraReadOnly   map[string]string // guest path → host path, read-only (the platform binary as gh / aoconvert)
    Limits          Limits            // MemoryBytes, CPUMillis, PIDs, Wall, MaxOutputBytes, WorkspaceBytes, TmpfsBytes, MaxOpenFiles, MaxFileSizeByte
}

type Result struct {
    ExitCode              int
    Stdout, Stderr        string      // capped at MaxOutputBytes/2 each
    Truncated, OOMKilled, TimedOut bool
    Duration              time.Duration
    Driver                string
}
```

`sandbox.New(name, cfg)` is the registry: add a `case "firecracker":` returning
your constructor; `--sandbox-driver`/`AGENTORCH_SANDBOX_DRIVER` selects it.
Return `ErrUnsupported` from the constructor when the environment cannot run
it (no `/dev/kvm`, no vendor key) so startup fails loudly.

## 2. Obligations — what the safety suite will check

| # | Obligation | Test that fails otherwise |
|---|---|---|
| 1 | `NetworkNone` ⇒ **no route at all**: `connect()` fails before a packet exists (not a firewall that drops) | `TestSandbox_HasNoNetworkAccess` (asserts `ENETUNREACH` and a host control that succeeds) |
| 2 | the host filesystem is invisible; only the image's read-only tree, `/work` (rw), `/tmp` (rw), a minimal `/etc` and `/dev` exist | `TestSandbox_HostFilesystemIsNotVisible`, `_RootFilesystemIsReadOnly`, `_WorkspaceIsTheOnlyThingThatPersists` |
| 3 | the payload runs as a non-root uid with an empty capability bounding set and `no_new_privs` | `TestSandbox_RunsUnprivilegedWithNoCapabilities` |
| 4 | the escape-primitive syscalls fail (`mount`, `ptrace`, `unshare`, `setns`, `open_by_handle_at`, `bpf`, `keyctl`, `kexec_*`, `io_uring_*` …; `clone3` → `ENOSYS` or blocked) | `TestSandbox_EscapeSyscallsAreBlocked` |
| 5 | `Limits.Wall` kills **everything** the payload started, and `TimedOut=true` | `TestSandbox_WallClockTimeoutIsEnforced` |
| 6 | `Limits.MemoryBytes` is enforced and reported as `OOMKilled=true` | `TestSandbox_MemoryLimitIsEnforced` |
| 7 | `Limits.PIDs` is enforced | `TestSandbox_ForkBombIsContained` |
| 8 | output is capped at `MaxOutputBytes` (split stdout/stderr) with `Truncated=true`, not buffered unboundedly | `TestSandbox_OutputIsCapped` |
| 9 | cold start is measured and reported | `TestSandbox_ColdStartIsMeasured` — and the number decides whether per-call sandboxes remain the right lifecycle ([capacity §4](../03-operations/05-capacity.md)) |
| 10 | `ExtraReadOnly` binaries are present at their guest paths, read-only, and first on `PATH` (`/opt/agentorch/bin`) | the credential-boundary demo assertions (`gh` in the broker sandbox) |
| 11 | the platform's own failure to set up the sandbox is returned as `ErrSetup` (exit 125 + marker for process drivers; a pod phase like `ImagePullBackOff` for Kubernetes), **never** as the payload's failure | `exitcode.go` classification; the gateway maps it to 502 "platform failed" |
| 12 | no credential reaches the agent's environment, files or `/proc`; the broker sandbox is a **separate** instance with its own PID and user namespace (or VM) | `TestSandbox_HostFilesystemIsNotVisible` + demo assertion 4 |

The suite constructs the namespace driver directly (`newDriver` in
`sandbox_linux_test.go`). To run it against a new driver, parametrise that
helper on `AGENTORCH_SANDBOX_DRIVER` (a five-line change) so the same
assertions run unchanged. A driver that cannot pass a row must say so in its
`Name()` documentation and in the [isolation](../reasoning/02-isolation.md)
options table — "labelled as weaker" is acceptable for a development driver;
silent is not.

## 3. Mapping the limits

| `Limits` field | namespace | docker | kubernetes | a microVM driver would use |
|---|---|---|---|---|
| `MemoryBytes` | cgroup `memory.max` | `--memory` = `--memory-swap` | `resources.limits.memory` | guest RAM |
| `CPUMillis` | cgroup `cpu.max` | `--cpus` | `resources.limits.cpu` | vCPU quota |
| `PIDs` | `pids.max` | `--pids-limit` | (LimitRange / container runtime) | guest `pids.max` |
| `Wall` | parent timer → `killCgroup` | `context` timeout → `docker kill` | `activeDeadlineSeconds = wall + 5` | VM shutdown timer |
| `MaxOutputBytes` | `capWriter` | `capWriter` | log read cap | vsock stream cap |
| `WorkspaceBytes`, `TmpfsBytes` | tmpfs `size=` (`/tmp`), quota (designed) for `/work` | `--tmpfs size=` | ephemeral-storage | virtio-fs / block size |
| `MaxOpenFiles`, `MaxFileSizeByte` | `RLIMIT_NOFILE`, `RLIMIT_FSIZE` | inherited defaults | inherited defaults | guest rlimits |

## 4. The Kubernetes driver as a template

`driver_k8s.go` builds a pod: `restartPolicy: Never`,
`automountServiceAccountToken: false`, `runtimeClassName` from config,
`activeDeadlineSeconds = wall + 5`, resources from `Limits`, labels carrying
tenant and run for NetworkPolicy/quota/forensics, the workspace mounted at
`/work`, the platform binary mounted read-only for `ExtraReadOnly`; then waits
for completion, reads logs (capped), classifies the phase, and deletes the
pod. A vendor-sandbox driver has the same shape with API calls in place of
pods; a Kata/Firecracker driver is the Kubernetes driver with a different
`RuntimeClass` — which is why that reversal costs one config line.

## 5. What a remote sandbox vendor driver must additionally answer

* **Where does `/work` live?** It must be uploaded before and downloaded
  after each call (or mounted through the vendor); the workspace's
  authoritative copy stays on the platform's side.
* **Network:** `NetworkNone` must map to the vendor's "no egress"; if the
  vendor cannot guarantee it, the driver must refuse `NetworkNone` specs
  rather than approximate.
* **Data residency:** tenant workspace bytes leave the cluster — a contractual
  question, not a code one.
* **Latency:** per-call round trips of 50–300 ms change the capacity math
  ([capacity §4](../03-operations/05-capacity.md)).

## 6. Documentation to update

[sandbox](../architecture/04-sandbox.md) drivers table · [isolation](../reasoning/02-isolation.md) options table (strength, cold start, kernel-LPE consequence) · [configuration](../02-architecture/04-configuration.md) (`--sandbox-driver` values) · [limits and defaults §4](../05-reference/05-limits-and-defaults.md).

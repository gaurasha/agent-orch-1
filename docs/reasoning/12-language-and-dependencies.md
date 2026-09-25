# 12 · Language, binary and dependency policy

> Decision: **Go, one statically-linked binary that is the API server, the
> worker, the gateway, the sandbox init and the CLI shim (dispatch on
> subcommand and `argv[0]`); one external module (`lib/pq`); everything else —
> JWT, metrics, the Kubernetes client, the seccomp assembler — hand-rolled on
> the standard library.** Not Rust, not Python, not TypeScript; not a
> framework; not the usual thirty dependencies.
>
> Related: [02-isolation](02-isolation.md), [14-industry-learnings](14-industry-learnings.md)

## Problem

The platform is a trusted computing base for other people's credentials and
code. Two constraints follow that most application code does not face:

1. **Every dependency is inside the TCB.** A compromised or vulnerable module
   in the gateway is a compromised gateway. The 2024 `xz` backdoor, the 2018
   `event-stream` incident, the 2021 `ua-parser-js` hijack and the 2025 npm
   worm campaigns are the reference cases.
2. **The sandbox init runs as uid 0 inside a user namespace**, calls
   `clone`, `pivot_root`, `capset`, `prctl` and installs a BPF program. The
   language must give direct, predictable access to those syscalls with no
   runtime in the way at that moment.

Secondary: the same code must run on a laptop, in CI and in a cluster; a
single artefact simplifies pinning, scanning and rollback; and reviewers must
be able to read the whole privileged path.

## Options

| # | Language | Strongest case | Where it breaks for this problem |
|---|---|---|---|
| A | **Go** (chosen) | static binaries; `syscall`/`x/sys` expose everything needed; goroutines make N workers per process trivial; the standard library has HTTP, JSON, crypto, SQL, `slog`, `embed`; `go vet`, race detector, fast builds; the language every container runtime (runc, containerd, Kubernetes, gVisor) is written in — the idioms for this exact problem are established | GC pauses are the canonical "paused worker" (the fence exists partly for this); no memory safety *guarantees* beyond the GC (unsafe is rare but present in syscall code); the fork/exec gap requires the re-exec trick |
| B | **Rust** | memory safety without GC; the strongest choice for the *sandbox* half (youki, Firecracker, Bottlerocket are Rust); excellent for a seccomp/namespace jail | slower to write the *platform* half (HTTP, SQL, orchestration); the async ecosystem adds dependencies (tokio, hyper, sqlx…) that contradict constraint 1 unless vendored and audited; compile times; a two-language repo if only the jail is Rust |
| C | **Python** | the agent/LLM ecosystem; fastest to prototype the loop | a runtime, an interpreter and a package tree inside the TCB; namespaces/seccomp via ctypes or a C extension; a `venv` per environment; the GIL for N workers per process; the supply-chain surface (PyPI) is the largest of any ecosystem |
| D | **TypeScript / Node** | the other agent ecosystem; the console is already TS | same TCB problem as Python; `npm` has the worst-documented supply-chain incidents; syscall access needs native addons; a runtime in every container |
| E | **Java / Kotlin / C#** | mature, fast, good concurrency | a JVM/CLR in the sandbox-init path is wrong; startup time; the syscall story is JNI/P/Invoke |
| F | **C** for the jail, anything for the rest | the smallest possible init | memory-unsafe code in the most privileged path; a second toolchain; every container runtime moved *away* from this |

## Deep dive

### Why one binary

* **Re-exec for the sandbox init.** Go cannot safely run code between `fork`
  and `exec` (runtime threads and locks), so the jail is built by re-executing
  `/proc/self/exe` with a marker — the same shape as runc's `init`. That
  requires the binary that builds the jail to *be* the platform binary, or a
  second artefact to ship and pin.
* **The busybox pattern for the CLI shim.** `/opt/agentorch/bin/gh` in the
  broker sandbox is the platform binary bind-mounted read-only under another
  name; `main()` dispatches on `argv[0]`. One artefact, one review, no
  `curl | sh` in a jail.
* **Operational simplicity.** One image, one digest, one SBOM, one scan;
  every Deployment pins the same tag; version skew between control plane,
  workers and gateway is impossible.
* **The console is embedded** with `embed`, so the API and the SPA share an
  origin and a build.

### Why one external module

`lib/pq` is the only third-party code linked into the binary. Everything
else that would normally be a dependency is a small, purpose-built package:

| Would usually be | Here | Lines |
|---|---|---|
| `golang-jwt` | `jwtmini` (HS256 only, `sub`/`aud`/`exp`, constant-time compare) | ~110 |
| `client_golang` (Prometheus) | `obs.Metrics` renders the exposition format | ~180 |
| `client-go` (Kubernetes) | `k8s.Client`: in-cluster token, create/get/delete/log pods | ~250 |
| `libseccomp-golang` | `seccomp_linux.go` assembles the BPF program directly | ~200 |
| `x/sys/unix` | `syscall` from the standard library (x/sys needed a newer Go) | — |
| a web framework | `net/http` with Go 1.22 method patterns | — |

The trade is explicit: **less functionality, entirely reviewable**. The JWT
package does not do RS256, key rotation or JWKS; it does not need to (one
HMAC key, one audience, five-minute tokens). The metrics package has no
exemplars or native histograms. Each is a few hundred lines that a reviewer
can read end to end, versus tens of thousands in the usual dependency
closure — inside a process that holds credentials.

`x/sys` was dropped for a mundane reason (it required a Go version the build
environment did not have), and the mundane reason is also the argument: a
dependency is a coupling to someone else's release schedule inside your
TCB.

### What the policy costs

* Re-implementations can have bugs the ecosystem has fixed. Mitigation: each
  hand-rolled package is small, has tests, and implements the *subset* the
  platform uses; the JWT and seccomp packages have negative tests.
* Missing features: RS256, JWKS, OpenMetrics, informers/watch in the k8s
  client. Each is a documented production step, not an unknown.
* The console (`ui/`) has the normal npm tree — it is not in the TCB (it runs
  in the operator's browser) and is built separately.

### Go-specific hazards handled

* **GC pauses** are the paused-worker case; the fence makes them safe.
* **`fmt` on a struct with a secret** — solved by the `Secret` type ([06](06-credentials.md)).
* **`syscall.Setuid` and threads** — since Go 1.16 the runtime broadcasts
  setuid across threads; the jail's order (no_new_privs → bounding set →
  setgroups/setgid/setuid → clear caps → seccomp) was still found the hard
  way (B1).
* **Exported vs unexported syscall structs** — `CapUserHeader` is not
  exported; local struct definitions and `RawSyscall(SYS_CAPSET)` are used.

## State of the art

* **Container runtimes are Go** (runc, containerd, CRI-O, gVisor's Sentry) or
  **Rust** (youki, crun is C, Firecracker, Kata's runtime-rs).
* **Single-binary platforms** are a Go tradition: Consul, Vault, Nomad,
  Caddy, Tailscale, Prometheus, etcd, kind.
* **Supply-chain frameworks**: SLSA levels, Sigstore signing, SBOMs (SPDX,
  CycloneDX), `govulncheck`, dependency review — the production checklist
  for the one dependency and the base image.
* **Anthropic's Claude Code** is TypeScript; **OpenAI's Codex CLI** moved
  from TypeScript to Rust in 2025 citing performance and the sandboxing
  primitives — a public data point that the *sandbox* half pulls toward
  systems languages.

## Documented issues

* **`xz`/liblzma backdoor (CVE-2024-3094)**: a maintainer-level supply-chain
  attack on a dependency of `sshd` on some distributions — the case for "as
  few dependencies as possible in the privileged path".
* **`event-stream`** (2018), **`ua-parser-js`** (2021), **`colors`/`faker`**
  (2022), and the 2025 npm worm campaigns (Shai-Hulud) — the npm ecosystem's
  record; the console is the only npm consumer here and it runs in a
  browser.
* **PyPI typosquatting and malicious packages** — the recurring PyPI
  incident class; the reason a Python TCB would need a private index and
  hash-pinning.
* **Go's own incidents** are fewer but exist (e.g. malicious modules found
  on the module proxy); `govulncheck` and the checksum database are the
  mitigations.
* **The re-exec trick's failure modes**: runc CVE-2019-5736 abused
  `/proc/self/exe` from inside a container; the jail here bind-mounts the
  binary read-only and the container (in production) runs under gVisor.

## Evidence in this repository

* `go.mod`: `require github.com/lib/pq` and nothing else.
* `TestSecret_RevealCallSitesAreFewAndIntentional` walks the source — a test
  that is only possible because the source is small.
* `go vet` and the race detector are in `make lint`/`make test`.

## Would reverse if

* the sandbox half grows (a supervisor, a proxy, an agent inside the jail) →
  Rust for that component, Go for the platform, two artefacts;
* RS256/JWKS/OIDC are required for run tokens → adopt a vetted JWT library
  rather than extend `jwtmini`;
* the Kubernetes integration needs informers, leader election or CRDs →
  `client-go`, accepted as a reviewed dependency.

## References

* Go, *Standard library* — https://pkg.go.dev/std · `syscall` — https://pkg.go.dev/syscall · `embed` — https://pkg.go.dev/embed · `log/slog` — https://pkg.go.dev/log/slog · `net/http` routing patterns (Go 1.22) — https://go.dev/blog/routing-enhancements
* Go, *Vulnerability management (`govulncheck`)* — https://go.dev/doc/security/vuln/ · *Module checksum database* — https://sum.golang.org/
* runc, `libcontainer` init (re-exec) — https://github.com/opencontainers/runc/tree/main/libcontainer
* BusyBox, *FAQ (multi-call binary)* — https://busybox.net/FAQ.html
* SLSA — https://slsa.dev/ · Sigstore — https://www.sigstore.dev/ · CycloneDX — https://cyclonedx.org/
* CVE-2024-3094 (xz) — https://nvd.nist.gov/vuln/detail/CVE-2024-3094 · npm `event-stream` incident — https://github.com/dominictarr/event-stream/issues/116 · `ua-parser-js` hijack (GitHub advisory) — https://github.com/advisories/GHSA-pjwm-rvh2-c87w
* OpenAI, *Codex CLI (Rust)* — https://github.com/openai/codex
* youki (Rust OCI runtime) — https://github.com/youki-dev/youki

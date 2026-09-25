# 02 · Isolation — how model-authored code is contained

> Decision: **build the sandbox from raw Linux primitives in the PoC (six
> namespaces, cgroups, rlimits, capability drop, seccomp-BPF, empty network
> namespace) and run it under gVisor in production.** Not runc, not
> Kata/Firecracker microVMs, not WebAssembly, not a remote sandbox vendor —
> each of which is the right answer under a condition stated below.
>
> Diagrams: [04-sandbox](../architecture/04-sandbox.md), [05-credential-plane](../architecture/05-credential-plane.md) · Concept: [Linux isolation](../01-concepts/01-linux-isolation.md) · Reference: [syscall denylist](../05-reference/02-syscalls.md)

## Problem

`exec.bash` runs a script the model wrote. The model read a web page an
attacker wrote. Therefore the script is attacker-controlled, and it runs on
our node, next to other tenants' work and next to the process holding a
credential. What must be true?

1. The script cannot read anything but its workspace — not the host
   filesystem, not `/proc` of other processes, not the node's `/etc`.
2. The script cannot send a packet. (Exfiltration needs a channel; remove the
   channel and the lethal trifecta is broken — see [10](10-prompt-injection.md).)
3. The script cannot gain privilege, escape its namespace, or exhaust the
   node.
4. All of the above must hold at **8 ms** per invocation, because the
   measured design choice — a sandbox per tool call — depends on it.
5. The strength of the boundary must be a *choice per environment*: a laptop
   does not need a microVM; production does not accept a shared kernel.

## Options

| # | Option | Strongest case for it | Where it breaks | Who uses it in production |
|---|---|---|---|---|
| A | **No isolation** — run the tool in the worker | zero cost; the model "just works" | any injection is a full compromise of the worker and its credentials; unacceptable by constraint 1–3 | early demos; never at scale |
| B | **seccomp / AppArmor only** (no namespaces) | tiny cost; filters the dangerous syscalls | the filesystem, the network and other processes are all still visible; a denylist cannot express "only your workspace" | as a *layer* in every container runtime; alone, nowhere serious |
| C | **Linux namespaces + cgroups + seccomp, raw** (chosen for the PoC) | the actual mechanism every container runtime uses, with nothing between the kernel and us; 8.4 ms cold start measured; every layer is a ~50-line function that can be tested in isolation | a kernel LPE is a host compromise (shared kernel); the code is privilege-sensitive and ours to get right (bugs B1, B2 were here); no ecosystem of hardened defaults | bubblewrap (Flatpak), Anthropic's `sandbox-runtime` (bubblewrap on Linux, seatbelt on macOS), Chrome's Linux sandbox, systemd, nsjail, Firejail |
| D | **runc / crun via an OCI runtime or Docker** | the same kernel primitives with years of hardening and CVE response; a one-line PodSpec mapping; SELinux/AppArmor profiles for free | 300–800 ms cold start (daemon round trip, image layers); a daemon with root on the node; still a shared kernel; Docker-in-Docker for CI is a known mess | most CI systems; Kubernetes' default runtime (containerd + runc) |
| E | **gVisor (runsc)** — user-space kernel (chosen for production) | the guest's syscalls hit the Sentry, not the host kernel; a Linux LPE in the guest compromises the Sentry, which is itself sandboxed by seccomp; runs as a normal Kubernetes `RuntimeClass` on standard nodes; Google runs it for App Engine, Cloud Run, Cloud Functions, GKE Sandbox | 10–30 % syscall-heavy overhead; some syscalls unimplemented (incompatibility list); 1–3 s pod start in k8s; the Sentry is 100 K+ lines of Go that is *also* an attack surface | Google Cloud Run / GKE Sandbox, OpenAI's early Code Interpreter (publicly stated), many "AI sandbox" vendors, Modal (gVisor) |
| F | **MicroVMs** — Firecracker / Kata Containers / Cloud Hypervisor | a hardware virtualisation boundary: the guest has its own kernel; the strongest widely-available isolation; Firecracker boots in ~125 ms with a minimal kernel; AWS Lambda and Fargate run on it | needs `/dev/kvm` — nested virtualisation or bare-metal nodes, which many clusters do not have; heavier per-instance memory (tens of MB); image/rootfs management; snapshot/restore for warm starts is real engineering; Kata on k8s needs a compatible node OS | AWS Lambda/Fargate (Firecracker), E2B (Firecracker), Fly Machines (Firecracker), Codesandbox, Northflank, Kata at Alibaba/Ant |
| G | **WebAssembly runtimes** (wasmtime, Wasmer, WasmEdge, Spin) | capability-based by construction (WASI); microsecond instantiation; deterministic; no kernel at all | the agent wants `bash`, `python`, `pandoc`, `gh` — none of which run as Wasm today without a rewrite; WASI's filesystem/network story is still maturing; this constrains *what agents can do*, not just how safely | Fastly, Cloudflare Workers (V8 isolates, not Wasm proper), Shopify Functions, Fermyon |
| H | **Language-level sandboxes** (Pyodide in a browser, Deno permissions, restricted Python) | trivial to embed; no OS dependency | only one language; escapes are a known genre (Python's `restrictedpython` history); still needs a process boundary for the credential argument | Pyodide for in-browser code interpreters |
| I | **Remote sandbox vendors** (E2B, Modal Sandboxes, Daytona, Vercel Sandbox, Cloudflare Sandbox SDK, AWS AgentCore Code Interpreter, OpenAI's hosted tools) | isolation is someone else's problem and it is usually Firecracker or gVisor; warm pools and snapshots are solved; pay per second | a network hop per tool call (50–300 ms) and an external dependency on every step; data leaves the cluster; per-tenant network policy becomes a vendor feature; cost at 10 k calls/min | agent products that do not own infrastructure: many YC-era agent startups; Manus (E2B publicly), several coding agents |

## Deep dive: the layers and why each exists

The sandbox is not one mechanism; it is ten, each removing one capability
that an attack needs. [04-sandbox](../architecture/04-sandbox.md) shows them
in order. The reasoning for each:

| Layer | Removes | Why not skip it |
|---|---|---|
| cgroup cpu/mem/pids + rlimits + parent wall-clock | resource exhaustion | a fork bomb or a `while true` is the *cheapest* attack; a sandbox that only guards secrets still lets one tenant take down a node |
| user namespace | root on the host | with `uid 0 → host 100000`, "root inside" is nobody outside; without it every other layer is one `CAP_SYS_ADMIN` away from useless |
| mount namespace + `pivot_root` on tmpfs | the host filesystem | `chroot` is escapable by a process holding an outside directory fd (a decades-old trick); `pivot_root` + unmounting the old root leaves *no mount that refers to the host* |
| synthesised `/etc`, empty `resolv.conf` | host configuration, internal DNS names | binding the host's `/etc` leaks whatever an operator ever dropped there; a stale `resolv.conf` is free reconnaissance |
| PID namespace + fresh `/proc` | other processes, including the one holding the credential | `/proc/<pid>/environ` is the canonical credential leak; a fresh procfs shows only the sandbox's tree |
| empty network namespace | exfiltration | "no route" fails *before a packet exists*; it cannot be misconfigured the way a firewall rule can |
| UTS + IPC namespaces | shared memory channels, hostname | small, free, and closes the SysV/mqueue side channel between tenants |
| `no_new_privs` | setuid binaries and file capabilities | the precondition for installing seccomp unprivileged; neuters every setuid escalation in the tree |
| capability bounding set → ∅, then `setuid(1000)` | capabilities, forever | with the bounding set empty no `execve` can *acquire* a capability; the order matters (B1: caps before uid → `setgroups` EPERM → glibc `abort()`) |
| seccomp-BPF denylist (35 + `clone3`) | the syscalls escapes are built from | `mount`, `ptrace`, `unshare`, `setns`, `open_by_handle_at`, `io_uring_*`, `bpf`, `keyctl`, `kexec_*` …; `clone3` → `ENOSYS` because its flags hide behind a pointer seccomp cannot inspect (Docker does the same) |

**Denylist vs allowlist.** Docker's default profile *allows* ~300 syscalls and
blocks the rest; this repository blocks 36 and allows the rest. An allowlist
is stronger in principle but needs the payload's syscall surface enumerated —
impossible for "any toolchain the tenant installs". The namespaces are the
primary boundary; seccomp is defence in depth against kernel bugs reachable
through specific syscalls, which is exactly what a denylist of *escape
primitives* targets. Production under gVisor gets the allowlist effect for
free: the Sentry implements a fixed syscall surface.

**Why raw primitives and not runc in the PoC.** Three reasons. (1) The number
that shaped the architecture — a sandbox per tool call — required knowing the
real cold-start floor, which is 8.4 ms with primitives and hundreds of ms
through a daemon. (2) Each layer had to be *proved*, not assumed: the safety
suite asserts the mechanism (an `ENETUNREACH`, an `EPERM`, a `pids.max`
refusal), and a negative control that must *succeed* on the host guards
against tests that pass for the wrong reason (B2). (3) It is the only way to
learn what runc is doing for you, which is what makes the production choice
(E) an informed one.

**Why gVisor and not Firecracker in production.** The threat model's worst
case is a kernel LPE from model-authored code. Under runc, that is a host
compromise. Under gVisor, the guest kernel is the Sentry, itself confined by
seccomp and running as an unprivileged process; the Linux LPE does not apply
to it, and a Sentry escape still lands on a tainted node with no credentials
and a default-deny policy. Firecracker is stronger (hardware boundary) but
requires KVM on the node; on the managed Kubernetes offerings most teams run,
that means special node pools or bare metal. gVisor is a `RuntimeClass` on any
node. The design's "would reverse" condition is exactly that: if the cluster
has KVM, Kata is the better default.

## State of the art (2025–26)

* **Anthropic** open-sourced `sandbox-runtime`, the sandboxing Claude Code
  uses: bubblewrap on Linux (namespaces + seccomp — option C), seatbelt on
  macOS, with **network egress through a proxy** that enforces a domain
  allowlist. That is this repository's design for the broker sandbox: no
  network in the jail, a proxy as the only route.
* **OpenAI Codex** (cloud) runs each task in an isolated container with
  network disabled by default and an allowlist when enabled; the local Codex
  CLI uses seatbelt/landlock+seccomp. OpenAI has stated publicly that Code
  Interpreter ran on gVisor.
* **E2B** ("sandboxes for AI agents") runs Firecracker microVMs with ~150 ms
  starts and snapshot/restore; **Modal** runs gVisor; **Fly Machines** are
  Firecracker; **Daytona**, **Vercel Sandbox** (Firecracker), **Cloudflare
  Sandbox SDK** (containers on Cloudflare's runtime) and **AWS Bedrock
  AgentCore Code Interpreter** (microVM session isolation) are the 2025 wave.
  The convergence is clear: **Firecracker when the vendor owns the metal,
  gVisor when it runs on someone's Kubernetes.**
* **Google**: gVisor is the sandbox for Cloud Run, Cloud Functions gen 1, App
  Engine flexible and GKE Sandbox; the gVisor paper and the "Blending
  containers and VMs" post give the design and its overhead numbers.
* **Kata Containers** is the CNCF microVM runtime; Alibaba and Ant run it at
  scale; it is the reference for "Firecracker under Kubernetes".

## Documented issues

* **runc CVE-2019-5736** — a container process could overwrite the host `runc`
  binary via `/proc/self/exe`; a reminder that the runtime binary itself is
  attack surface, and one reason the jail here re-execs a *read-only
  bind-mounted* copy of itself.
* **"Leaky Vessels" (CVE-2024-21626)** — `runc` leaked a host directory fd into
  the container's working directory, allowing host filesystem access; the class
  of bug (an fd that survives into the jail) is why the jail here closes
  everything but the config and sync pipes and why `pivot_root` unmounts the
  old root.
* **Dirty Pipe (CVE-2022-0847)** and **Dirty COW (CVE-2016-5195)** — kernel
  bugs reachable from unprivileged code with no special syscalls; the reason
  "shared kernel" is the stated residual risk of option C/D and gVisor is the
  production choice.
* **gVisor compatibility** — the project maintains a list of unimplemented
  syscalls and known-incompatible software; syscall-heavy workloads see
  measurable overhead (the gVisor performance guide quotes the ranges).
  Mitigation here: the sandbox runs short tool calls, not services.
* **Firecracker on Kubernetes** requires `/dev/kvm`; nested virtualisation is
  unavailable or slow on many cloud instance types, which is why Kata's docs
  list supported hypervisors and node requirements.
* **`clone3` and seccomp** — seccomp cannot inspect `clone3`'s struct
  argument, so a filter that blocks namespace creation via `clone` flags can
  be bypassed with `clone3`; Docker's default profile returns `ENOSYS` for it
  (forcing the glibc fallback), which this repository copies.
* **User namespaces as attack surface** — several kernel LPEs have been
  reachable only *because* unprivileged user namespaces were enabled (a
  reason some distributions disable them). The trade-off is accepted because
  the alternative is running the jail builder as real root.

## Evidence in this repository

* S1–S11 in [safety proofs](../04-evidence/02-safety-proofs.md): no network
  (with a host negative control), host filesystem invisible, read-only root,
  workspace-only persistence, no capabilities, escape syscalls blocked,
  wall-clock, memory, fork bomb, output cap, cold start.
* B1 (privilege-drop order → SIGABRT), B2 (a network test that passed for the
  wrong reason), B3 (a fork-bomb test that never forked) in
  [bugs found](../04-evidence/03-bugs-found.md).
* Cold start: mean 8.4–8.7 ms, worst 15–20 ms over 10 runs on a 4-core VM
  ([benchmarks §1](../04-evidence/01-benchmarks.md)).

## Would reverse if

* the cluster's node pools have KVM → Kata/Firecracker replaces gVisor as the
  production `RuntimeClass` (the driver interface does not change);
* the agents' tools were rewritable as Wasm components → option G removes the
  kernel from the TCB entirely;
* tool-call volume were low and latency-tolerant → a remote sandbox vendor (I)
  removes ~1 200 lines of privilege-sensitive code from this repository;
* the measured cold start were ≥ 500 ms → sandboxes per *run* with a warm
  pool, not per call.

## References

* Linux man pages: `namespaces(7)` https://man7.org/linux/man-pages/man7/namespaces.7.html · `user_namespaces(7)` https://man7.org/linux/man-pages/man7/user_namespaces.7.html · `pivot_root(2)` https://man7.org/linux/man-pages/man2/pivot_root.2.html · `seccomp(2)` https://man7.org/linux/man-pages/man2/seccomp.2.html · `capabilities(7)` https://man7.org/linux/man-pages/man7/capabilities.7.html · `cgroups(7)` https://man7.org/linux/man-pages/man7/cgroups.7.html · `prctl(2)` (`PR_SET_NO_NEW_PRIVS`) https://man7.org/linux/man-pages/man2/prctl.2.html
* Kernel docs, *Seccomp BPF* — https://www.kernel.org/doc/html/latest/userspace-api/seccomp_filter.html
* Docker, *Seccomp security profiles* (the default profile, `clone3` handling) — https://docs.docker.com/engine/security/seccomp/
* gVisor — https://gvisor.dev/ · *Architecture guide* https://gvisor.dev/docs/architecture_guide/ · *Performance* https://gvisor.dev/docs/architecture_guide/performance/ · *Compatibility* https://gvisor.dev/docs/user_guide/compatibility/
* Google Cloud, *Open-sourcing gVisor* (2018) — https://cloud.google.com/blog/products/gcp/open-sourcing-gvisor-a-sandboxed-container-runtime
* Agache et al., *Firecracker: Lightweight Virtualization for Serverless Applications* (NSDI 2020) — https://www.usenix.org/conference/nsdi20/presentation/agache
* Firecracker — https://firecracker-microvm.github.io/ · Kata Containers — https://katacontainers.io/
* Kubernetes, *RuntimeClass* — https://kubernetes.io/docs/concepts/containers/runtime-class/ · *Pod Security Standards* — https://kubernetes.io/docs/concepts/security/pod-security-standards/
* bubblewrap — https://github.com/containers/bubblewrap · nsjail — https://github.com/google/nsjail
* Anthropic, `sandbox-runtime` — https://github.com/anthropic-experimental/sandbox-runtime
* OpenAI, *Codex security* (sandboxing, network policy) — https://developers.openai.com/codex/security
* E2B — https://e2b.dev/docs · Modal Sandboxes — https://modal.com/docs/guide/sandbox · Fly Machines — https://fly.io/docs/machines/ · Daytona — https://www.daytona.io/docs · Vercel Sandbox — https://vercel.com/docs/vercel-sandbox · Cloudflare Sandbox SDK — https://developers.cloudflare.com/sandbox/ · AWS AgentCore Code Interpreter — https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/code-interpreter.html
* CVE-2019-5736 (runc) — https://nvd.nist.gov/vuln/detail/CVE-2019-5736 · CVE-2024-21626 (Leaky Vessels) — https://nvd.nist.gov/vuln/detail/CVE-2024-21626 · CVE-2022-0847 (Dirty Pipe) — https://nvd.nist.gov/vuln/detail/CVE-2022-0847 · CVE-2016-5195 (Dirty COW) — https://nvd.nist.gov/vuln/detail/CVE-2016-5195
* WebAssembly: wasmtime — https://wasmtime.dev/ · WASI — https://wasi.dev/

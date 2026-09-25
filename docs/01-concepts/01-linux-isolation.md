# Linux isolation from first principles

> **Prerequisite:** [Threat model](../00-problem/04-threat-model.md)
> **Read next:** [Durable execution](02-durable-execution.md)
> **Code:** [`internal/sandbox/`](../../backend/internal/sandbox/)

People say "run it in a container" as though a container were something the
kernel provides. **It is not.** Linux has no `container` object, no
`CONTAINER_CREATE` syscall, no `struct container` anywhere in the source tree.

A container is an ordinary process that has had six or seven unrelated kernel
features applied to it. Understanding them one at a time is the difference
between *using* a sandbox and *knowing what it protects you from* — which is the
difference between a security claim and a hope.

This document builds each mechanism from the syscall up, then shows exactly how
this codebase composes them.

---

## 1. Namespaces — "what can this process see?"

A [namespace](https://man7.org/linux/man-pages/man7/namespaces.7.html) gives a
process a private copy of some global kernel resource. Created with
[`clone(2)`](https://man7.org/linux/man-pages/man2/clone.2.html) flags, or
entered later with `unshare(2)` / `setns(2)`.

| Flag | Namespace | Without it | With it |
|---|---|---|---|
| `CLONE_NEWNS` | mount | sees every mount on the host | sees only what you mounted for it |
| `CLONE_NEWPID` | PID | sees and can signal every process | sees only itself and its children; it is PID 1 |
| `CLONE_NEWNET` | network | uses host interfaces and routes | **its own stack — which can be empty** |
| `CLONE_NEWUSER` | user | uid 0 is real root | uid 0 inside maps to an unprivileged uid outside |
| `CLONE_NEWUTS` | UTS | shares the hostname | its own hostname |
| `CLONE_NEWIPC` | IPC | shares SysV shm / message queues | its own |

This system uses **all six**
([`jail_linux.go`](../../backend/internal/sandbox/jail_linux.go)):

```go
Cloneflags: syscall.CLONE_NEWNS |   // own mount table
    syscall.CLONE_NEWPID |          // cannot see or signal host processes
    syscall.CLONE_NEWUTS |          // own hostname
    syscall.CLONE_NEWIPC |          // own SysV IPC and message queues
    netFlag |                       // CLONE_NEWNET, unless this is the broker
    syscall.CLONE_NEWUSER,          // uid 0 inside is unprivileged outside
```

### 1a. The network namespace is the strongest control in the system

When this system says a sandbox has no network access, it does **not** mean a
firewall rule that says `DROP`. A firewall rule is configuration: it can be
mistyped, reordered, or shadowed by a later rule. It is something a human
maintains.

An empty network namespace is *structural*. The process has:

- one interface (`lo`), and it is **down**
- **zero** addresses
- **zero** routes

There is nothing to misconfigure. The kernel's answer to
`connect(169.254.169.254:80)` is `ENETUNREACH` — *Network is unreachable* —
because from in there, **there is no network at all**.

Measured, from [`sandbox_linux_test.go`](../../backend/internal/sandbox/sandbox_linux_test.go):

```
denied AWS/GCP metadata   169.254.169.254:80  -> errno=ENETUNREACH
denied public internet    1.1.1.1:443         -> errno=ENETUNREACH
denied loopback           127.0.0.1:22        -> errno=ENETUNREACH
interfaces=['lo']  routes=0
```

> **Why 169.254.169.254 specifically?**
> On AWS, GCP and Azure that address is the **instance metadata service**.
> Anything that can issue an HTTP GET to it can often retrieve the node's cloud
> credentials. It is the highest-value target on any cloud machine and it is
> reachable by default from every process. A large fraction of real cloud
> breaches are exactly this: get code execution anywhere → curl the metadata
> endpoint → assume the node's IAM role.
>
> AWS IMDSv2 mitigates it with a PUT-then-GET token dance, which helps against
> naive SSRF but not against a process that can run arbitrary code. The empty
> netns removes the reachability entirely.

### 1b. The user namespace is what makes "root in the container" harmless

A user namespace maps uid ranges. The child sees uid 0; the host sees something
else entirely.

This codebase maps **two ranges** deliberately:

```go
UidMappings: []syscall.SysProcIDMap{
    {ContainerID: 0, HostID: 0,      Size: 1},      // setup only
    {ContainerID: 1, HostID: 100000, Size: 65535},  // the payload
},
```

Why two? Because setup and execution need different privilege:

| Phase | Runs as | Needs |
|---|---|---|
| Build the rootfs, mount, `pivot_root` | container uid 0 | `CAP_SYS_ADMIN` **in this userns** |
| Run the payload | container uid **1000** → host uid **100999** | nothing at all |

The child does the privileged setup, then **drops to container uid 1000** before
`execve`. Host uid 100999 owns no file on the system. Even a complete compromise
of the payload yields an identity with no host authority.

> **The bug I hit here, preserved as a warning.** My first version mapped
> `{ContainerID: 0, HostID: 65534, Size: 1}` — intending "container root is
> nobody". But the *process itself* was host uid 0, which that map does not
> cover, so it landed on the overflow uid (65534) with an **empty permitted
> capability set** and `sethostname` failed with `EPERM`. The mapping describes
> which host uids are visible inside, not which identity you become.

### 1c. The PID namespace does more than hide processes

Two properties, one obvious and one not:

- **Obvious:** the payload cannot see or signal host processes. `ps` shows 3–4
  PIDs.
- **Not obvious:** it cannot read `/proc/<pid>/environ` of any process outside
  its namespace. This is what makes the
  [two-sandbox credential pattern](05-secrets.md#3-the-two-sandbox-pattern) work —
  the broker's environment is not merely permission-protected, it is
  **unaddressable**.

A fresh `procfs` mount is required for this; otherwise the namespace exists but
`/proc` still shows the host's view:

```go
syscall.Mount("proc", filepath.Join(c.Root, "proc"), "proc",
    syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, "")
```

---

## 2. cgroups — "how much can this process use?"

[cgroups](https://docs.kernel.org/admin-guide/cgroup-v2.html) bound resource
consumption for an entire process **tree**.

### Why not just `ulimit`?

| | `RLIMIT_AS` (ulimit) | cgroup `memory.max` |
|---|---|---|
| Scope | one process | the whole tree |
| Measures | **virtual** address space | **resident** memory |
| Fork bomb | trivially sidestepped | contained |
| Go runtime / JVM | **breaks them** — they reserve huge virtual mappings | fine |

`RLIMIT_AS` is close to useless for this workload for that third row alone. The
Go runtime reserves a large virtual arena at startup; a virtual-size limit either
kills it or has to be set so high it bounds nothing.

This system uses **both**: cgroups for the tree-wide resident bounds, rlimits for
the per-process ones that cgroups do not express
([`cgroup_linux.go`](../../backend/internal/sandbox/cgroup_linux.go),
[`init_linux.go`](../../backend/internal/sandbox/init_linux.go)).

### Both hierarchies, because the world is split

| | cgroup v1 | cgroup v2 |
|---|---|---|
| Layout | one tree per controller | one unified tree |
| Memory limit | `memory/…/memory.limit_in_bytes` | `…/memory.max` |
| CPU quota | `cpu/…/cpu.cfs_quota_us` + `cfs_period_us` | `…/cpu.max` (`"quota period"`) |
| PID limit | `pids/…/pids.max` | `…/pids.max` |
| OOM signal | `memory.oom_control` → `oom_kill N` | `memory.events` → `oom_kill N` |

Detection is at runtime (`/sys/fs/cgroup/cgroup.controllers` exists ⟹ v2),
because CI runners, older nodes and container-in-container environments still
expose v1 — and failing on the operator's laptop is not acceptable.

### The controls, and what each stops

| Control | Value (default) | Stops |
|---|---|---|
| `memory.max` | 256 MiB | Memory bombs. **Measured:** OOM-killed, `oom_kill` counter confirmed |
| `memory.swap.max` = 0 | — | Escaping the memory bound into swap, which would turn a memory limit into a latency attack on every other tenant on the node |
| `cpu.max` | 500m | CPU monopolisation |
| `pids.max` | 64 | Fork bombs. **Measured:** kernel refused at **23 children** against a limit of 24 |

> **Why assert the *number* of children, not just "it did not hang"?** Because a
> fork bomb test that merely completes proves nothing — it could have been
> stopped by a system-wide ceiling, or by the shell failing to parse. Asserting
> that the refusal happened at ≈`pids.max` proves **the cgroup** is what bound
> it. An earlier version of this test used a shell fork bomb that was a dash
> syntax error; it "passed" in 27 ms having never forked at all.

### OOM detection is subtler than it looks

`memory.failcnt` counts *times the limit was reached*, which also happens when
reclaim succeeds and nothing is killed. Using it alone reports an OOM for
workloads that merely ran close to their limit. The authoritative signal is the
`oom_kill` counter in `memory.oom_control` (v1) or `memory.events` (v2):

```go
// Prefer memory.oom_control's oom_kill counter: it counts actual kills.
// memory.failcnt only counts times the limit was reached, which also
// happens when reclaim succeeds and nothing is killed.
```

This matters operationally: an operator must be able to distinguish *"this agent
is misbehaving"* from *"this agent is under-provisioned"*, and `failcnt` conflates
them.

### Cleanup is not instant

`rmdir` on a cgroup fails with `EBUSY` while it still has members, and the
SIGKILLs we send are delivered asynchronously. A single immediate `os.Remove`
leaks a directory per sandbox — at hundreds of sandboxes a minute, that becomes
a real problem on the node. Hence the bounded retry in `Destroy()`.

---

## 3. Capabilities — "root" is not one thing

Since Linux 2.2, root's powers are split into ~40
[capabilities](https://man7.org/linux/man-pages/man7/capabilities.7.html):

| Capability | Grants |
|---|---|
| `CAP_SYS_ADMIN` | mount, and a grab-bag of ~30 other things. Effectively root |
| `CAP_SYS_PTRACE` | inspect and modify other processes' memory |
| `CAP_NET_ADMIN` | configure networking |
| `CAP_NET_RAW` | raw sockets — packet crafting, ARP spoofing |
| `CAP_SETUID` / `CAP_SETGID` | change identity |
| `CAP_SETPCAP` | modify the capability **bounding** set |
| `CAP_DAC_OVERRIDE` | bypass all file permission checks |

Each process has four sets:

| Set | Meaning |
|---|---|
| **Effective** | what it can use *right now* |
| **Permitted** | what it may move into effective |
| **Inheritable** | what survives `execve` (with file capabilities) |
| **Bounding** | the ceiling — **nothing outside it can ever be acquired** |

### The bounding set is the one that matters

Clearing *effective* means "cannot use capabilities right now". Emptying the
**bounding** set means **no `execve` anywhere in this process tree can ever
acquire one**, regardless of what binary runs or what file capabilities it
carries. It is a one-way door.

```go
const prCapBsetDrop = 24
for capID := 0; capID <= 64; capID++ {
    syscall.RawSyscall(syscall.SYS_PRCTL, prCapBsetDrop, uintptr(capID), 0)
}
```

Verified from inside the sandbox:

```
CapEff: 0000000000000000    ← nothing usable now
CapBnd: 0000000000000000    ← nothing ever acquirable
```

---

## 4. `no_new_privs` — closing the setuid door

A [setuid](https://man7.org/linux/man-pages/man2/execve.2.html) binary runs as
its owner rather than its caller. That is how `sudo`, `ping` and `passwd` work.

[`PR_SET_NO_NEW_PRIVS`](https://docs.kernel.org/userspace-api/no_new_privs.html)
permanently disables that for the process **and every descendant**. It cannot be
unset. After it, `execve` can never grant more privilege than the caller had.

It does double duty here:

1. Neuters any setuid binary that is somehow reachable.
2. Is a **precondition** for installing an unprivileged seccomp filter — the
   kernel refuses `SECCOMP_SET_MODE_FILTER` without either `CAP_SYS_ADMIN` or
   `no_new_privs`, because otherwise a filter could be used to confuse a setuid
   binary into misbehaving.

---

## 5. seccomp-BPF — "which syscalls may this process make?"

Every service a program asks of the kernel is a
[syscall](https://man7.org/linux/man-pages/man2/syscalls.2.html) — roughly 350
of them on x86-64.
[seccomp](https://www.kernel.org/doc/html/latest/userspace-api/seccomp_filter.html)
attaches a classic-BPF program that inspects each one and returns a verdict:
allow, fail with an errno, log, trap, or kill.

### The filter, instruction by instruction

The kernel hands the filter a `struct seccomp_data`:

```c
struct seccomp_data {
    int   nr;                    // offset 0  — syscall number
    __u32 arch;                  // offset 4  — architecture
    __u64 instruction_pointer;   // offset 8
    __u64 args[6];               // offset 16 — the arguments
};
```

Our program ([`seccomp_linux.go`](../../backend/internal/sandbox/seccomp_linux.go)):

```
[0]  LD  W ABS  offset 4        ; A = arch
[1]  JEQ AUDIT_ARCH_X86_64, +1  ; match? skip the kill
[2]  RET KILL_PROCESS           ; wrong arch -> die
[3]  LD  W ABS  offset 0        ; A = syscall number
[4]  JGE 0x40000000, +0, +1     ; x32 ABI bit set?
[5]  RET KILL_PROCESS           ; yes -> die
[6]  JEQ 435 (clone3), +0, +1
[7]  RET ERRNO|ENOSYS           ; force the filterable clone(2) fallback
[8..] for each denied syscall:  JEQ nr -> RET ERRNO|EPERM
[n]  RET ALLOW
```

Two guards before the denylist even starts, and both are load-bearing:

**The architecture check.** Without it, a 32-bit process uses a *different*
syscall numbering — on i386, `165` is `nfsservctl`, not `mount`. Every entry in
the denylist would be checking the wrong syscall. This is a classic seccomp
bypass and it is why the filter validates `arch` first.

**The x32 rejection.** The x32 ABI uses the same registers as x86-64 but ORs
syscall numbers with `0x40000000`. A filter that only compares bare numbers
misses every x32 call.

### Why a denylist, not an allowlist

An allowlist is strictly stronger and is what
[Docker ships](https://github.com/moby/moby/blob/master/profiles/seccomp/default.json)
(~350 permitted syscalls). This system uses a denylist, deliberately:

| | Allowlist | Denylist |
|---|---|---|
| Security | stronger — unknown syscalls blocked by default | weaker — new syscalls allowed until added |
| Failure mode | **breaks real workloads** in confusing ways (Python's `importlib`, glibc NSS, CPU-feature probing) | permissive |
| Review burden | enumerate everything benign | enumerate what escapes need |

The deciding argument: in the production design, seccomp is **layer 7 of 8** and
is *superseded* by gVisor, which implements the syscall ABI in user space and
makes the host syscall surface largely irrelevant. Spending the complexity
budget on removing known escape primitives beats spending it enumerating
everything benign.

> If gVisor were removed from the design, this should become an allowlist.

### `clone3` and `ENOSYS` — the subtlest entry

```go
// clone3 gets ENOSYS rather than EPERM on purpose: seccomp cannot inspect its
// arguments (they live behind a struct pointer), so it could otherwise be used
// to create namespaces without us seeing the flags.
```

seccomp can only inspect *register* arguments. `clone3` takes a pointer to a
`struct clone_args`, so the flags are in memory the filter cannot read. A
`clone3` with `CLONE_NEWUSER` would sail past a filter that only checks `clone`.

Returning `EPERM` would break every glibc-linked binary, because modern glibc
calls `clone3` first. Returning **`ENOSYS`** makes glibc fall back to `clone(2)`
— which *is* filterable. Docker does exactly this, for exactly this reason.

### The full denylist

36 entries, each with its reason: [Syscall reference](../05-reference/02-syscalls.md).

Verified from inside the sandbox by calling each through `ctypes` raw
`syscall()` — not a libc wrapper, which might refuse first for its own reasons:

```
blocked  unshare(CLONE_NEWNS)   blocked  setns       blocked  ptrace
blocked  bpf                    blocked  perf_event_open
blocked  keyctl                 blocked  open_by_handle_at
blocked  init_module            clone3 -> ENOSYS
FAILURES:none
```

---

## 6. `pivot_root`, not `chroot`

`chroot(2)` changes where `/` resolves. It is famously escapable:

```c
/* the classic chroot escape */
int fd = open(".", O_RDONLY);   /* a directory fd from OUTSIDE */
chroot("subdir");
fchdir(fd);                     /* walk back out through the fd */
for (i = 0; i < 1024; i++) chdir("..");
chroot(".");
```

`chroot` never promised to be a security boundary; it is a path-resolution
convenience.

[`pivot_root(2)`](https://man7.org/linux/man-pages/man2/pivot_root.2.html)
replaces the **mount namespace's root mount** outright. After moving the old
root aside and unmounting it, there is no mount in the namespace that refers to
the host filesystem. There is nothing left to walk back to.

```go
syscall.PivotRoot(c.Root, oldRoot)
syscall.Chdir("/")
syscall.Unmount("/.oldroot", syscall.MNT_DETACH)   // ← the essential step
os.Remove("/.oldroot")
syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
```

Omitting the `MNT_DETACH` unmount leaves the host filesystem mounted at
`/.oldroot` and the whole exercise is pointless.

### Mount details that are easy to get wrong

**Propagation.** Without making mounts private first, our mounts propagate back
to the host and — worse — our *unmounts* can tear down host mounts:

```go
syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, "")
```

**Read-only bind mounts need two calls.** A bind mount ignores flags on the
first `mount(2)`; read-only requires a second `MS_REMOUNT|MS_BIND|MS_RDONLY`.
Getting this wrong silently leaves the mount writable — a classic container bug:

```go
syscall.Mount(host, dst, "", syscall.MS_BIND|syscall.MS_REC, "")
syscall.Mount("", dst, "", syscall.MS_REMOUNT|syscall.MS_BIND|
    syscall.MS_RDONLY|syscall.MS_REC|syscall.MS_NOSUID|syscall.MS_NODEV, "")
```

**A synthesised `/etc`, not the host's.** Binding host `/etc` would expose
machine configuration and any secret an operator ever drops there to every
tenant's agents. We write a minimal `passwd`, `group`, `hosts`, `nsswitch.conf`
and an **empty `resolv.conf`** (a stale host one would leak internal DNS server
addresses — free reconnaissance), then bind a short allowlist of entries the
dynamic linker genuinely needs (`ld.so.cache`, `ld.so.conf.d`, `ssl`,
`alternatives`, `localtime`).

**`/dev` by bind mount, not `mknod`.** `mknod` is not permitted in a user
namespace, so each device node is bind-mounted individually. Note what is
**absent**: no `/dev/kmsg`, no `/dev/mem`, no block devices, no `/dev/kvm`.

**Merged-`/usr` distributions.** On modern Debian/Ubuntu, `/bin` is a symlink to
`usr/bin`. Bind-mounting through the symlink would produce a layout that differs
from the host's, breaking hard-coded interpreter paths like `#!/bin/sh`. The
code replicates the symlink instead.

---

## 7. rlimits — the per-process complement

cgroups bound the tree; rlimits bound each process, and `RLIMIT_NPROC` in
particular is checked at **fork time against the real uid**, giving a second,
independent fork-bomb stop:

| Limit | Value | Purpose |
|---|---|---|
| `RLIMIT_NOFILE` | 256 | fd exhaustion |
| `RLIMIT_FSIZE` | 64 MiB | filling the workspace |
| `RLIMIT_CORE` | **0** | a core dump of a compromised payload would land in the workspace and could contain material from elsewhere in the process |
| `RLIMIT_NPROC` | = `pids.max` | fork bombs, independently of cgroups |

---

## 8. The ordering — and why it is forced

This sequence is not stylistic. **Each step removes a privilege that the next
step needs.**

```
1. no_new_privs        ── must be first: precondition for unprivileged seccomp,
                          and it must precede the uid drop so no setuid binary
                          reachable afterwards can regain privilege
2. drop bounding set   ── needs CAP_SETPCAP, which we still hold as uid 0
3. setgroups/setgid/setuid ── needs CAP_SETGID/CAP_SETUID, which we still hold.
                          Changing away from uid 0 makes the kernel clear our
                          permitted and effective sets automatically
4. clear residual caps ── a no-op after (3); explicit rather than assumed
5. install seccomp     ── last, so the filter need not permit any of the
                          privilege-dropping syscalls above
6. execve
```

> **This cost me twenty minutes and is preserved in the code comments.** My first
> version cleared all capabilities at step 1. `setgroups` then returned `EPERM`,
> and glibc's response to a failed thread-wide setxid broadcast is to call
> `abort()`. The symptom was a `SIGABRT` with a Go stack trace pointing at
> `syscall.Setgroups` and no indication of the cause.
>
> **The general lesson:** a security control that fails loudly is a gift. This
> one failed as an opaque crash inside libc, and only running the test found it.

---

## 9. Output caps — a control people forget

Not a kernel mechanism, but it belongs in the same list because it defends a
real resource.

An agent that runs `yes` or `cat /dev/urandom` would push unbounded output
into (a) the event log, which is durable, and (b) **the model's context on every
subsequent turn**, which costs real money repeatedly (see
[context growth](../00-problem/01-what-is-an-agent.md#3a-every-turn-re-sends-everything)).

```go
type capWriter struct { buf []byte; cap int; truncated bool }
// absorbs and drops past the cap; never blocks or errors the payload
```

Two properties matter: it **never blocks the payload** (that would be a DoS on
ourselves) and truncation is **reported** to the model, so it does not silently
reason about a partial result.

---

## 10. Composing it — the whole jail

```
PARENT (executor)
  ├── create cgroups with limits                    ← BEFORE the child can run
  ├── clone(/proc/self/exe) with 6 namespace flags
  ├── write the config over a PIPE (never argv/env, so it never appears in ps)
  ├── put the child's PID into the cgroups
  └── release the child ────────────────────────────┐
                                                     │
CHILD (sandbox init, PID 1 in its namespace)  ◄──────┘
  ├── wait for the parent's release byte       ← unbounded until limits are on
  ├── sethostname
  ├── make all mounts MS_PRIVATE
  ├── mount a tmpfs as the new root
  ├── bind /usr /bin /sbin /lib read-only (or replicate symlinks)
  ├── synthesise /etc, mount /proc, /tmp, /dev, /dev/shm
  ├── bind the workspace at /work (nosuid, nodev)
  ├── pivot_root, unmount the old root, remount / read-only
  ├── set rlimits
  ├── no_new_privs → drop bounding set → setuid → clear caps → seccomp
  └── execve(payload)
```

### The release handshake

The child blocks on a pipe read until the parent has placed it in the cgroups.
Doing this in the other order leaves a window in which the payload runs
**unbounded**. It is a small detail with a large failure mode.

### Why re-exec `/proc/self/exe`

Setup must happen *inside* the new namespaces, which only exist after `clone`.
Go cannot safely run arbitrary code between fork and exec — the runtime's
threads and locks do not survive fork. Re-executing our own binary gives a clean,
single-threaded starting point inside the namespaces. Every container runtime
does this; runc calls it `nsexec`.

---

## 11. What this does **not** protect against

Honesty about the boundary is the point of drawing it.

| Threat | Status |
|---|---|
| **Kernel LPE** | **Not covered by namespaces.** They all share one kernel. This is why production uses gVisor. |
| Spectre-class side channels | Not covered. Needs per-tenant hardware. |
| Resource exhaustion *within* limits | By design — the limits are the contract. |
| A bug in this code | The reason there are 11 tests asserting each property separately. |

### The gVisor upgrade

[gVisor](https://gvisor.dev/docs/) (`runsc`) intercepts syscalls and services
them in a user-space kernel (the *Sentry*), written in Go. A Linux kernel
privilege-escalation bug is no longer directly reachable: an attacker must first
compromise the Sentry.

| | Cost |
|---|---|
| CPU overhead | ~2–15%, [worse for syscall- and I/O-heavy workloads](https://gvisor.dev/docs/architecture_guide/performance/) |
| Cold start | ~150–300 ms vs ~8 ms here |
| Compatibility | some syscalls unimplemented |

Enabled via `RuntimeClass` in
[`deploy/k8s/base/runtimeclass.yaml`](../../deploy/k8s/base/runtimeclass.yaml).
If the RuntimeClass is absent, sandbox pods stay `Pending` with a clear message
— deliberately. **Failing to start a sandbox is correct; silently falling back
to `runc` is not.**

---

## 12. Driver parity table

The same `Driver` interface, three implementations, **not** equal security.
`Result.Driver` records which one ran so nothing can overstate its boundary.

| Control | `namespace` | `docker` | `kubernetes` |
|---|---|---|---|
| Kernel isolation | shared | shared (or `--runtime=runsc`) | **gVisor via RuntimeClass** |
| Network | empty netns | `--network none` | default-deny NetworkPolicy |
| Root filesystem | tmpfs + `MS_RDONLY` | `--read-only` | `readOnlyRootFilesystem: true` |
| Capabilities | bounding set emptied | `--cap-drop ALL` | `capabilities.drop: [ALL]` |
| `no_new_privs` | `prctl` | `--security-opt no-new-privileges` | `allowPrivilegeEscalation: false` |
| Non-root | userns → host uid 100999 | `--user 1000:1000` | `runAsNonRoot` + `runAsUser` |
| Memory / CPU / PIDs | cgroup direct | `--memory`/`--cpus`/`--pids-limit` | `resources.limits` (Guaranteed QoS) |
| seccomp | hand-written 36-entry filter | Docker default profile | `RuntimeDefault` |
| API credential | n/a | n/a | `automountServiceAccountToken: false` |
| Node isolation | none | none | tainted node pool |
| **Cold start** | **8.4 ms** | ~150–400 ms | ~1–3 s |

---

**Next:** [Durable execution](02-durable-execution.md) — how a run survives the
death of everything running it.

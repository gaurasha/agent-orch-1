# Syscall denylist reference

> **Prerequisite:** [Linux isolation](../01-concepts/01-linux-isolation.md)
> **Source:** [`internal/sandbox/seccomp_linux.go`](../../backend/internal/sandbox/seccomp_linux.go)
>
> *This table is generated from the source. Numbers are linux/amd64.*

The sandbox installs a seccomp-BPF filter that denies **36** syscalls. Each entry
is a primitive an escape actually needs, not an arbitrary restriction.

All denied calls return `EPERM`, **except `clone3`, which returns `ENOSYS`** — see
the note below.

`EPERM` rather than `SIGSYS` is deliberate: the payload gets a legible error it
can report, and the agent's transcript then shows *what it tried*, which is
valuable signal for the operator.

---

## Mount and filesystem escape

| # | Syscall | Why it is denied |
|---|---|---|
| 165 | `mount` | re-mounting the rootfs read-write, or mounting host paths |
| 166 | `umount2` | unmounting our read-only protections |
| 155 | `pivot_root` | escaping the mount namespace |
| 161 | `chroot` | confusing path resolution to break out |
| 303 | `name_to_handle_at` | filesystem handle escape primitive |
| 304 | `open_by_handle_at` | the classic bind-mount escape primitive |

## Process inspection and injection

| # | Syscall | Why it is denied |
|---|---|---|
| 101 | `ptrace` | reading memory of, or injecting into, a sibling process |
| 310 | `process_vm_readv` | reading another process's memory directly |
| 311 | `process_vm_writev` | writing another process's memory directly |

## Kernel code loading

| # | Syscall | Why it is denied |
|---|---|---|
| 175 | `init_module` | loading kernel code |
| 313 | `finit_module` | loading kernel code from an fd |
| 176 | `delete_module` | unloading kernel code |
| 246 | `kexec_load` | replacing the running kernel |
| 320 | `kexec_file_load` | replacing the running kernel |

## Kernel attack surface

| # | Syscall | Why it is denied |
|---|---|---|
| 321 | `bpf` | loading eBPF programs; a recurring source of LPEs |
| 298 | `perf_event_open` | a recurring source of LPEs and a side channel |
| 323 | `userfaultfd` | widening kernel race windows during exploitation |
| 425 | `io_uring_setup` | large kernel attack surface that also bypasses seccomp |
| 426 | `io_uring_enter` | large kernel attack surface that also bypasses seccomp |
| 427 | `io_uring_register` | large kernel attack surface that also bypasses seccomp |

## Namespace manipulation

| # | Syscall | Why it is denied |
|---|---|---|
| 272 | `unshare` | creating fresh namespaces to re-gain capabilities |
| 308 | `setns` | entering another process's namespaces |
| 435 | `clone3` | seccomp cannot inspect its struct-pointer args; ENOSYS forces the filterable clone(2) fallback |

## Kernel keyring

| # | Syscall | Why it is denied |
|---|---|---|
| 248 | `add_key` | kernel keyring access |
| 249 | `request_key` | kernel keyring access |
| 250 | `keyctl` | kernel keyring access |

## Host-wide state

| # | Syscall | Why it is denied |
|---|---|---|
| 169 | `reboot` | denial of service against the node |
| 167 | `swapon` | host-wide resource manipulation |
| 168 | `swapoff` | host-wide resource manipulation |
| 164 | `settimeofday` | host-wide clock manipulation |
| 227 | `clock_settime` | host-wide clock manipulation |
| 159 | `adjtimex` | host-wide clock manipulation |
| 305 | `clock_adjtime` | host-wide clock manipulation |
| 163 | `acct` | host-wide accounting manipulation |
| 179 | `quotactl` | host-wide quota manipulation |

## Exploitation aids

| # | Syscall | Why it is denied |
|---|---|---|
| 135 | `personality` | disabling ASLR to make exploitation easier |

---

## The `clone3` special case

`clone3` returns **`ENOSYS`**, not `EPERM`. Two reasons, both essential:

1. **seccomp cannot inspect its arguments.** `clone3` takes a pointer to a
   `struct clone_args`, so the flags live in memory the BPF filter cannot read.
   A `clone3` with `CLONE_NEWUSER` would sail straight past a filter that only
   checks `clone`.
2. **`EPERM` would break everything.** Modern glibc calls `clone3` first.
   `ENOSYS` makes it fall back to `clone(2)` — which *is* filterable, because its
   flags are in a register.

Docker does exactly this, for exactly this reason.

---

## The guards that run before the denylist

Two checks precede every comparison, and both are load-bearing:

### Architecture validation

```
[0]  LD  W ABS  offset 4        ; A = seccomp_data.arch
[1]  JEQ AUDIT_ARCH_X86_64, +1  ; match? skip the kill
[2]  RET KILL_PROCESS
```

Without this, a 32-bit process uses **different syscall numbering** — on i386,
`165` is `nfsservctl`, not `mount`. Every entry in the table above would be
checking the wrong call. This is a classic seccomp bypass.

### x32 ABI rejection

```
[4]  JGE 0x40000000, +0, +1     ; x32 bit set?
[5]  RET KILL_PROCESS
```

The x32 ABI uses the same registers as x86-64 but ORs syscall numbers with
`0x40000000`. A filter comparing bare numbers misses every x32 call.

---

## Why a denylist rather than an allowlist

An allowlist is strictly stronger, and is what
[Docker ships](https://github.com/moby/moby/blob/master/profiles/seccomp/default.json)
(~350 permitted syscalls).

| | Allowlist | **Denylist (here)** |
|---|---|---|
| Unknown syscalls | blocked | **permitted** |
| Breaks real workloads | frequently — Python's `importlib`, glibc NSS, CPU probing | rarely |
| Review burden | enumerate everything benign | enumerate what escapes need |

The deciding argument: in the production design seccomp is **layer 7 of 8** and
is *superseded* by gVisor, which implements the syscall ABI in user space and
makes the host syscall surface largely irrelevant.

> **If gVisor were removed from the design, this should become an allowlist.**

---

## Verified

`TestSandbox_EscapeSyscallsAreBlocked` probes each through **raw `syscall()` via
`ctypes`** — not a libc wrapper, which might refuse first for its own reasons and
make the test a tautology:

```
blocked  unshare(CLONE_NEWNS)   blocked  setns       blocked  ptrace
blocked  bpf                    blocked  perf_event_open
blocked  keyctl                 blocked  open_by_handle_at
blocked  init_module            clone3 -> ENOSYS
FAILURES:none
```

---

## What this does **not** prove

It is a denylist. **An unlisted dangerous syscall is permitted.** New syscalls
are added to Linux regularly, and each is allowed here until someone adds it.

That is precisely why gVisor is the production answer: it moves the entire
syscall surface into user space, so the host kernel's syscall table stops being
the boundary.

---

## References

- [`seccomp_filter.rst`](https://www.kernel.org/doc/html/latest/userspace-api/seccomp_filter.html)
- [`seccomp(2)`](https://man7.org/linux/man-pages/man2/seccomp.2.html)
- [Docker's default seccomp profile](https://github.com/moby/moby/blob/master/profiles/seccomp/default.json)
- [Linux syscall table](https://filippo.io/linux-syscall-table/)

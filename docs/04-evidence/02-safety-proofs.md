# Safety proofs

> **Prerequisite:** [Linux isolation](../01-concepts/01-linux-isolation.md)
> **Read next:** [Bugs found](03-bugs-found.md)
> **Source:** [`internal/sandbox/sandbox_linux_test.go`](../../backend/internal/sandbox/sandbox_linux_test.go)

For each safety claim: what is asserted, how, what would make the test lie, and
what it does **not** prove.

---

## What makes a safety test worth having

Three properties, in order of how often they are missing:

1. **A negative control.** "The connection failed" proves nothing unless you also
   show it *could* have succeeded.
2. **Assert the mechanism, not the outcome.** "The fork bomb did not hang" is
   compatible with the fork bomb never forking.
3. **Probe the real primitive.** A libc wrapper may refuse for its own reasons,
   before your control ever fires.

Every test below is written against those three, because the first version of
this suite failed all of them at least once.

---

## S1 — A sandbox has no network access

**Claim:** model-authored code cannot reach the network, including the cloud
metadata service.

```python
for host, port in [("169.254.169.254", 80), ("1.1.1.1", 443), ("8.8.8.8", 53)]:
    s = socket.socket(); s.settimeout(3)
    try:
        s.connect((host, port)); print("REACHED"); sys.exit(1)
    except OSError as e:
        print("denied %s:%d %s" % (host, port, errno.errorcode.get(e.errno)))
print("interfaces=" + …); print("routes=%d" % …)
```

```
denied 169.254.169.254:80  ENETUNREACH
denied 1.1.1.1:443         ENETUNREACH
denied 8.8.8.8:53          ENETUNREACH
interfaces=['lo']  routes=0
ALL_DENIED
```

### Three things make this meaningful

**A real `connect(2)`**, not a shell redirection.

**The specific errno.** `ENETUNREACH` means *no route exists*. `ECONNREFUSED`
would mean something answered; `ETIMEDOUT` would mean a firewall dropped it.
Only `ENETUNREACH` is evidence of an empty network namespace.

**A negative control:**

```go
out, _ := exec.CommandContext(ctx, py, "-c", `…connect(("1.1.1.1",443))…`).CombinedOutput()
if !strings.Contains(string(out), "HOST_OK") {
    t.Skipf("negative control failed: this host has no outbound network either (%s), "+
        "so the isolation result above is not meaningful", …)
}
```

The identical probe runs on the host. If the host is also offline the test
**skips** rather than passing, because it would otherwise prove only that the
machine has no network.

> **This test used to pass for the wrong reason.** The first version used
> `echo > /dev/tcp/169.254.169.254/80`, a **bash**ism — and `/bin/sh` in the
> sandbox is dash. It was failing on *"Directory nonexistent"*, not on network
> isolation, and would have passed with **full network access**.

**Does not prove:** anything about the `NetworkProxy` profile used by the broker
sandbox, which deliberately has egress.

---

## S2 — The host filesystem is invisible

```go
secretPath := filepath.Join(t.TempDir(), "other-tenant-secret.txt")
os.WriteFile(secretPath, []byte("TENANT_B_PRIVATE_DATA"), 0o600)

res := run(t, d, sandbox.Spec{ Argv: []string{"/bin/sh", "-c",
    "cat " + secretPath + " 2>&1; ls /home 2>&1; ls /"} })

if strings.Contains(res.Stdout, "TENANT_B_PRIVATE_DATA") { t.Fatal(…) }
if strings.Contains(res.Stdout, "/home/user")            { t.Fatal(…) }
```

A file standing in for another tenant's data is planted at a **known absolute
path**, and the sandbox is asked for it by that exact path — so this is not
"could not find it", it is "the path does not resolve to it".

---

## S3 — The root filesystem is read-only, except `/work` and `/tmp`

```sh
touch /pwned         || echo "DENIED /"
touch /usr/bin/pwned || echo "DENIED /usr/bin"
touch /etc/pwned     || echo "DENIED /etc"
echo ok > /work/allowed && echo "ALLOWED /work"
echo ok > /tmp/allowed  && echo "ALLOWED /tmp"
```

All five assertions are required. Testing only the denials would pass on a
sandbox where **nothing** is writable — which is broken, not secure.

---

## S4 — The workspace is the only thing that persists

Writes a file on the host, has the sandbox transform it, and asserts the result
is visible on the host afterwards. Proves the bind mount works in both
directions, which is what makes the two-sandbox credential pattern possible.

---

## S5 — Unprivileged, with no capabilities

```
uid=1000
CapEff:     0000000000000000
CapBnd:     0000000000000000
NoNewPrivs: 1
Seccomp:    2
```

| Assertion | Meaning |
|---|---|
| `uid=1000` | Not root inside; host uid 100999, which owns nothing |
| `CapEff = 0` | No capability usable **now** |
| **`CapBnd = 0`** | **No `execve` in this tree can ever acquire one** — the stronger claim |
| `NoNewPrivs = 1` | setuid binaries are neutered |
| `Seccomp = 2` | `SECCOMP_MODE_FILTER` is active |

Read from `/proc/self/status` **inside** the sandbox — the kernel's own view,
not our configuration.

---

## S6 — Escape syscalls are blocked

```python
libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
def call(nr, *args):
    ctypes.set_errno(0)
    r = libc.syscall(ctypes.c_long(nr), *[ctypes.c_long(a) for a in args])
    return r, ctypes.get_errno()
```

```
blocked  unshare(CLONE_NEWNS)   blocked  setns       blocked  ptrace
blocked  bpf                    blocked  perf_event_open
blocked  keyctl                 blocked  open_by_handle_at
blocked  init_module            clone3 -> ENOSYS
FAILURES:none
```

**Raw `syscall()` through `ctypes`, not libc wrappers** — a wrapper might refuse
for its own reasons before seccomp ever fires, which would make the test a
tautology.

The `clone3` case asserts **`ENOSYS`, not `EPERM`**: seccomp cannot inspect its
struct-pointer arguments, so `ENOSYS` forces glibc to fall back to the filterable
`clone(2)`. `EPERM` would break every glibc-linked binary.

**Does not prove:** the denylist is complete. It is a denylist; an unlisted
dangerous syscall is permitted. That is why gVisor is the production answer —
see [Linux isolation §5](../01-concepts/01-linux-isolation.md).

---

## S7 — Wall-clock timeout is enforced

```go
Argv: []string{"/bin/sh", "-c", "trap '' TERM; while :; do :; done"},
Limits: sandbox.Limits{Wall: 2 * time.Second},
```

**`trap '' TERM` is the point.** The payload explicitly ignores graceful
shutdown, so this proves the kill is not cooperative. The deadline is held by
the *parent*, which the payload cannot reach.

---

## S8 — Memory limit is enforced

```
memory limit held: oom_killed=true exit=137
```

Allocates 1.6 GiB against a 64 MiB limit, **touching each page** so it is
resident — a limit on virtual memory would not catch it.

`oom_killed` comes from the cgroup's `oom_kill` counter, not from the exit code.
Exit 137 alone is ambiguous (any SIGKILL), and an operator needs to distinguish
*"this agent is misbehaving"* from *"this agent is under-provisioned"*.

> `memory.failcnt` was the original signal and is wrong: it counts times the
> limit was *reached*, which also happens when reclaim succeeds and nothing is
> killed.

---

## S9 — Fork bombs are contained

```python
while children < 5000:
    pid = os.fork()
    if pid == 0:
        try: os.pause()
        finally: os._exit(0)
    children += 1
except OSError as e:
    print("FORK_REFUSED after %d children: %s" % (children, e.strerror))
```

```
fork bomb contained: kernel refused fork after 23 children (pids.max=24)
```

```go
if got > pidLimit+8 {
    t.Fatalf("forked %d children against a pids limit of %d; the cgroup is not binding", got, pidLimit)
}
```

**Asserting the count is what makes this real.** "The test finished" is
compatible with being stopped by a system-wide ceiling, or by the program never
forking. 23 against a limit of 24 proves **the cgroup** bound it.

> The first version used a shell fork bomb, `f(){ f|f & }; f; wait`. It "passed"
> in 27 ms with exit 0 — because it was a **dash syntax error** and never forked
> at all.

---

## S10 — Output is capped

`yes` against a 32 KiB cap. Asserts `Truncated` is set **and** that stdout is
under 64 KiB — a cap that reports truncation but does not enforce it would pass
the first check alone.

Truncation is reported to the model so it does not silently reason about a
partial result. See
[why this is a real control](../01-concepts/01-linux-isolation.md#9-output-caps--a-control-people-forget).

---

## S11 — Cold start is measured

10 runs, mean and worst reported, with a 2 s ceiling so that a regression from
milliseconds to seconds fails the build rather than quietly invalidating the
per-call sandbox model. See [Benchmarks §1](01-benchmarks.md).

---

## S12 — Credentials never reach the agent

Asserted in `make demo`, from **both sides simultaneously**:

```
-> agent's own sandbox contains no credential           ok
   the agent ran `env`, read /proc/self/environ, and grepped for token/secret/key
-> credentialed CLI call succeeded through the broker sandbox   ok
   Created pull request #42: Release v1.4.0
-> the third-party API verified a genuine minted credential     ok
   1 verified credential presentation(s); fingerprints: aot_acme.c...CGD1rQ
-> no credential material anywhere in the event log      ok
```

**All four are required.** "No credential in the environment" is trivially true
if the call also failed. "PR created" is trivially true if we handed the agent a
token. The third assertion is what makes it falsifiable: the fake GitHub API
**rejects** missing, malformed, expired, revoked and wrong-tenant tokens.

The fourth scans every event payload for the `aot_` prefix, because a credential
in the event log would be **durable** and replayed into the model's context on
every subsequent turn.

---

## S13 — The `Secret` type cannot be printed

```
fmt.Sprint · fmt.Sprintf("%v|%s|%q|%#v|%x") · log output
json.Marshal(wrapper) · error wrapping · String()
```

Each asserted to contain `[REDACTED]` and **not** the plaintext, including when
the secret is nested inside a struct, a map and a slice — the realistic accident
of logging a whole request object.

Plus the call-site guard:

```
plaintext credential use sites: 3 across 2 files
```

The test asserts **exactly 3** and names each in its doc comment, so a fourth is
a review event rather than a silent change.

---

## S14 — Prompt injection is contained

```
denied: http.post   -> tool "http.post" is not in this agent's granted tool set
denied: github.cli  -> tool "github.cli" is not in this agent's granted tool set
denied: http.get    -> tool "http.get" is not in this agent's granted tool set
-> every exfiltration attempt was refused by policy      ok  (3 denied)
-> cloud metadata endpoint unreachable from the sandbox  ok
-> run still finished cleanly rather than crashing       ok  state=SUCCEEDED
```

Four attack routes. **None was stopped by detecting the injection** — each was
stopped by the agent not having the capability. Attempt 3 (`exec.bash` → `curl`
the metadata endpoint) *was* authorized and executed; containment came entirely
from the empty network namespace.

The last assertion matters: a contained attack must not be an outage.

---

## S15 — Budgets are enforced

```
-> poison agent was stopped by the platform      ok  state=FAILED
-> stopped by an enforced budget, not by chance  ok  reason="tool-call budget exhausted (5/5)"
-> bounded cost                                  ok  $0.19467 of a $0.50 cap
```

The second assertion is the one that matters. A run that *happens* to finish is
not evidence of a working control; the reason string must name the budget.

---

## S16 — Tenancy holds

```
globex reading acme run -> HTTP 404      ← not 403; existence is not confirmed
no key at all           -> HTTP 401
```

---

## S17 — The audit chain is tamper-evident

```go
tampered[1].ArgsRedacted = map[string]any{"script": "rm -rf /"}
if store.VerifyChain(tampered) == nil { t.Fatal("tampering went undetected") }
```

Forges a record the way someone covering their tracks would, and asserts
detection. Run against **both** stores — which is how the timestamp-precision bug
was found.

---

## Coverage summary

| Requirement | Tests |
|---|---|
| Sandboxed execution | S1–S11 |
| Credentials never in model context | S12, S13 |
| Cannot call an ungranted tool | S14 |
| Durability | [§6 Benchmarks](01-benchmarks.md#6-durability) |
| Attributable to agent/tenant/user | S17 + `make demo` |
| Scale | [§2–§4 Benchmarks](01-benchmarks.md) |

## What is **not** proven

| Gap | Honest status |
|---|---|
| gVisor's boundary | Not tested here — no runsc on this host |
| The seccomp denylist is complete | It is a denylist. Unlisted syscalls are permitted |
| NetworkPolicy enforcement | Requires a Calico-backed cluster; `kind-test.sh` checks the policies **exist** |
| Resistance to a determined attacker | No red-team exercise was performed |
| Multi-replica fairness | The limiter is per-process |

---

**Next:** [Bugs found](03-bugs-found.md).

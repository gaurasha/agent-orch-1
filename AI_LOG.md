# AI usage log

**Tools used:** Claude (Opus 5) as the primary coding agent, driving the whole
build in one long session; `go test`, `kubeconform` and Playwright as the
things that actually decided whether output was correct. No other assistants.

**How I worked:** I treated the assistant's output the way I would treat a
capable engineer's pull request — useful by default, wrong in specific,
predictable places, and not to be merged on the strength of it looking right.
The discipline that made the difference was refusing to accept any security
claim that was not demonstrated by a test that could fail.

---

## The entries

### 1. Sandbox isolation approach — **overrode**

| | |
|---|---|
| **Asked** | "Propose an isolation approach for running arbitrary model-authored code, multi-tenant." |
| **Produced** | Docker with a seccomp profile, `--read-only`, `--cap-drop ALL`, `--network none`. Correct flags, sensible defaults. |
| **What I did** | **Rejected the framing, kept the flags.** Two problems. (a) A shared kernel is not enough for untrusted code in a multi-tenant cluster — [CVE-2022-0185](https://nvd.nist.gov/vuln/detail/CVE-2022-0185) and [CVE-2022-0492](https://nvd.nist.gov/vuln/detail/CVE-2022-0492) are container escapes via kernel bugs, and a model that has read attacker-controlled text is exactly the adversary who will try. I chose gVisor for production and wrote down the cold-start and CPU cost rather than hiding it. (b) More importantly for the PoC, `docker run` would have made the security properties *invisible* — they would have been Docker's defaults, not decisions in my diff. I implemented the jail directly from namespaces, cgroups, `pivot_root`, capabilities and seccomp so each property is separately reviewable and separately testable. That is also what let the demo run on a machine with no Docker daemon at all. |

**The defence:** "Docker with seccomp" is the right answer to "how do I isolate a
process". It is not the right answer to "what boundary do I want between two
tenants' untrusted code", because it does not name the boundary — the kernel —
that is being trusted.

---

### 2. The seccomp filter — **overrode a subtle detail**

| | |
|---|---|
| **Asked** | "Write a seccomp-BPF denylist for container escape primitives." |
| **Produced** | A reasonable list — `mount`, `ptrace`, `bpf`, `init_module`, `kexec_load` — all returning `EPERM`. |
| **What I did** | **Added the syscalls it missed and changed one return value.** Missing: `io_uring_*` (a large kernel surface that *also* bypasses seccomp for the operations it performs), `userfaultfd` (widens kernel race windows during exploitation), `open_by_handle_at` (the classic bind-mount escape), `process_vm_readv/writev`. Changed: `clone3` must return **`ENOSYS`, not `EPERM`** — seccomp cannot inspect its arguments because they sit behind a struct pointer, so it could create namespaces unseen; `ENOSYS` makes modern glibc fall back to `clone(2)`, which *can* be filtered. `EPERM` would have broken every glibc-linked binary in the sandbox. Docker does the same thing for the same reason. |

**The defence:** a denylist is only as good as the author's model of what an
escape *needs*. The additions are not trivia — `io_uring` and `open_by_handle_at`
are load-bearing. And the `clone3` return value is a case where the plausible
answer is functionally broken.

---

### 3. The privilege-drop ordering — **AI was wrong, the failure was silent**

| | |
|---|---|
| **Asked** | "Write the child init: build the rootfs, drop privileges, exec the payload." |
| **Produced** | An ordering of: drop capabilities → `no_new_privs` → seccomp → `setgid`/`setuid` → `execve`. It reads perfectly sensibly — drop everything as early as possible. |
| **What I did** | **The test crashed with `SIGABRT` and no useful message.** `setgroups`/`setgid`/`setuid` *need* `CAP_SETGID`/`CAP_SETUID`, which had just been cleared. The syscall returned `EPERM`, and glibc's response to a failed thread-wide uid change is `abort()`. Corrected to the ordering real runtimes use: `no_new_privs` → drop the capability **bounding** set (needs `CAP_SETPCAP`, still held) → `setgid`/`setuid` (needs `CAP_SETUID`, still held) → clear residual capabilities → seccomp → `execve`. Each step removes a privilege the next step needs, so the order is forced. |

**The defence:** this is the entry I would most want to be asked about. The
suggested code was not sloppy — it followed the correct instinct (drop privilege
early) and produced a crash with no obvious cause. I found it because I ran the
test, not because I reviewed the code harder. **A security control that fails
loudly is a gift; this one failed as a `SIGABRT` inside glibc.**

---

### 4. The first network-isolation test — **overrode my own test, not the AI's code**

| | |
|---|---|
| **Asked** | "Write a test proving a sandboxed script cannot reach the cloud metadata endpoint." |
| **Produced** | A shell test using `echo > /dev/tcp/169.254.169.254/80`, asserting failure. **It passed.** |
| **What I did** | **Rejected it — it passed for the wrong reason.** `/dev/tcp` is a *bash* feature and `/bin/sh` in the sandbox is dash. The command was failing with "Directory nonexistent", not with a network error. The test would have passed on a sandbox with **full** network access. Replaced with a Python `socket.connect()` that asserts the specific errno (`ENETUNREACH`), and — the part that actually matters — **added a negative control** that runs the identical probe on the host and skips the test if the host has no network either. Without the control, the test proves only that this machine is offline. |

**The defence:** the brief warns against tests that look green without
demonstrating anything, and this is exactly that. The lesson generalises: a
safety test needs a **negative control**, or you are asserting that something
failed without establishing that it could ever have succeeded.

---

### 5. Store limit clamping — **found by a test, not by review**

| | |
|---|---|
| **Asked** | "Add pagination bounds to the list queries." |
| **Produced** | `if limit <= 0 || limit > 1000 { limit = 200 }` — the standard-looking idiom. |
| **What I did** | Left it in. **The 500-agent load test then stalled at exactly 200 completions.** A caller asking for 2000 was silently given 200 — the *default*, not the maximum — so the polling loop could only ever see the 200 newest runs. Fixed to clamp to the maximum, and added a regression test (`TestListRuns_OverMaxLimitReturnsMaximumNotDefault`) that asks for 9999 of 260 rows and asserts it gets 260. |

**The defence:** I did not catch this by reading it, and neither did the
assistant. Silently shrinking a request to *less than the maximum* is a trap
that hides paging bugs — the caller believes it has seen everything. It surfaced
only because a load test asserted an exact total.

---

### 6. Test isolation across parallel packages — **caught by running the whole suite**

| | |
|---|---|
| **Asked** | "Set up the Postgres-backed test packages." |
| **Produced** | Each package reads `AGENTORCH_TEST_DSN` and truncates the tables on entry. Every package passed when run on its own. |
| **What I did** | **Ran `go test ./...` and got two failures that had passed minutes earlier.** `go test` runs packages *concurrently*, so `store`, `durability` and `load` were truncating the same database underneath each other — producing failures that looked like race conditions in the lease queue and vanished when you ran the package alone. The tempting fix is `-p 1`, which serialises packages and hides it. I fixed the isolation instead: each package gets a private Postgres schema via the connection's `search_path` (`internal/testsupport`), so they still run in parallel and still start clean. |

**The defence:** "flaky test" is almost never a real diagnosis. This one would
have been blamed on the lease queue — the most concurrency-sensitive code in the
system — and I would have spent hours hardening code that was already correct.

---

### 7. Where AI was clearly better than I would have been alone

**The Postgres/Go timestamp-precision bug in the audit hash chain.**

The chain verified perfectly in memory and failed against Postgres, every time.
Go's `time.Time` carries nanoseconds; `timestamptz` stores microseconds; the
timestamp is inside the hash, so a round trip changed it. I would have got there
eventually, but I would have started by suspecting my canonical-JSON encoder,
because that is where hashing bugs usually live — and I would have lost an hour
in the wrong file. The precision mismatch was identified immediately from the
symptom "verifies before write, fails after read".

The second, larger win was **structural**: the suggestion to run one conformance
suite against *both* store implementations rather than testing them separately.
That decision is what surfaced this bug and the `pq.Array(nil)` → SQL `NULL`
bug (where `cardinality(NULL)=0` evaluates to `NULL`, not `true`, so the lease
query silently matched nothing). Both were Postgres-only. Tested separately,
the in-memory suite would have been green and I would have shipped a broken
lease queue with a passing test suite.

---

### 8. What I deliberately did without AI

**The three hardest decisions, the failure-mode table, and what to cut.**

Specifically: the argument that a run is a row rather than a pod; the position
that prompt injection must be *contained* rather than detected; the decision to
draw the credential boundary at a second sandbox rather than a proxy; and the
list in "What I cut".

I wrote these first, on my own, before asking for any implementation. Two
reasons. First, these are the parts the brief says it is actually grading, and
they are judgement calls where an assistant's answer would be a well-informed
average of what other people have written — which is exactly wrong for "the
decision *you* found hardest". Second, and more practically: if I had not fixed
the architecture first, I would have got a competent implementation of whatever
architecture the assistant assumed, and I would have spent the session reviewing
code instead of deciding anything.

I also wrote every **reversal condition** myself. "I would reverse this if X"
requires knowing which constraint you are actually trading against, and that is
not something to outsource.

---

## Two things I would tell the next person

**1. Make the assistant produce the test before the implementation.** Every bug
above was caught by a test that could fail, and every one was invisible to code
review — including to mine. The `SIGABRT` ordering bug, the false-positive
network test, the timestamp precision mismatch, the limit clamp: four real bugs,
zero found by reading.

**2. The assistant is excellent at the layer where a correct answer is
*known*** — syscall numbers, BPF encoding, mount flags, `FOR UPDATE SKIP LOCKED`
semantics, Kubernetes PodSpec fields. It is weakest exactly where this brief is
strongest: deciding *which* constraint to trade away, and being honest about
what that costs. Those two facts are complementary, which is why this went well.

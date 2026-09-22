# Bugs found, and what each one teaches

> **Prerequisite:** [Safety proofs](02-safety-proofs.md)
> **Read next:** [Glossary](../05-reference/01-glossary.md)

Eight real bugs from building this. Every one was found by **running something**,
not by reading code — including the ones I wrote the code for and reviewed
myself.

They are listed because the pattern is more useful than any individual fix.

---

## B1 — Privilege-drop ordering: `SIGABRT` from inside glibc

**Symptom**

```
child error: SIGABRT: abort
signal arrived during cgo execution
syscall.Setgroups(…)
  sandbox.runInit()
```

A Go stack trace pointing at `syscall.Setgroups` with no indication of why.

**Cause**

The ordering was: drop capabilities → `no_new_privs` → seccomp → `setgid`/`setuid`
→ `execve`. That reads perfectly sensibly — drop privilege as early as possible.

But `setgroups`/`setgid`/`setuid` **need** `CAP_SETGID`/`CAP_SETUID`, which had
just been cleared. The syscall returned `EPERM`, and glibc's response to a failed
thread-wide setxid broadcast is to call `abort()`.

**Fix**

The ordering real container runtimes use, where each step removes a privilege
the *next* step needs:

```
no_new_privs → drop bounding set (CAP_SETPCAP) → setuid (CAP_SETUID)
  → clear residual caps → seccomp → execve
```

**What it teaches**

> A security control that fails loudly is a gift. This one failed as an opaque
> crash inside libc.

I did not find it by reviewing the code more carefully — the code looked right,
and the instinct behind it *was* right. I found it by running the test. The
ordering constraint is now a 15-line comment in
[`init_linux.go`](../../backend/internal/sandbox/init_linux.go) precisely because
it is not self-evident.

---

## B2 — A network test that passed for the wrong reason

**Symptom**

None. **The test passed.**

```sh
timeout 3 sh -c "echo > /dev/tcp/169.254.169.254/80" || echo "DENIED"
```

**Cause**

`/dev/tcp` is a **bash** feature. `/bin/sh` in the sandbox is dash. The command
failed with *"Directory nonexistent"* — it was never a network operation at all.

**The test would have passed on a sandbox with full network access.**

**Fix**

A real `socket.connect()` asserting the specific errno, plus — the part that
actually matters — **a negative control** that runs the identical probe on the
host and skips if the host is also offline.

**What it teaches**

> A safety test needs a negative control, or you are asserting that something
> failed without establishing it could ever have succeeded.

This is the bug I would most want to be asked about. It is exactly the class the
brief warns against: green, and meaningless.

---

## B3 — A fork-bomb test that never forked

**Symptom**

```
fork bomb contained: exit=0 timed_out=false duration=27ms
```

Passing. 27 milliseconds. Exit 0.

**Cause**

```sh
f(){ f|f & }; f; wait
```

A dash syntax error. The bomb never forked.

**Fix**

Explicit `os.fork()` in a loop, and — critically — assert the **count**:

```go
if got > pidLimit+8 {
    t.Fatalf("forked %d children against a pids limit of %d; the cgroup is not binding", got, pidLimit)
}
```

```
fork bomb contained: kernel refused fork after 23 children (pids.max=24)
```

**What it teaches**

> Assert the mechanism, not the outcome. "It finished" is compatible with "it
> never started".

23 against a limit of 24 proves the **cgroup** stopped it, not a system-wide
ceiling and not a broken script.

---

## B4 — `cardinality(NULL) = 0` is `NULL`, not `true`

**Symptom**

Every Postgres lease test failed with `acquire: not found`. The in-memory store
was green.

**Cause**

```go
pq.Array(nil)   // → SQL NULL
```

```sql
AND (cardinality($1::text[])=0 OR tenant_id = ANY($1))
```

In SQL, `cardinality(NULL)` is `NULL`; `NULL = 0` is `NULL`; `NULL OR NULL` is
`NULL`. The predicate was never `true`, so **no row ever matched** when the
tenant filter was empty.

**Fix**

```sql
AND ($1::text[] IS NULL OR cardinality($1::text[])=0 OR tenant_id = ANY($1))
```

**What it teaches**

> SQL's three-valued logic is not Go's two-valued logic, and a `NULL` predicate
> is silently false.

Found **only** because the conformance suite runs every test against both stores.
Tested separately, the fake would have been green while the real lease queue
matched nothing.

---

## B5 — The audit chain never verified on Postgres

**Symptom**

```
freshly written chain must verify:
  audit record seq 1 was modified after it was written (hash mismatch)
```

Perfect in memory. Failing **every single time** on Postgres.

**Cause**

Go's `time.Time` carries **nanoseconds**. Postgres `timestamptz` stores
**microseconds**. The timestamp is inside the hash (so backdating breaks the
chain), so:

```
hash with ns → INSERT → stored truncated to µs → SELECT → rehash with µs → MISMATCH
```

**Fix**

```go
rec.TS = time.Now().UTC().Truncate(time.Microsecond)
```

before hashing **and** storing.

**What it teaches**

> A tamper-evident log that *always* reports tampering is worse than no log at
> all — it trains operators to ignore the alarm, and then the real alarm is
> ignored too.

This is also the one place an assistant was clearly faster than I would have
been: I would have started by suspecting my canonical-JSON encoder, because that
is where hashing bugs usually live, and lost an hour in the wrong file.

---

## B6 — A run stuck in `RUNNING` forever

**Symptom**

The durability test hung for 45 seconds and failed:

```
run … did not reach [SUCCEEDED FAILED] within 45s (state=RUNNING, 1 events)
```

**Cause**

When a worker stopped mid-step, cleanup called a bare `ReleaseLease`, which nulls
the lease **but leaves `state='RUNNING'`**. Then:

- the dispatcher selects `state='QUEUED'` → never sees it
- the reaper selects `lease_expires_at < now()` → it is `NULL` → never sees it

**The run was invisible to both and stuck forever.**

**Fix**

A fenced `YieldRun` that returns the run to `QUEUED` in one statement:

```sql
UPDATE runs SET state='QUEUED', lease_owner=NULL, lease_expires_at=NULL, status_reason=$3
WHERE id=$1 AND lease_owner=$2 AND state='RUNNING'
```

plus a belt-and-braces reaper clause for ownerless `RUNNING` rows.

**What it teaches**

> When you have two independent queries that are each supposed to find work, ask
> what state falls between them.

The state machine had a hole that neither query covered. Recovery went from a
45-second hang to 0.19 s.

---

## B7 — A limit that silently clamped to the default

**Symptom**

The 500-agent load test stalled at **exactly 200** completions and timed out.

**Cause**

```go
if limit <= 0 || limit > 1000 { limit = 200 }   // ← the standard-looking idiom
```

A caller asking for 2000 got **200** — the *default*, not the maximum. The poll
loop could only ever see the 200 newest runs, so 300 were invisible forever.

**Fix**

```go
if limit <= 0   { limit = defaultRunLimit }
if limit > max  { limit = maxRunLimit }
```

plus a regression test that asks for 9999 of 260 rows and asserts it gets 260.

**What it teaches**

> Silently shrinking a request to *less than the maximum* is a trap: the caller
> believes it has seen everything.

Neither I nor the assistant caught this by reading it. It surfaced only because a
load test asserted an exact total.

---

## B8 — Test packages truncating each other's database

**Symptom**

Two store tests failed that had passed minutes earlier, in a way that looked
like a race in the lease queue — and vanished when the package was run alone.

**Cause**

`go test ./...` runs packages **concurrently**. Three Postgres-backed packages
(`store`, `durability`, `load`) each truncate on entry, against the same
database.

**Fix**

Not `-p 1` — that serialises packages and **hides** the problem. Each package now
gets a private Postgres schema via the connection's `search_path`
([`internal/testsupport`](../../backend/internal/testsupport/pg.go)), so they
still run in parallel and still start clean.

**What it teaches**

> "Flaky test" is almost never a diagnosis.

This would have been blamed on the lease queue — the most concurrency-sensitive
code in the system — and I would have spent hours hardening code that was
already correct.

---

## B9 — A documentation claim that quietly became false

**Symptom**

```
plaintext credential use sites: 4 across 2 files
```

The docs said **one**.

**Cause**

The claim was true when the guard test was first written, before `invoke.go`
existed. The test had been reporting the real number all along; nobody re-read
it. The test *also* counted a **comment** containing `.Reveal()`, which made the
number meaningless as a guard.

**Fix**

- Skip comment lines when counting → the true figure is **3**
- Assert an **exact** count, not a ceiling, so a change is a review event
- Enumerate the three legitimate sites in the test's doc comment
- Correct the claim in `DESIGN.md`, `DEEP_DIVE.md`, `TUTORIAL.md` and the system
  diagram

**What it teaches**

> A guard that reports a number nobody reads is not a guard.

`total > maxReveal` would have stayed green as the count grew from 1 to 3.
`total != expected` forces a human to look.

---

## The pattern

| Bug | Found by | Would code review have caught it? |
|---|---|---|
| B1 privilege ordering | running the test | No — the code looked right |
| B2 false-positive network test | reading the *output* carefully | No — it was green |
| B3 fork bomb never forked | noticing 27 ms was too fast | No — it was green |
| B4 `cardinality(NULL)` | conformance suite, both stores | Unlikely |
| B5 timestamp precision | conformance suite, both stores | No |
| B6 stuck `RUNNING` | durability test hanging | Maybe, with a state-machine audit |
| B7 limit clamping | load test stalling at a round number | No |
| B8 test isolation | running the **whole** suite | No |
| B9 stale doc claim | reading test output | No |

**Zero of nine were found by reading code.** Three were found only because one
suite runs against two implementations. Two were found because a test looked
green and I checked *why*.

### The three habits that actually found things

1. **Run the whole suite, not the package you are working on.** B8 only exists
   when packages run together.
2. **Read the output of passing tests.** B2, B3 and B9 were all green. The
   numbers were wrong.
3. **Test two implementations against one contract.** B4 and B5 were invisible
   to the fake.

---

**Next:** [Glossary](../05-reference/01-glossary.md).

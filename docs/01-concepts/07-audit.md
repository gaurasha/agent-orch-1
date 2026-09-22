# Audit and tamper evidence

> **Prerequisite:** [Multi-tenancy](03-multi-tenancy.md)
> **Read next:** [Prompt injection](08-prompt-injection.md)
> **Code:** [`internal/store/verify.go`](../../backend/internal/store/verify.go)

---

## 1. The question an auditor actually asks

The brief says: *"Assume auditors will ask 'show me every command agent X ran
last Tuesday'."*

Answering that requires four things, and most systems get the first three:

1. **Every** tool call is recorded — including refused ones
2. Each record names the agent, the tenant **and the triggering human**
3. Records are queryable by time
4. **The records can be shown to be unmodified**

Point 4 is the one that separates a log from evidence. Any database can hold
rows. The question is:

> *Can someone with database access quietly change what this says?*

If the answer is yes, the log is a convenience for debugging, not an audit
trail. And the moment it matters — during an incident, in a dispute, in front of
a regulator — "we have logs" is worth very little if the person under
investigation could have edited them.

---

## 2. Record every decision, not every success

```go
if decision.Effect != authz.Allow {
    // Denials are audited exactly as thoroughly as successes. A security
    // log that only records what worked answers the wrong question.
    g.audit(ctx, run, def, req.Tool, string(decision.Effect), decision.Reason, req.Args, …)
    …
}
```

A log of successes tells you what the system did. A log of **attempts** tells you
what someone tried to do — which is the actual security question. Three denied
exfiltration attempts from one agent is a detectable signal; an empty success log
is not.

---

## 3. Two records per call, on purpose

```
audit(pre)  → { phase: "pre",  idem_key, args_sha256 }
   … the side effect happens …
audit(post) → { phase: "post", is_error, result_bytes, duration_ms,
                credential_ref, credential_id, driver, exit_code }
```

If the process dies between them, you still know the attempt was made. That is
the difference between *"we do not know the outcome"* and *"we have no idea it
was even attempted"* — and only the first is recoverable.

---

## 4. Hash chaining

Each record includes the hash of the one before it:

```
record₁.hash = sha256(canonical(record₁) ‖ "genesis")
record₂.hash = sha256(canonical(record₂) ‖ record₁.hash)
record₃.hash = sha256(canonical(record₃) ‖ record₂.hash)
```

```go
func HashAudit(r AuditRecord) (string, error) {
    payload := map[string]any{
        "tenant_id": r.TenantID, "seq": r.Seq,
        "ts": r.TS.UTC().Format(time.RFC3339Nano),   // ← timestamp is INSIDE the hash
        "run_id": r.RunID, "agent_name": r.AgentName,
        "triggering_user": r.TriggeringUser, "tool": r.Tool,
        "decision": r.Decision, "reason": r.Reason,
        "args_redacted": r.ArgsRedacted, "result_meta": r.ResultMeta,
        "prev_hash": r.PrevHash,
    }
    b, _ := canon.Bytes(payload)
    return canon.HashHex(b), nil
}
```

### What each attack costs

| Attack | Result |
|---|---|
| `UPDATE` one record | Its hash changes → the next record's `prev_hash` no longer matches → **chain breaks at that seq** |
| `DELETE` one record | Sequence gap → `VerifyChain` reports "records were deleted" |
| Backdate a record | The timestamp is inside the hash → breaks |
| Reorder records | `seq` is inside the hash → breaks |
| **Rewrite the entire tail** | **Possible with sustained write access** ⚠ |

### Verification

```go
func VerifyChain(recs []AuditRecord) error {
    prev := GenesisHash
    var lastSeq int64
    for i, r := range recs {
        if i > 0 && r.Seq != lastSeq+1 {
            return fmt.Errorf("audit chain gap before seq %d …: records were deleted", r.Seq)
        }
        if r.PrevHash != prev { … }
        want, _ := HashAudit(r)
        if want != r.Hash {
            return fmt.Errorf("audit record seq %d was modified after it was written", r.Seq)
        }
        prev, lastSeq = r.Hash, r.Seq
    }
    return nil
}
```

Run on every tenant-scoped read, and surfaced in the console as a banner:

```
Hash chain verified over 14 records. No record has been modified,
reordered or removed since it was written.
```

**Tested by forging** — the test rewrites one record's arguments the way someone
covering their tracks would, and asserts detection:

```
-> audit chain verifies for tenant "acme"            ok  (14 records, hash-chained)
-> tampering with tenant "acme"'s audit is detected  ok
```

---

## 5. Per-tenant chains

```sql
PRIMARY KEY (tenant_id, seq)
```

Appends must serialise behind the chain head — you cannot compute record *N*'s
hash until *N−1* exists. Two consequences of chaining per tenant instead of
globally:

| | Global chain | **Per-tenant chain** |
|---|---|---|
| Append throughput | the **whole fleet** serialises | each chain sees a fraction |
| Export | must redact other tenants → breaks verification | **self-verifying** in isolation |

The second is not a minor convenience. Handing a customer their own audit chain,
which they can verify independently without trusting you, is a meaningfully
different product than "we looked in our logs for you".

The serialisation is implemented by locking the tail row inside the transaction:

```sql
SELECT seq, hash FROM audit_log WHERE tenant_id=$1 ORDER BY seq DESC LIMIT 1 FOR UPDATE
```

At 10k tool calls/min spread over many tenants each chain sees a small fraction,
so this is not a bottleneck. At 10× with a skewed tenant distribution it would
need per-tenant-per-shard chains.

---

## 6. The bug that made this real

The chain verified perfectly against the in-memory store and **failed against
Postgres, every single time**.

Cause: Go's `time.Time` carries **nanoseconds**; Postgres `timestamptz` stores
**microseconds**. The timestamp is inside the hash, so:

```
hash computed with ns  →  INSERT  →  stored truncated to µs  →  SELECT  →
rehash with µs  →  DIFFERENT HASH  →  "tampered"
```

```go
// Postgres timestamptz has microsecond resolution. Hashing nanoseconds
// that the column will truncate makes every chain fail to re-verify
// after a round trip, so we truncate before hashing and storing.
rec.TS = time.Now().UTC().Truncate(time.Microsecond)
```

**Why this is worse than an ordinary bug:** a tamper-evident log that *always*
reports tampering is worse than no log at all. It trains operators to ignore the
alarm, and then the one real alarm is ignored too. A security control that cries
wolf has negative value.

It was found because one conformance suite runs against **both** stores. Tested
separately, the in-memory suite would have been green and this would have shipped.

---

## 7. Redaction — what is safe to record

Arguments are attacker-influenced and can be large (a whole script, a whole
document).

```go
func redactArgs(args map[string]any) map[string]any {
    const maxLen = 2000
    for k, v := range args {
        lk := strings.ToLower(k)
        if strings.Contains(lk, "token") || strings.Contains(lk, "secret") ||
           strings.Contains(lk, "password") || strings.Contains(lk, "authorization") ||
           strings.Contains(lk, "api_key") || strings.Contains(lk, "apikey") {
            out[k] = "[REDACTED]"
            continue
        }
        if s, ok := v.(string); ok && len(s) > maxLen {
            out[k] = s[:maxLen] + fmt.Sprintf("...[truncated, %d bytes total]", len(s))
            continue
        }
        out[k] = v
    }
}
```

Two competing pressures, resolved deliberately:

| Pressure | Resolution |
|---|---|
| The auditor needs to see what actually ran | Arguments are kept, readable, up to 2 KB |
| Recording a credential would make it **durable** | Key-name heuristic redaction, plus `args_sha256` so the *full* value is still verifiable without being stored |

The `args_sha256` is the nice part: an auditor holding a copy of the original
argument can prove it matches, without the log ever containing it.

**Honest limit:** the key-name heuristic misses a token passed under a key like
`x` or embedded in a URL. It is defence in depth for a case that should not
happen (the agent should never *have* a credential to pass), not the primary
control.

---

## 8. Answering the auditor's question

`GET /v1/audit?tenant=acme&limit=500`, backed by:

```sql
CREATE INDEX idx_audit_tenant_ts ON audit_log (tenant_id, ts);
```

"Every command agent X ran last Tuesday" is a range scan on that index.

```
11:00:49  run_01a0c8c68  report-writer  exec.bash   {"script":"echo '--- files ---'; ls -la /work…
11:00:50  run_01a0c8c68  report-writer  doc.convert {"from":"markdown","input":"report.md",…}
11:00:52  run_01a0c8c69  release-publisher github.cli {"argv":["pr","create","--title",…]}
```

Each line carries: time, run, agent, tool, redacted arguments — and the record
behind it also carries tenant, triggering user, decision, reason, the definition
digest, and the credential reference used.

**The definition digest is the one people forget.** Without it, "agent X ran this
command" is unanswerable if someone edited agent X yesterday. With it, you can
reconstruct exactly what that agent was *permitted* to do at the moment it ran.

---

## 9. What is not built

| Gap | Why it matters |
|---|---|
| **External anchoring of the chain head** | This is the residual risk. Hash chaining is tamper-**evident**, not tamper-**proof**: an attacker with sustained write access can rewrite the whole tail. Periodically publishing the head hash somewhere they do not control — a [transparency log](https://transparency.dev/), another account's object store, a receipt handed to the customer — makes that require forging the external record too. **This is the single change that would move the audit log from "good" to "defensible in a dispute".** |
| Write-once storage | S3 Object Lock / WORM would remove the rewrite path at the storage layer |
| Signed records | HMAC or a signature per record would bind them to a key an operator does not hold |
| Log shipping | Records live only in Postgres; a SIEM should receive them too |
| Retention and tiering | The table grows forever |
| Audit of *reads* | We record what agents did, not who looked at the records |

---

## References

- [Certificate Transparency, RFC 6962](https://www.rfc-editor.org/rfc/rfc6962) — Merkle-tree logs
- [transparency.dev](https://transparency.dev/)
- [Merkle trees](https://en.wikipedia.org/wiki/Merkle_tree) — the upgrade from a chain when you need efficient inclusion proofs
- [JCS, RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) — canonicalisation, without which hashing is unstable

---

**Next:** [Prompt injection](08-prompt-injection.md) — the attack you cannot
detect.

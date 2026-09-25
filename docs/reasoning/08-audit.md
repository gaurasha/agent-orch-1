# 08 · Audit — proving who did what

> Decision: **a per-tenant hash chain in Postgres, written by the gateway
> only: two records per allowed tool call (intent before the side effect,
> outcome after) and one per denial; `hash = sha256(prev_hash ‖ canonical(record))`;
> timestamps at microsecond resolution; `VerifyChain` recomputes on demand.**
> Not a plain append-only table, not a Merkle tree, not an external ledger,
> not "the logs".
>
> Diagrams: [10-observability](../architecture/10-observability.md), [07-state-failover](../architecture/07-state-failover.md) · Concept: [audit](../01-concepts/07-audit.md)

## Problem

The requirement is a question an operator must be able to answer: *"every
command agent X ran last Tuesday, who triggered it, and whether it was
allowed."* The physical constraints:

1. The record must exist **even if the process died mid-action** — otherwise
   the most interesting events (crashes during a side effect) are exactly the
   ones missing.
2. **Denials matter as much as successes.** A security log that only records
   what worked answers the wrong question; an injection attempt shows up as a
   denial.
3. **Tampering must be detectable.** Whoever can write the database can edit
   history; the audit record must at least make that visible.
4. It must be **queryable by tenant and time** at 10 k tool calls/min without
   a separate system.
5. **No secret may ever land in it** — it is the longest-lived, most-read
   store in the system.

## Options

| # | Option | Strongest case | Where it breaks | Who does it |
|---|---|---|---|---|
| A | **"The logs are the audit trail"** | already exist; free | sampled, rate-limited, rotated, unstructured; a log pipeline outage silently loses records; not transactional with the action; anyone with log access can edit them; no tamper evidence | far too common |
| B | **Plain append-only table** | queryable; transactional; simple | no tamper evidence: a DB admin can `UPDATE` or `DELETE` and nobody knows; Postgres has no true append-only mode (revoke `UPDATE`/`DELETE` helps against application bugs, not against the owner) | many SaaS "activity logs" |
| C | **Hash chain per tenant** (chosen) | edits, deletions and reordering break every hash after them; `VerifyChain` is a linear scan; one extra column pair; no external dependency; per-tenant chains do not contend | proves *modification* but not *omission by the DB owner* who can also recompute the chain — needs periodic anchoring (publish the head hash externally) for that; verification is O(n) per tenant | Certificate Transparency-adjacent designs, many compliance logs, this repository |
| D | **Merkle tree / verifiable log** (Trillian, Sigstore Rekor, CT logs) | O(log n) inclusion and consistency proofs; auditors can verify without the full log; the gold standard | another service and its storage; proof generation on the write path or a batching pipeline; the tenant's auditor would need tooling to consume proofs | Certificate Transparency, Sigstore, Go module checksum DB |
| E | **A ledger database** (Amazon QLDB — deprecated 2025, immudb) | tamper-evident with built-in verification | vendor/lifecycle risk (QLDB's shutdown is the example); another store on the hot path | some regulated workloads |
| F | **Blockchain / distributed ledger** | strong immutability claims | throughput, latency, cost and operational complexity are wildly out of proportion; a private chain reduces to option D with more parts | vanishingly few production systems for audit |
| G | **Ship to a SIEM / WORM storage** (Splunk, S3 Object Lock, Cloud Audit Logs) | retention and compliance controls exist; write-once buckets | not transactional with the action; not queryable by the application; adds latency or async loss; but an excellent *second copy* | most enterprises, as the archive tier |

## Deep dive

### Two records per call, and why the first is written *before* the side effect

If the gateway dies between "we are about to run `gh pr create`" and
"here is what happened", a single post-hoc record would never be written and
the run's history would show nothing. Writing `{phase: pre, idem_key,
args_sha256, rule}` **before** the side effect means the worst case is a
record that says "attempted, outcome unknown" — which is also exactly what the
`tool_calls` journal says after the reaper marks the stuck call. The two
systems agree because they were designed from the same failure.

### Denials are recorded with the rule

`{decision: DENY, rule: params.host_not_allowed, reason: …, args_sha256}` —
the rule name is the operator's classifier. A prompt-injection attempt
produces a specific sequence (`params.denied_pattern`, `params.host_not_allowed`,
`params.ssrf`, `tool.not_granted`) that is visible in one query and in one
metric (`tool_calls_total{decision="DENY"}`). No detector needed.

### The chain

```
record = {tenant, seq, ts(µs), run, agent, user, tool, decision, reason, args_redacted, result_meta}
hash   = sha256(prev_hash ‖ canonical_json(record))      prev_hash = genesis for seq 1
```

`AppendAudit` locks the chain head (`… ORDER BY seq DESC LIMIT 1 FOR UPDATE`)
so one tenant's records are serialised; other tenants' chains are unaffected.
`VerifyChain` recomputes from the first record and reports the first `seq`
whose `prev_hash` does not match. Canonical JSON (sorted keys, no
insignificant whitespace) makes the hash independent of how a record was
serialised.

**Bug B5** is the cautionary tale: Go's `time.Now()` has nanoseconds; Postgres
`timestamptz` has microseconds. The hash was computed over nanoseconds and
the stored record had microseconds, so every chain failed verification *after
a round trip* — and passed in the in-memory store. The conformance suite
running against both stores found it; the fix truncates before hashing and
storing. The general lesson: **verify after a round trip through the real
store, never only in memory.**

### What the chain does not prove

The database owner can rewrite a record *and* recompute every subsequent
hash. The chain proves modification by anyone who cannot do that — an
application bug, a compromised API key, a partial write. To bind the owner,
publish the chain head (the latest `hash`) somewhere the owner cannot
rewrite: a WORM bucket, a transparency log, a customer's own store, or a
printed daily digest. That is the designed production step, and it is cheap
(one hash per tenant per interval).

### Redaction

`args_redacted` is the arguments with known-sensitive keys masked;
`result_meta` carries sizes, exit codes, `credential_id` — never a value. The
`Secret` type ([06](06-credentials.md)) makes accidental inclusion a type
error.

### Why not the event log

The event log is the *run's* history: what the model saw and said. The audit
log is the *platform's* history: what was allowed and why, across runs, per
tenant, tamper-evident. They overlap (both record a tool call) but serve
different readers and retention policies, and only one of them is chained.
Human actions (`approve`, `cancel`) are currently in the event log only;
adding them to the chain is a one-line change the production checklist
includes.

## State of the art

* **Certificate Transparency** (RFC 6962 / 9162) and **Trillian**: Merkle
  logs with inclusion/consistency proofs — the standard when third parties
  must verify without trusting the operator.
* **Sigstore Rekor**: a transparency log for software signatures, built on
  Trillian; the same shape applied to supply chain.
* **immudb**: an embedded/tamper-evident database with cryptographic proofs;
  the nearest "ledger as a library".
* **Amazon QLDB**: a managed ledger with a journal and verification API;
  AWS announced its end-of-support in 2025 — a reminder that a vendor ledger
  on the hot path is a lifecycle risk.
* **Cloud audit logs** (AWS CloudTrail with log-file validation, GCP Cloud
  Audit Logs): CloudTrail's digest files are a hash chain over log files —
  the same mechanism, at the file level.
* **SOC 2 / ISO 27001** expectations: attributable, complete, protected
  from modification, retained — the chain plus WORM archive meets them; logs
  alone typically do not.

## Documented issues

* **Log pipelines lose data**: every major logging vendor documents
  sampling, rate limits and ingestion caps; the point of a transactional
  audit table is that the record is written in the same database, on the
  same path, as the action.
* **Timestamp precision mismatches** (this repository's B5) are a known
  class of hash-chain bug; CloudTrail's digest documentation is careful to
  define exactly what bytes are hashed for the same reason.
* **QLDB deprecation** (2025): an example of the vendor-ledger risk.
* **Audit tables and bloat/retention**: an insert-only table grows forever;
  partitioning by time and tiering old partitions to object storage is the
  designed mitigation (the chain still verifies across tiers because hashes
  are in the rows).

## Evidence in this repository

* `TestAudit_ChainIsTamperEvident` — edits a record and asserts
  `VerifyChain` reports the seq.
* S17 in [safety proofs](../04-evidence/02-safety-proofs.md).
* B5 in [bugs found](../04-evidence/03-bugs-found.md).
* The demo verifies the chain end to end after the injection scenario.

## Would reverse if

* tenants' auditors need to verify without trusting the operator → D
  (Merkle proofs) or at least anchored heads;
* audit volume outgrows Postgres partitions → keep the chain, tier
  partitions to object storage, verify across tiers;
* regulation requires an independent WORM copy → add G as the archive tier
  (the chain stays as the queryable copy).

## References

* Laurie, Langley, Kasper, *Certificate Transparency* (RFC 6962) — https://www.rfc-editor.org/rfc/rfc6962 · RFC 9162 — https://www.rfc-editor.org/rfc/rfc9162
* Google Trillian — https://github.com/google/trillian
* Sigstore Rekor — https://docs.sigstore.dev/logging/overview/
* AWS CloudTrail, *Log file integrity validation* — https://docs.aws.amazon.com/awscloudtrail/latest/userguide/cloudtrail-log-file-validation-intro.html
* AWS, *Amazon QLDB end of support* — https://aws.amazon.com/blogs/database/migrate-an-amazon-qldb-ledger-to-amazon-aurora-postgresql/
* immudb — https://docs.immudb.io/
* Haber & Stornetta, *How to time-stamp a digital document* (1991; the hash-chain origin) — https://link.springer.com/article/10.1007/BF00196791
* PostgreSQL, *Table partitioning* — https://www.postgresql.org/docs/current/ddl-partitioning.html
* AICPA, *SOC 2 Trust Services Criteria* — https://www.aicpa-cima.com/resources/landing/system-and-organization-controls-soc-suite-of-services

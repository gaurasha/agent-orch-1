# The demo, assertion by assertion

> `make demo` (Linux, root for namespaces; `AGENTORCH_DSN` optional — without
> it the in-memory store is used) runs six scenarios through the **real
> binary** — control plane, workers, gateway, sandboxes, broker, a fake GitHub
> API — and exits non-zero if any of 21 assertions fails. This page lists every
> assertion, what it proves, the mechanism it exercises and where that
> mechanism lives, so the output can be read as evidence rather than as a
> green light.
>
> Code: [`backend/cmd/agentorch/demo.go`](../../backend/cmd/agentorch/demo.go) · Related: [safety proofs](../04-evidence/02-safety-proofs.md) · [running](../03-operations/01-running.md)

## Setup the demo performs

Starts the platform in-process (`--role=all`, 4 workers, fake provider at 300 ms
latency), seeds the two tenants and six agents, starts `internal/fakegithub`
on a local port and points `github.cli`'s `URLTemplate` at it, then launches
runs with `POST /v1/runs`-equivalent calls and waits on their terminal state.

## Scenario 1 — a normal agent (`acme/report-writer`)

| # | Assertion | Proves | Mechanism |
|---|---|---|---|
| 1 | run completes successfully | the loop works end to end: lease → replay → model → tools → commit → `SUCCEEDED` | worker `step()`, `Commit` |
| 2 | document conversion ran inside the sandbox | `doc.convert` executed `aoconvert` in a jail (a `TOOL_RESULT` from `doc.convert` with `is_error=false`) | `invokeExec`, namespace driver |
| 3 | converted artifact persisted in the run workspace | `/work` is the one thing that survives the sandbox; the HTML file exists on disk | `WorkspaceManager`, bind mount |

## Scenario 2 — the credential boundary (`acme/release-publisher`)

| # | Assertion | Proves | Mechanism |
|---|---|---|---|
| 4 | agent's own sandbox contains no credential | `exec.bash` dumped `env` and `/proc/self/environ`; no token in the output | separate PID/user namespaces; `SafeEnv` |
| 5 | credentialed CLI call succeeded through the broker sandbox | `github.cli ["pr","create",…]` returned success | `invokeCLI`, broker sandbox, `GH_TOKEN` in its env only |
| 6 | the third-party API verified a genuine minted credential | the fake GitHub's `Verify` accepted a token for tenant acme / ref `github/token` (count > 0) | `DerivedBroker.Mint/Verify` |
| 7 | no credential material anywhere in the event log | scanning every event payload for the token's value finds nothing | `creds.Scrub`, `Secret` type |

## Scenario 3 — prompt injection (`globex/injected-agent`)

| # | Assertion | Proves | Mechanism |
|---|---|---|---|
| 8 | every exfiltration attempt was refused by policy (≥ 3 denials) | `http.get` to an unlisted host, `github.cli auth token`, an ungranted tool → `TOOL_DENIED` with rules `tool.not_granted` / `params.*` | `authz.Evaluate` |
| 9 | cloud metadata endpoint unreachable from the sandbox | `exec.bash` tried `169.254.169.254` → `ENETUNREACH` | empty network namespace |
| 10 | run still finished cleanly rather than crashing | denials are tool results the model can re-plan from; the run reaches a terminal state | `TOOL_DENIED` → tool message |

## Scenario 4 — a poison agent (`globex/poison-loop`)

| # | Assertion | Proves | Mechanism |
|---|---|---|---|
| 11 | poison agent was stopped by the platform | state `FAILED` | `Budget.ExceedsReason` |
| 12 | stopped by an enforced budget, not by chance | `status_reason` names the exhausted budget (`tool-call budget exhausted (5/5)`) | worker step 2 / gateway `budget.exhausted` |
| 13 | bounded cost | `usage.cost_usd ≤ budget.max_cost_usd` | `Settle`, price table, budget |

## Scenario 5 — human in the loop (`acme/change-approver`)

| # | Assertion | Proves | Mechanism |
|---|---|---|---|
| 14 | agent parked waiting for a human | state `WAITING_HUMAN` after `stop_reason = human_input_required` | `HUMAN_PAUSE`, `Commit{ReleaseLease}` |
| 15 | holds no worker lease while parked | `lease_owner` empty — a parked agent costs nothing | `ReleaseLease` |
| 16 | agent resumed and completed after human input | `POST …/resume` → `HUMAN_RESUME` → re-queued → `SUCCEEDED` | `AppendSystemEvents`, replay |

(“resume accepted” asserts only if the resume call itself errors.)

## Scenario 6 — the audit trail (both tenants)

| # | Assertion | Proves | Mechanism |
|---|---|---|---|
| 17, 18 | audit chain verifies for tenant `acme` / `globex` | `VerifyChain` over every record → nil | `AppendAudit` hash chain |
| 19, 20 | tampering with tenant `acme` / `globex`'s audit is detected | one record's `reason` is edited in memory → `VerifyChain` names its seq | sha256 over canonical record ‖ prev |
| 21 | audit records attribute each command to agent, tenant and user | at least one record carries `agent_name`, `tenant_id`, `triggering_user`, `tool`, `decision` | `AuditRecord` |

(“audit readable for <tenant>” asserts only if listing the audit fails.)

## Reading the output

```
== 3. PROMPT INJECTION: containment, not detection ==
  [PASS] every exfiltration attempt was refused by policy   (3 denials: tool.not_granted, params.denied_pattern, params.host_not_allowed)
  [PASS] cloud metadata endpoint unreachable from the sandbox
  ...
== RESULTS ==
21 passed, 0 failed
```

A `[FAIL]` line carries the detail string; the exit code is 1. Each scenario
prints the run id, so `GET /v1/runs/{id}/events` (or the console) shows the
full timeline behind any assertion.

## What the demo does not prove

That a *real* model behaves — the provider is scripted; every assertion is on
the **platform's** behaviour given adversarial model output. Isolation
strength beyond namespaces (gVisor) — the demo runs the `namespace` driver.
Multi-replica behaviour — one process. Those live in the safety suite, the
cluster tests and the durability/load tests ([test inventory](../05-reference/09-test-inventory.md)).

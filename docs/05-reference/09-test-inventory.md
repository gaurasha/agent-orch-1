# Test inventory — every test, what it proves, what it needs

> The claim "57 test results, 57 passing" decomposes as: 38 Go test functions,
> of which the 11 store-conformance tests run twice (in-memory and Postgres)
> via subtests = 49 results, plus `TestMain` and per-package setup counted by
> `go test -v`, and the 3 load tests' sub-scenarios. The exact count varies
> with `-run` filters; what matters is the list below and that every row has a
> way to fail that the attacker's absence cannot satisfy
> ([evals and testing](../reasoning/13-evals-and-testing.md)).
>
> Related: [safety proofs](../04-evidence/02-safety-proofs.md) (S1–S17, the security subset with full explanations) · [benchmarks](../04-evidence/01-benchmarks.md) · [bugs found](../04-evidence/03-bugs-found.md)

## How to run

```bash
make test              # unit + conformance against the in-memory store; no infrastructure
make test-safety       # sandbox tests; Linux + root
make test-durability   # needs AGENTORCH_TEST_DSN
make test-load         # needs AGENTORCH_TEST_DSN
make test-all          # everything (~3–5 min on a 4-core VM)
make demo              # the 21-assertion end-to-end script (Linux + root)
make k8s-validate      # kubeconform on both kustomizations
make kind-test / argocd-test   # live-cluster checks
```

Postgres-backed tests use a schema per package (`testsupport.SchemaDSN`) so
packages can run in parallel without truncating each other's tables (B8).

## 1. Unit — `internal/creds` (no infrastructure)

| Test | Proves | Failure mode caught |
|---|---|---|
| `TestSecret_NeverRendersItsValue` | `%v %+v %#v %s`, `String()`, `json.Marshal` all yield `[REDACTED]` | a formatter path that prints the value |
| `TestSecret_SurvivesStructPrintingInsideAContainer` | a `Secret` nested in a struct/map/slice still redacts | the common "log the config struct" leak |
| `TestSecret_RevealCallSitesAreFewAndIntentional` | exactly **3** `.Reveal()` call sites in non-test source (comments skipped) | a new plaintext use added without changing the test (B9) |

## 2. Unit — `internal/fairness` (no infrastructure)

| Test | Proves |
|---|---|
| `TestFairness_NoisyTenantCannotStarveQuietTenant` | a tenant offering 50× the load cannot reduce a quiet tenant's grants below its guarantee |
| `TestFairness_WeightsAllocateProportionally` | weights 2:1 produce grants 12:6 after refill |
| `TestFairness_InteractiveReserveProtectsHumanBlockingWork` | batch cannot drain the last 20 %; interactive can |
| `TestFairness_SettleRefundsOverEstimate` | over-estimate → tokens returned |
| `TestFairness_SettleChargesUnderEstimate` | under-estimate → charged, bucket may go negative |
| `TestFairness_UnknownTenantFailsClosed` | no bucket → `Reserve` false |
| `TestFairness_GuaranteesSumToProviderCapacity` | Σ guaranteed rates = provider rate (with plan caps → spare) |

## 3. Store conformance — `internal/store` (memory ×1, Postgres ×1 when `AGENTORCH_TEST_DSN` is set)

| Test | Proves | Bug it caught |
|---|---|---|
| `TestLease_OnlyOneWorkerWins` | N concurrent `AcquireLease` on one run → exactly one winner | |
| `TestCommit_RejectedAfterLeaseLost` | `Commit` by a non-owner → `ErrLeaseLost`, nothing written | |
| `TestCommit_SeqMismatchIsConflict` | wrong `expectedNextSeq` → `ErrConflict` | |
| `TestLease_PriorityBeatsFIFO` | an interactive run created later is leased before an older batch run | |
| `TestLease_RespectsEligibleTenants` | `eligible=[A]` never leases B's runs; empty/nil eligible leases anything | **B4** `cardinality(NULL)` |
| `TestToolCall_IdempotencyPreventsSecondSideEffect` | second `BeginToolCall` with the same key → `fresh=false` + the existing row | |
| `TestToolCall_StuckCallsAreReapedAsAmbiguous` | `IN_FLIGHT` older than `stuck_after` → `FAILED` with the "MAY OR MAY NOT" text | |
| `TestAudit_ChainIsTamperEvident` | editing one record → `VerifyChain` names its seq | **B5** ns vs µs |
| `TestEvents_AppendOnlyOrdering` | `ListEvents(since)` returns dense, ordered seqs; `next_seq` advances by `len(events)` | |
| `TestListRuns_OverMaxLimitReturnsMaximumNotDefault` | `limit=5000` → 1000 rows, not 200 | **B7** |
| (setup) | the same suite body runs against `store.Memory` and `store.PG` | keeps the fake honest |

## 4. Safety — `internal/sandbox` (Linux, root, `namespace` driver)

| Test | Proof | Mechanism asserted | Negative control |
|---|---|---|---|
| `TestSandbox_HasNoNetworkAccess` | S1 | Python `socket.connect` → `ENETUNREACH` | the same probe **succeeds** on the host (or the test is skipped, never passed) — **B2** |
| `TestSandbox_HostFilesystemIsNotVisible` | S2 | `/etc/hostname`, `/root`, host paths absent; synthesised `/etc` present | |
| `TestSandbox_RootFilesystemIsReadOnly` | S3 | writes outside `/work`,`/tmp` → `EROFS` | writes inside succeed |
| `TestSandbox_WorkspaceIsTheOnlyThingThatPersists` | S4 | a file in `/work` survives; a file in `/tmp` does not | |
| `TestSandbox_RunsUnprivilegedWithNoCapabilities` | S5 | uid 1000, `CapBnd`/`CapEff` = 0, `NoNewPrivs` = 1 | |
| `TestSandbox_EscapeSyscallsAreBlocked` | S6 | 8 raw syscalls via `ctypes` → `EPERM`; `clone3` → `ENOSYS` | an allowed syscall succeeds |
| `TestSandbox_WallClockTimeoutIsEnforced` | S7 | `sleep 60` under `Wall=1s` → `TimedOut`, ~1 s | |
| `TestSandbox_MemoryLimitIsEnforced` | S8 | allocate > `MemoryBytes` → `OOMKilled` read from `oom_kill` | |
| `TestSandbox_ForkBombIsContained` | S9 | `os.fork()` loop refused at ≈ `pids.max` (23 of 24) and the test takes real time | **B3** |
| `TestSandbox_OutputIsCapped` | S10 | 10 MB of output → `Truncated`, ≤ `MaxOutputBytes` | |
| `TestSandbox_ColdStartIsMeasured` | S11 | 10 runs of `exit 0`; reports mean/worst (8.4 ms / 16 ms) | |
| `TestMain` | — | skips the package unless root on Linux, prints why | |

## 5. Durability — `test/durability` (Postgres)

| Test | Scenario | Assertion |
|---|---|---|
| `TestDurability_WorkerKilledMidToolCall_RunCompletesExactlyOnce` | worker A is killed between the tool call and its commit; B takes over | run `SUCCEEDED`; **4 tool requests, 3 side effects** (one replayed from the journal); no duplicate |
| `TestDurability_RollingDeployLosesNoWork` | SIGTERM all workers mid-run, start new ones | every run completes; total side effects equal expected |
| `TestDurability_PartitionedWorkerIsFencedOut` | worker A paused after replay; B commits; A resumes and commits | A gets `ErrLeaseLost`; the log has B's events only |

## 6. Load — `test/load` (Postgres)

| Test | Shape asserted | Observed |
|---|---|---|
| `TestLoad_500ConcurrentAgents` | 500 runs / 16 workers complete with **exactly 1000** tool calls (lost or duplicated work fails) | p99 1.0–1.6 s on a 4-core VM, stubbed model |
| `TestLoad_ScalesWithWorkers` | throughput(24 workers) > throughput(2 workers) — deliberately weak, shape only | 52.9–55.8× |
| `TestLoad_BurstDoesNotStarveTheOtherTenant` | a 50× burst from one tenant does not move the other's p99 beyond a bound | quiet tenant ≥ 98 % of its solo throughput |

## 7. End-to-end — `make demo` (21 assertions across 6 scenarios)

Listed in the [demo walkthrough](../06-guides/08-demo-walkthrough.md). Two further
`assert` calls exist only on error branches ("audit readable for", "resume
accepted") and do not fire on a healthy run, which is why `demo.go` contains 22
`assert(` calls but the script reports 21/21.

## 8. Manifests and cluster — scripts

| Check | Tool | What it verifies |
|---|---|---|
| `make k8s-validate` | kubeconform, K8s 1.31 schemas, `-strict` | 30/30 resources in base and local overlay |
| `scripts/kind-test.sh` | kubectl | Deployments Available; `runAsNonRoot`/RO root/`drop ALL` read back; default-deny policy, quota and taint present; RBAC `can-i` (gateway can create pods, cannot exec/read secrets; workers cannot create pods); an end-to-end run through the deployed API; cross-tenant read → 404 |
| `scripts/argocd-test.sh` | kubectl | Application Synced + Healthy; AppProject present; `selfHeal` reverts a `kubectl set image` |
| `python3 docs/tools/check-links.py` | — | 0 broken internal links/anchors across the docs; `--external` fetches every URL |

## 9. What is not tested, and where that is written down

Real model behaviour, gVisor's isolation, absolute performance, multi-replica
fairness, a live denied-connection probe on the cluster — each listed with the
reason and the next step in [evals and testing §"What is deliberately not tested"](../reasoning/13-evals-and-testing.md).

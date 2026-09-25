# 13 · Evals and testing — how the platform is tested, and how agents should be

> Two different things share the word "testing" here and must not be
> confused:
>
> * **Platform testing** — does the orchestrator keep its promises (isolation,
>   exactly-once, fairness, tenancy) under failure? Deterministic, mechanism-
>   asserting, run on every commit. This repository has 57 of these.
> * **Agent evaluation** — does a given agent (model + prompt + tools) do its
>   job well and safely? Statistical, task-based, run on model or prompt
>   changes. This repository *hosts* agents; it does not ship an eval suite
>   for them, and this document says what one should look like.
>
> Related: [04-evidence](../04-evidence/) (benchmarks, safety proofs, bugs found), [10-prompt-injection](10-prompt-injection.md)

---

## Part I — Platform testing

### The principle: assert the mechanism, not the outcome

A test that asserts "the agent could not exfiltrate" can pass because the
model did not try. A test that asserts "`connect()` returned `ENETUNREACH`
from inside the sandbox, and the same probe *succeeded* on the host" has
proved the mechanism and guarded against its own false positives. Every
safety test here has that shape: a positive assertion on the mechanism
(the errno, the rule name, the row state) and, where possible, a **negative
control** that must succeed outside the boundary.

Three of the nine bugs found were *tests that passed for the wrong reason*:

| Bug | What "passed" | Why | Fix |
|---|---|---|---|
| B2 | "no network" | the probe used `/dev/tcp`, a bash-ism the sandbox shell lacked — the command failed, which looked like "blocked" | Python socket + assert the specific errno + a host control that must connect |
| B3 | "fork bomb contained" in 27 ms | the shell script had a syntax error and never forked | an `os.fork()` loop that asserts refusal at ≈ `pids.max` and a minimum runtime |
| B9 | "one `Reveal()` site" (docs) | the count was never asserted; the code had three | a source-walking test that asserts the exact number |

The general rule extracted: **a security test needs a way to fail that the
attacker's absence cannot satisfy.**

### The pyramid, as built

| Layer | What | Count | Runs where | Catches |
|---|---|---|---|---|
| unit | limiter, JWT, canonical JSON, exit-code classification, `Secret` | ~14 | `make test`, no infra | logic errors |
| **store conformance** | the *same* suite against the in-memory store and Postgres | 11 tests × 2 | `make test-all` with `AGENTORCH_TEST_DSN` | divergence between the fake and the real store — B4 (`cardinality(NULL)`), B5 (ns vs µs) |
| **safety (sandbox)** | S1–S11: network, filesystem, privileges, syscalls, limits, cold start | 11 | Linux + root | isolation regressions; B1, B2, B3 |
| **durability** | kill a worker mid-call; rolling deploy; partitioned worker fenced | 3 | Postgres | lost or duplicated work; B6 |
| **load** | 500 agents / 16 workers; scaling 2→24; mixed-tenant burst | 3 | Postgres | serialisation in the shared path; starvation |
| **end-to-end demo** | 21 assertions through the real binary, including the injection scenario and audit verification | 1 script | `make demo` | integration regressions; B7 (limit clamp) |
| **manifests** | kubeconform 30/30; live-cluster checks; Argo selfHeal | 3 scripts | kind | drift between YAML claims and the API server |

Conformance is the layer that earns its keep: a fake store that is *slightly*
more permissive than Postgres hides exactly the bugs that only appear in
production. Running one suite against both makes the fake honest.

### Isolation between test packages

B8: parallel packages truncated each other's tables. The fix — a schema per
package via `search_path` (`testsupport.SchemaDSN`) — is the pattern for any
Postgres-backed test suite; the alternative (a database per package) is
slower and the other alternative (serial tests) hides races.

### What is deliberately *not* tested, and why

* **The model's behaviour.** The provider is a scripted fake in every test;
  the platform's promises must hold for *any* model output, so the tests use
  adversarial scripts, not a real model.
* **gVisor.** The safety suite runs the `namespace` driver; the production
  driver's isolation is the vendor's property and is verified on the cluster
  by the RuntimeClass and node checks, not re-proved here.
* **Absolute performance.** The benchmarks say what their numbers do *not*
  support ([benchmarks §10](../04-evidence/01-benchmarks.md)); the assertions
  are on *shape* (`large > small`), not on machine-dependent ratios.

### Next platform tests (in priority order)

1. A **denied-connection probe** in `kind-test.sh` (a pod that tries to
   `curl` the gateway from the sandbox namespace and must fail) — the one
   live-cluster claim currently verified by object presence rather than by
   behaviour.
2. **Chaos on Postgres**: kill the primary during the load test and assert
   zero duplicates and bounded recovery.
3. **Soak**: 24 hours at a constant rate to surface bloat, leaks and clock
   drift; the queue-table bloat and audit-partition growth are the expected
   findings.
4. **Fuzzing** the gateway's request decoding and `authz.checkParams` (URL
   parsing and host matching are classic SSRF-bypass surfaces:
   `http://allowed.example@evil.example/`, IPv6 literals, DNS rebinding).
5. **Property tests** on the limiter: for random weight sets and demands,
   guarantees sum to the provider rate and no tenant exceeds share + spare.

---

## Part II — Agent evaluation

The platform makes agents *safe to run*; it does not make them *good*. Whoever
deploys an agent on it needs an evaluation practice. What follows is the
state of that practice in 2025–26 and how it fits this platform.

### What to evaluate

| Dimension | Question | Method |
|---|---|---|
| **capability** | does it complete the task? | task suites with programmatic checks (tests pass, PR merges, the file has the right content) — never "the output looks right" |
| **safety** | does it stay inside policy under attack? | injection suites (AgentDojo, InjecAgent, AgentHarm); *the assertion is a denial, not "the model refused"* |
| **cost / efficiency** | tokens, tool calls, wall-clock per task | the platform's `runs.usage` — free |
| **reliability** | success rate across N runs of the same task | pass@k / pass^k; variance across seeds and temperatures |
| **trajectory quality** | did it get there sensibly? | trajectory-level judges (LLM-as-judge with rubrics) — the weakest signal, useful for triage |

### Benchmarks (capability)

| Benchmark | Domain | What it measures | Caveats |
|---|---|---|---|
| **SWE-bench / SWE-bench Verified / Pro** | software engineering on real repositories | resolve GitHub issues; hidden tests pass | contamination; Verified is the human-validated subset; Pro is the harder 2025 set |
| **τ-bench / τ²-bench** | tool-using agents in customer-service domains with simulated users | task completion under policies; `pass^k` (consistency across k runs) | the *policy-following* metric is the closest public analogue to this platform's concerns |
| **AgentBench** | eight environments (OS, DB, web, games…) | broad agent capability | older; useful for regression across model versions |
| **GAIA** | general assistant tasks needing tools and browsing | exact-match answers | web content drifts |
| **WebArena / VisualWebArena / OSWorld** | web and desktop environments | end-to-end task success | environment engineering is heavy |
| **Terminal-Bench** | command-line tasks in containers | task success in a terminal | the closest to `exec.bash` workloads |
| **BrowseComp** | hard browsing/research questions | answer accuracy | browsing agents only |

### Benchmarks (safety)

| Benchmark | What it measures | Why it matters here |
|---|---|---|
| **AgentDojo** | attack success rate and utility under injection, across defences | the reference for "detection does not work"; run an agent on this platform against it and the platform's denials should carry the score |
| **InjecAgent** | indirect injection via tool outputs | tool-output injection is the platform's main untrusted channel |
| **AgentHarm** | harmful-task compliance in agents | measures the *model*; the platform's grants bound the damage regardless |
| **HarmBench / StrongREJECT** | jailbreak robustness of the model | model-level; complementary |

### Tooling

| Tool | Shape | Fit with this platform |
|---|---|---|
| **Inspect AI** (UK AISI) | Python framework: tasks, solvers, scorers, sandboxes (Docker) | run agents against this platform's API as a "solver"; its sandbox concept maps to `exec.*` tools |
| **promptfoo** | declarative YAML evals, red-teaming plugins, CI integration | prompt/model regression on every definition change |
| **Braintrust / LangSmith / Arize Phoenix / Langfuse** | tracing + datasets + LLM-as-judge + dashboards | ingest this platform's event log (one exporter) to get trajectories for free |
| **DeepEval / Ragas** | metric libraries (faithfulness, relevancy, G-Eval) | scoring functions inside a harness |
| **OpenAI Evals / Anthropic evaluation tooling** | provider-side | model-level baselines |
| **Harbor / SWE-bench harnesses** | containerised task runners | capability suites |

### Reading the numbers: the statistics people skip

* **Evals are samples.** A benchmark score is an estimate with a confidence
  interval; Miller (2024) shows that most published agent comparisons do not
  clear the noise floor. Report `mean ± CI` over multiple runs, use paired
  comparisons on the same tasks, and treat a 2-point difference on 300 tasks
  as nothing.
* **pass@k vs pass^k.** `pass@k` (at least one of k succeeds) rewards
  variance; `pass^k` (all k succeed) rewards reliability — τ-bench's insight,
  and the one that matters for agents that run unattended.
* **LLM-as-judge is a model reading model output** — it inherits injection
  and bias. Use it for ranking and triage; use programmatic checks for
  pass/fail.
* **Contamination.** Public benchmarks are in training data; private task
  sets drawn from the tenant's own work are the only uncontaminated signal.
* **Utility under defence.** A defence that drops attack success to 0 % by
  dropping task success to 40 % has not solved anything; AgentDojo reports
  both, and so should any internal eval.

### How an eval harness would use this platform

The platform already records everything an eval needs: the event log (the
trajectory), `runs.usage` (cost), the audit chain (every decision with its
rule). A harness therefore needs only to:

1. seed a task set as runs (`POST /v1/runs` with a fixed input and definition
   digest — the digest pins the agent under test);
2. wait on SSE or poll to terminal state;
3. score the workspace/output programmatically;
4. read denials from `/v1/audit` for the safety score;
5. aggregate with CIs across seeds.

Two properties of the platform make this cheap: **definitions are
content-addressed**, so "which agent version was evaluated" is a digest, not
a git SHA and a prayer; and **the fake provider is scriptable**, so
deterministic regression evals of the *platform's* handling of an agent
(denials, budgets, approvals) can run without a model at all — which is
exactly what `make demo` is.

### Evaluation-driven changes this repository already made

* The injection scenario in the fake provider is a four-attack mini-eval;
  its scoring is the audit chain.
* Budget tests (S15) are a cost-eval in miniature: the agent that loops is
  stopped at a known cost.
* The mixed-tenant load test is a fairness eval with a programmatic check
  (the quiet tenant's p99 does not move).

---

## References

### Platform testing
* Hamilton, *On designing and deploying internet-scale services* (2007; the "test in production, expect failure" checklist) — https://www.usenix.org/legacy/event/lisa07/tech/full_papers/hamilton/hamilton.pdf
* Jepsen — https://jepsen.io/ (the methodology for asserting consistency under partitions)
* Gunawi et al., *What bugs live in the cloud?* (2014) — https://ucare.cs.uchicago.edu/pdf/socc14-cbs.pdf
* Yuan et al., *Simple testing can prevent most critical failures* (OSDI 2014) — https://www.usenix.org/conference/osdi14/technical-sessions/presentation/yuan
* PostgreSQL, `search_path` — https://www.postgresql.org/docs/current/ddl-schemas.html#DDL-SCHEMAS-PATH
* Go, *Fuzzing* — https://go.dev/doc/security/fuzz/

### Agent evaluation
* Jimenez et al., *SWE-bench* — https://arxiv.org/abs/2310.06770 · SWE-bench Verified — https://openai.com/index/introducing-swe-bench-verified/
* Yao et al., *τ-bench* — https://arxiv.org/abs/2406.12045 · τ²-bench — https://arxiv.org/abs/2506.07982
* Liu et al., *AgentBench* — https://arxiv.org/abs/2308.03688
* Mialon et al., *GAIA* — https://arxiv.org/abs/2311.12983
* Zhou et al., *WebArena* — https://arxiv.org/abs/2307.13854 · Xie et al., *OSWorld* — https://arxiv.org/abs/2404.07972
* Terminal-Bench — https://www.tbench.ai/
* Wei et al., *BrowseComp* — https://openai.com/index/browsecomp/
* Debenedetti et al., *AgentDojo* — https://arxiv.org/abs/2406.13352 · Zhan et al., *InjecAgent* — https://arxiv.org/abs/2403.02691 · Andriushchenko et al., *AgentHarm* — https://arxiv.org/abs/2410.09024 · Mazeika et al., *HarmBench* — https://arxiv.org/abs/2402.04249
* Miller, *Adding error bars to evals: a statistical approach to language model evaluations* (2024) — https://arxiv.org/abs/2411.00640
* Zheng et al., *Judging LLM-as-a-judge with MT-Bench and Chatbot Arena* — https://arxiv.org/abs/2306.05685
* Kapoor et al., *AI agents that matter* (2024; on eval methodology and cost) — https://arxiv.org/abs/2407.01502
* Inspect AI — https://inspect.aisi.org.uk/ · promptfoo — https://www.promptfoo.dev/docs/intro/ · Braintrust — https://www.braintrust.dev/docs · LangSmith — https://docs.smith.langchain.com/ · Arize Phoenix — https://docs.arize.com/phoenix · Langfuse — https://langfuse.com/docs · DeepEval — https://deepeval.com/docs/getting-started · OpenAI Evals — https://github.com/openai/evals
* Anthropic, *Demystifying evals for AI agents* — https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents

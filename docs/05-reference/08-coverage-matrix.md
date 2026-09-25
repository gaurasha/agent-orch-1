# Coverage matrix — where every part of the design is defined, drawn, argued, proved and operated

> The purpose of this page is to let a reader check, without trusting anyone,
> that the collection has no holes: every component, every stated requirement,
> every cross-cutting concern and every question a given reader would ask has a
> row, and every row names the document that answers it. If you find a
> question with no row, that is a documentation bug — the
> [coverage checker](../tools/check-coverage.py) catches the mechanical half
> (identifiers in code that no document mentions); this page is the human half.

## A. Components

| Component | What it is | Diagram + companion | Concept | Reasoning (why this way) | Evidence | Operate / extend |
|---|---|---|---|---|---|---|
| Control plane (API + console) | [components §1](../02-architecture/01-components.md), [API](../02-architecture/03-api.md) | [01](../architecture/01-control-plane.md) | [multi-tenancy](../01-concepts/03-multi-tenancy.md) | [runtime model](../reasoning/01-runtime-model.md), [authorization](../reasoning/05-authorization.md) | kind-test end-to-end run; cross-tenant 404 | [console](../06-guides/07-operator-console.md), [onboarding](../06-guides/06-onboarding-a-tenant.md) |
| agentd workers + scheduler | [components §2](../02-architecture/01-components.md) | [02](../architecture/02-scheduling-runtime.md) | [durable execution](../01-concepts/02-durable-execution.md) | [runtime model](../reasoning/01-runtime-model.md), [scheduling](../reasoning/04-scheduling.md) | durability ×3, load ×3, lease conformance ×5 | [limits §1](05-limits-and-defaults.md), [capacity §3](../03-operations/05-capacity.md) |
| Reaper | [components §9](../02-architecture/01-components.md) | [02](../architecture/02-scheduling-runtime.md), [09](../architecture/09-failure-map.md) | [durable execution](../01-concepts/02-durable-execution.md) | [scheduling](../reasoning/04-scheduling.md) | `TestToolCall_StuckCallsAreReapedAsAmbiguous`, B6 | [runbook INC-6](../03-operations/04-runbook.md) |
| Tool gateway + policy engine | [components §3](../02-architecture/01-components.md) | [03](../architecture/03-tool-gateway.md) | [authorization](../01-concepts/04-authorization.md) | [authorization](../reasoning/05-authorization.md), [prompt injection](../reasoning/10-prompt-injection.md) | S14–S16, demo 8–13, journal conformance | [errors §2–3](06-errors-and-codes.md), [adding a tool](../06-guides/03-adding-a-tool.md) |
| Tool registry + tools | [tools catalogue](../06-guides/02-tools-catalogue.md) | [03](../architecture/03-tool-gateway.md) | [authorization §schema/backend](../01-concepts/04-authorization.md) | [authorization](../reasoning/05-authorization.md) | registry schema-only serialisation (S12 companion) | [adding a tool](../06-guides/03-adding-a-tool.md) |
| Agent sandbox | [components §6](../02-architecture/01-components.md) | [04](../architecture/04-sandbox.md) | [Linux isolation](../01-concepts/01-linux-isolation.md), [syscalls](02-syscalls.md) | [isolation](../reasoning/02-isolation.md) | S1–S11, B1–B3 | [limits §4](05-limits-and-defaults.md), [adding a driver](../06-guides/05-adding-a-sandbox-driver.md) |
| Credential broker + broker sandbox | [components §7](../02-architecture/01-components.md) | [05](../architecture/05-credential-plane.md) | [secrets](../01-concepts/05-secrets.md) | [credentials](../reasoning/06-credentials.md) | S12–S13, demo 4–7, B9 | [rotation](../03-operations/06-upgrades-backup-and-rotation.md), [onboarding §3](../06-guides/06-onboarding-a-tenant.md) |
| Model gateway | [components §4](../02-architecture/01-components.md) | [06](../architecture/06-model-plane.md) | — | [LLM integration](../reasoning/09-llm-integration.md) | gateway tests; demo `--model-fail-every` | [adding a provider](../06-guides/04-adding-a-model-provider.md) |
| Fairness limiter | [components §5](../02-architecture/01-components.md) | [06](../architecture/06-model-plane.md) | [fairness](../01-concepts/06-fairness.md) | [fairness](../reasoning/07-fairness.md) | fairness ×7, `TestLoad_BurstDoesNotStarveTheOtherTenant` | [runbook INC-5](../03-operations/04-runbook.md), [capacity §6](../03-operations/05-capacity.md) |
| Store (Postgres) + schema | [data model](../02-architecture/02-data-model.md) | [07](../architecture/07-state-failover.md) | [durable execution](../01-concepts/02-durable-execution.md) | [durability substrate](../reasoning/03-durability-substrate.md) | conformance ×11 ×2 stores, B4, B5, B7, B8 | [upgrades/backup](../03-operations/06-upgrades-backup-and-rotation.md), [capacity §5](../03-operations/05-capacity.md) |
| Audit chain | [audit](../01-concepts/07-audit.md) | [10](../architecture/10-observability.md) | [audit](../01-concepts/07-audit.md) | [audit](../reasoning/08-audit.md) | S17, demo 17–21 | [runbook INC-1/2](../03-operations/04-runbook.md) |
| Workspace manager | [tools catalogue §workspace](../06-guides/02-tools-catalogue.md) | [05](../architecture/05-credential-plane.md) | — | [credentials](../reasoning/06-credentials.md) | S4, demo 3 | [upgrades §7](../03-operations/06-upgrades-backup-and-rotation.md) |
| Fake GitHub API | [components §11](../02-architecture/01-components.md) | [05](../architecture/05-credential-plane.md) | — | [credentials](../reasoning/06-credentials.md) | demo 6 | — |
| Kubernetes manifests + GitOps | [Kubernetes primitives](../01-concepts/09-kubernetes.md) | [08](../architecture/08-network-topology.md) | same | [Kubernetes](../reasoning/11-kubernetes.md) | k8s-validate 30/30, kind-test, argocd-test | [deployment](../03-operations/02-deployment.md) |
| Observability (metrics, logs, SSE) | [observability](../03-operations/03-observability.md) | [10](../architecture/10-observability.md) | — | [audit](../reasoning/08-audit.md) | — | [runbook](../03-operations/04-runbook.md) |
| Operator console (UI) | [console guide](../06-guides/07-operator-console.md) | [01](../architecture/01-control-plane.md) | — | [language §UI](../reasoning/12-language-and-dependencies.md) | typecheck in `make lint` | [console guide](../06-guides/07-operator-console.md) |
| CLI shim (`gh`) and `aoconvert` | [tools catalogue](../06-guides/02-tools-catalogue.md) | [05](../architecture/05-credential-plane.md) | [secrets §busybox](../01-concepts/05-secrets.md) | [language](../reasoning/12-language-and-dependencies.md) | demo 2, 5 | [adding a tool §2](../06-guides/03-adding-a-tool.md) |

## B. The brief's requirements

| Requirement (as decoded) | Decoded in | Mechanism | Diagram | Evidence |
|---|---|---|---|---|
| R1 scale: thousands of agents, 10k tool calls/min, bursty | [requirements R1](../00-problem/03-requirements-decoded.md) | stateless workers, per-call sandboxes, backpressure in scheduling | [02](../architecture/02-scheduling-runtime.md), [06](../architecture/06-model-plane.md) | [benchmarks §1–4](../04-evidence/01-benchmarks.md), [capacity](../03-operations/05-capacity.md) |
| R2a cannot call an ungranted tool | R2a | pinned digest + `tool.not_granted` at a network choke point | [03](../architecture/03-tool-gateway.md) | S14, demo 8 |
| R2b credentials never enter the model context | R2b | broker, two sandboxes, `Secret` type, scrub | [05](../architecture/05-credential-plane.md) | S12–S13, demo 4–7 |
| R2c attributable to agent, tenant, user | R2c | audit chain records `agent_name`, `tenant_id`, `triggering_user` | [10](../architecture/10-observability.md) | S17, demo 21 |
| R3 Kubernetes-native | R3 | manifests, PSA, NetworkPolicy, RuntimeClass, RBAC, HPA/PDB, Argo CD | [08](../architecture/08-network-topology.md) | k8s-validate, kind-test, argocd-test |
| R4 sandboxed execution | R4 | namespaces/seccomp → gVisor | [04](../architecture/04-sandbox.md) | S1–S11 |
| R5 durability across restarts/deploys | R5 | event log, leases, fence, journal | [02](../architecture/02-scheduling-runtime.md), [07](../architecture/07-state-failover.md) | durability ×3 |
| R6 observability: what is X doing, what has it cost | R6 | event log = console; `runs.usage`; 11 metrics | [10](../architecture/10-observability.md) | console, `/v1/overview` |
| R7 decisions stated with alternatives | R7 | — | — | [DESIGN §7](../../DESIGN.md), [DEEP_DIVE](../../DEEP_DIVE.md), [reasoning](../reasoning/README.md) |
| Deliverable A (design ≤ 4 pages) | — | — | — | [DESIGN.md](../../DESIGN.md) |
| Deliverable B (runnable PoC + safety test + README real/faked/gaps) | — | — | — | `make demo`, [README](../../README.md), [demo walkthrough](../06-guides/08-demo-walkthrough.md) |
| Deliverable C (AI log) | — | — | — | [AI_LOG.md](../../AI_LOG.md) |

## C. Cross-cutting concerns

| Concern | Where it is covered |
|---|---|
| trust boundaries and threat model | [threat model](../00-problem/04-threat-model.md), [00 overall](../architecture/00-overall.md), [system diagram](../diagrams/system.md) |
| multi-tenancy (every enforcement point) | [multi-tenancy](../01-concepts/03-multi-tenancy.md), [onboarding](../06-guides/06-onboarding-a-tenant.md) |
| failure handling (every failure → mechanism) | [09 failure map](../architecture/09-failure-map.md), [DESIGN §6](../../DESIGN.md), [runbook](../03-operations/04-runbook.md) |
| prompt injection | [concept](../01-concepts/08-prompt-injection.md), [reasoning](../reasoning/10-prompt-injection.md), [incidents](../reasoning/14-industry-learnings.md) |
| identity (callers, workers, gateway, agents) | [secrets §7](../01-concepts/05-secrets.md), [credentials](../reasoning/06-credentials.md), [hardening #1, #6](../03-operations/07-production-hardening-checklist.md) |
| network topology and egress | [08](../architecture/08-network-topology.md), [Kubernetes](../reasoning/11-kubernetes.md) |
| data model, lifecycle, retention, deletion | [data model](../02-architecture/02-data-model.md), [events and states](07-events-and-states.md), [upgrades §7](../03-operations/06-upgrades-backup-and-rotation.md), [offboarding](../06-guides/06-onboarding-a-tenant.md) |
| cost: per run, per tenant, platform | [observability](../03-operations/03-observability.md), [capacity §9](../03-operations/05-capacity.md), [fairness](../reasoning/07-fairness.md) |
| configuration: every flag, env var, constant | [configuration](../02-architecture/04-configuration.md), [limits and defaults](05-limits-and-defaults.md) |
| errors: every status, rule, sentinel, reason | [errors and codes](06-errors-and-codes.md), [API](../02-architecture/03-api.md) |
| upgrades, schema, backup, restore, DR, rotation | [upgrades/backup/rotation](../03-operations/06-upgrades-backup-and-rotation.md) |
| testing: platform | [test inventory](09-test-inventory.md), [safety proofs](../04-evidence/02-safety-proofs.md), [benchmarks](../04-evidence/01-benchmarks.md), [bugs found](../04-evidence/03-bugs-found.md), [evals and testing part I](../reasoning/13-evals-and-testing.md) |
| testing: agents (evals) | [evals and testing part II](../reasoning/13-evals-and-testing.md) |
| dependencies and supply chain | [language and dependencies](../reasoning/12-language-and-dependencies.md) |
| what is real, faked and missing | [README](../../README.md), [hardening checklist](../03-operations/07-production-hardening-checklist.md), "designed, not built" tables in [07](../architecture/07-state-failover.md) and [09](../architecture/09-failure-map.md) |
| industry context and precedent | [DEEP_DIVE](../../DEEP_DIVE.md), [state of the art](../reasoning/15-state-of-the-art.md), [industry learnings](../reasoning/14-industry-learnings.md), [reading list](03-reading-list.md) |
| how the documentation itself is built and checked | [architecture README](../architecture/README.md) (diagram generator), [tools/check-links.py](../tools/check-links.py), [tools/check-coverage.py](../tools/check-coverage.py) |

## D. Reader roles — the questions each asks, and where each is answered

**Architect / reviewer of the design**
| Question | Answer |
|---|---|
| What is the whole system and where are the boundaries? | [00 overall](../architecture/00-overall.md), [DESIGN](../../DESIGN.md) |
| Why this runtime model and not Temporal / a pod per agent / actors? | [runtime model](../reasoning/01-runtime-model.md) |
| Why Postgres for everything? What breaks first at scale? | [durability substrate](../reasoning/03-durability-substrate.md), [capacity §8](../03-operations/05-capacity.md) |
| What would make you reverse each decision? | the "Would reverse if" section of every [reasoning](../reasoning/README.md) document |
| What did you deliberately not build? | [DESIGN §8](../../DESIGN.md), [hardening checklist](../03-operations/07-production-hardening-checklist.md) |

**Security reviewer**
| Question | Answer |
|---|---|
| Who are the adversaries and what are the attack trees? | [threat model](../00-problem/04-threat-model.md) |
| Exactly how is model-authored code contained? Which syscalls? | [04 sandbox](../architecture/04-sandbox.md), [syscalls](02-syscalls.md), [isolation](../reasoning/02-isolation.md) |
| How does `gh` get a token the agent cannot read? Where are the `Reveal()` sites? | [05 credential plane](../architecture/05-credential-plane.md), [credentials](../reasoning/06-credentials.md) |
| What stops prompt injection? What does not? | [prompt injection](../reasoning/10-prompt-injection.md), [concept §7](../01-concepts/08-prompt-injection.md) |
| What is the TCB and how big is it? | [00 overall §domain boundaries](../architecture/00-overall.md), [language](../reasoning/12-language-and-dependencies.md) |
| What does the cluster enforce independently of the code? | [08 topology](../architecture/08-network-topology.md) |
| Which tests would fail if a boundary regressed, and could they pass for the wrong reason? | [safety proofs](../04-evidence/02-safety-proofs.md), [test inventory](09-test-inventory.md), B2/B3 in [bugs found](../04-evidence/03-bugs-found.md) |
| What are the residual risks, ranked? | [README gaps](../../README.md), [hardening #1–5](../03-operations/07-production-hardening-checklist.md), [threat model §7](../00-problem/04-threat-model.md) |

**SRE / operator**
| Question | Answer |
|---|---|
| How do I run it locally, in kind, with Argo? | [running](../03-operations/01-running.md), [deployment](../03-operations/02-deployment.md) |
| Every knob and its default? | [configuration](../02-architecture/04-configuration.md), [limits and defaults](05-limits-and-defaults.md) |
| What pages me, and what do I do? | [observability](../03-operations/03-observability.md), [runbook](../03-operations/04-runbook.md), [09 failure map §incident](../architecture/09-failure-map.md) |
| How do I upgrade, migrate the schema, back up, restore, rotate secrets? | [upgrades/backup/rotation](../03-operations/06-upgrades-backup-and-rotation.md) |
| How much hardware and quota for N agents? | [capacity](../03-operations/05-capacity.md) |
| What happens when X dies? | [09 failure map](../architecture/09-failure-map.md), [07 state and failover](../architecture/07-state-failover.md) |
| What does each error/status/log line mean? | [errors and codes](06-errors-and-codes.md), [events and states](07-events-and-states.md) |

**Developer extending the platform**
| Question | Answer |
|---|---|
| Where is everything in the code? | [code map](../02-architecture/05-code-map.md), "Where to look" in every [architecture](../architecture/README.md) doc |
| How do I add a tool / a model provider / a sandbox driver? | [03](../06-guides/03-adding-a-tool.md), [04](../06-guides/04-adding-a-model-provider.md), [05](../06-guides/05-adding-a-sandbox-driver.md) |
| What are the invariants I must not break? | [07 state and failover §transactions](../architecture/07-state-failover.md), [events and states §invariants](07-events-and-states.md), [upgrades §1 compatibility contract](../03-operations/06-upgrades-backup-and-rotation.md) |
| How are the tests organised, what needs Postgres/root? | [test inventory](09-test-inventory.md) |
| Why one binary, one dependency? | [language and dependencies](../reasoning/12-language-and-dependencies.md) |
| How are the diagrams generated? | [architecture README](../architecture/README.md) |

**Tenant / agent author**
| Question | Answer |
|---|---|
| How do I write an agent definition and its policy? | [authoring agent definitions](../06-guides/01-authoring-agent-definitions.md) |
| Which tools exist and what do they need? | [tools catalogue](../06-guides/02-tools-catalogue.md) |
| Why was my tool call refused? | [errors §3 rules](06-errors-and-codes.md), `GET /v1/audit` |
| What do the run states and events mean? | [events and states](07-events-and-states.md) |
| How do I use the console? | [console](../06-guides/07-operator-console.md) |
| How is my tenant isolated and how is quota shared? | [multi-tenancy](../01-concepts/03-multi-tenancy.md), [fairness](../01-concepts/06-fairness.md) |

**Auditor / compliance**
| Question | Answer |
|---|---|
| Who did what, when, and can I prove the record was not altered? | [audit](../01-concepts/07-audit.md), [10 observability](../architecture/10-observability.md), [audit reasoning §what it does not prove](../reasoning/08-audit.md) |
| What is retained, for how long, and how is a tenant deleted? | [upgrades §7](../03-operations/06-upgrades-backup-and-rotation.md), [offboarding](../06-guides/06-onboarding-a-tenant.md) |
| Which secrets exist and how are they rotated? | [upgrades §6](../03-operations/06-upgrades-backup-and-rotation.md) |
| What was actually tested versus asserted? | [benchmarks §10](../04-evidence/01-benchmarks.md), [test inventory](09-test-inventory.md) |

**Finance / capacity planner**
| Question | Answer |
|---|---|
| Where does cost come from and how is it attributed? | [observability §2](../03-operations/03-observability.md), [limits §2 price table](05-limits-and-defaults.md) |
| What does 1 000 agents cost to run? | [capacity §7–9](../03-operations/05-capacity.md) |
| How is one provider limit shared fairly? | [fairness](../reasoning/07-fairness.md) |

**Someone learning the field**
| Question | Answer |
|---|---|
| What is an agent, physically? | [what an agent is](../00-problem/01-what-is-an-agent.md), [TUTORIAL](../../TUTORIAL.md) |
| What do namespaces, leases, hash chains, token buckets actually do? | [concepts](../01-concepts/) |
| Who else does this, and what went wrong for them? | [state of the art](../reasoning/15-state-of-the-art.md), [industry learnings](../reasoning/14-industry-learnings.md) |
| How should agents be evaluated? | [evals and testing](../reasoning/13-evals-and-testing.md) |

**Reviewer of the take-home itself**
| Question | Answer |
|---|---|
| What was asked, what was delivered, what is faked? | [README](../../README.md), [requirements decoded](../00-problem/03-requirements-decoded.md) |
| How was AI used? | [AI_LOG](../../AI_LOG.md) |
| Did running it find anything the design missed? | [bugs found](../04-evidence/03-bugs-found.md) |

## E. Known remaining holes (stated, not hidden)

| Hole | Why it remains | Where tracked |
|---|---|---|
| the ~270 external references were not fetched from the build environment | egress policy denied those hosts | [reasoning README](../reasoning/README.md) — run `check-links.py --external` |
| no live denied-connection probe on the cluster | test not yet written | [hardening #10](../03-operations/07-production-hardening-checklist.md) |
| real-model behaviour is not tested | by design: the platform's guarantees must hold for any output | [evals and testing](../reasoning/13-evals-and-testing.md) |
| the console has no screenshots in the docs | text description only | [console guide](../06-guides/07-operator-console.md) |

## F. Keeping it hole-free

```bash
python3 docs/tools/check-links.py        # every relative link and #anchor resolves
python3 docs/tools/check-coverage.py     # every env var, rule name, event type, state, tool, metric, make target and test is mentioned in the docs
```

Both run in seconds and exit non-zero on a miss. Adding a constant, a rule or
a tool without documenting it fails the second check; that is the mechanism
that keeps this matrix true after the next change.

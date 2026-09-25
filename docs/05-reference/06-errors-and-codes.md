# Errors, statuses and codes — the complete catalogue

> Every status code, decision, rule name, error sentinel, status reason and
> exit code the platform can produce, what each means, who sees it, and where
> it is raised. If something in a log, an audit record or an API response is
> not on this page, that is a documentation bug — file it.
>
> Related: [API reference](../02-architecture/03-api.md) · [tool gateway](../architecture/03-tool-gateway.md) · [runbook](../03-operations/04-runbook.md) · [events and states](07-events-and-states.md)

## 1. Control-plane HTTP statuses (`:8080`)

| Status | Endpoint(s) | Meaning | Body |
|---|---|---|---|
| **200** | every `GET`; `POST …/resume`, `/approve`, `/cancel` | success | the resource, or the updated run |
| **201** | `POST /v1/runs`, `POST /v1/agents` | created | the run / the definition (with `digest`) |
| **400** | `POST /v1/runs` | body undecodable; neither `agent_digest` nor `agent_name` given; agent name unknown for the tenant; unknown tenant | `{"error": "…"}` |
| **400** | `POST /v1/agents` | body undecodable; `name` missing; spec invalid (`model`/`system_prompt` empty, unknown tool name, bad priority) | `{"error": "…"}` |
| **400** | `POST …/resume` | `input` missing | |
| **401** | every `/v1/*` | `Authorization: Bearer <key>` missing or unknown | `{"error":"missing or invalid API key"}` |
| **403** | `POST /v1/runs` | the agent definition belongs to another tenant (non-operator caller) | |
| **404** | `GET /v1/runs/{id}`, `/events`, `/stream`; `POST …/resume|approve|cancel` | run does not exist **or belongs to another tenant** — deliberately indistinguishable | `{"error":"run not found"}` |
| **409** | `POST …/resume` | run is not `WAITING_HUMAN` | `{"error":"run is <state>, not WAITING_HUMAN"}` |
| **409** | `POST …/approve` | run is not `WAITING_APPROVAL` | |
| **409** | `POST …/cancel` | run already terminal | `{"error":"run has already finished"}` |
| **429** | `POST /v1/runs` | tenant at `max_concurrent_runs` | `{"error", "active_runs", "limit", "retry_after_seconds": 30}` |
| **500** | any | store error, streaming unsupported by the response writer | `{"error": "…"}` |
| **503** | `GET /healthz` | the store did not answer | `{"error":"store unavailable"}` |

`GET /metrics` and `GET /healthz` need no key. SSE (`/stream`) accepts the
key as `?key=` because `EventSource` cannot set headers — listed as gap 4 in
the README.

## 2. Tool-gateway HTTP statuses (`POST /v1/toolcalls`, `:8081`)

The response body is always `{decision, reason, result, is_error, replayed, truncated}`.

| Status | `decision` | When | What the **model** is told (`result`) |
|---|---|---|---|
| **401** | `DENY` | run JWT missing, bad signature, expired (> 5 min), wrong audience (`tool-gateway`) | — (the worker treats it as a transport error) |
| **400** | `DENY` | malformed body | — |
| **404** | `DENY` | `sub` names no run | "unknown run" |
| **403** | `DENY` | token's `tenant` ≠ the run's tenant (logged as a possible attack) | "token does not match run" |
| **424** | `DENY` | the run's pinned definition digest cannot be loaded | "the agent definition for this run is unavailable" |
| **403** | `DENY` | policy refused (rule below) | `Tool call refused: <reason>` |
| **202** | `NEEDS_APPROVAL` | `approval.required` and `approved=false` | `Tool call refused: "<tool>" requires human approval before it runs` — the worker parks the run |
| **503** | `DENY` | the idempotency journal could not be written | "could not journal the call; refusing to execute" |
| **409** | `ALLOW`, `replayed=true`, `is_error=true` | the same idempotency key is still `IN_FLIGHT` | "This tool call is already in progress and has not reported an outcome yet." |
| **200** | `ALLOW`, `replayed=true` | the key was already `DONE`/`FAILED` | the recorded result; the side effect was **not** repeated |
| **500** | `DENY` | workspace directory unavailable | "workspace unavailable" |
| **502** | `ALLOW`, `is_error=true` | the platform failed to run the tool (sandbox setup, broker, network) | "the platform failed to complete this tool call" + for `UnsafeRetry` tools: "This tool cannot be safely retried automatically: it MAY OR MAY NOT have taken effect. Verify the current state before trying again." |
| **200** | `ALLOW` | executed | the tool's output (`is_error` mirrors a non-zero exit / HTTP ≥ 400; `truncated` when capped) |

## 3. Authorization rule names (`authz.Decision.Rule`)

Evaluated in this order; the first hit wins. Every denial is audited with the
rule name, so the audit log and `tool_calls_total{decision="DENY"}` can be
filtered by it.

| Rule | Effect | Reason text (template) | Layer |
|---|---|---|---|
| `run.terminal` | DENY | `run is <STATE> and can no longer call tools` | lifecycle |
| `budget.exhausted` | DENY | `<step|tool-call|token|cost|wall-clock> budget exhausted (<used>/<max>); this run will now be stopped` | budget |
| `tool.unknown` | DENY | `no tool named "<name>" exists` | registry |
| `tool.not_granted` | DENY | `tool "<name>" is not in this agent's granted tool set (granted: …)` | grant |
| `args.missing` | DENY | `argument "<arg>" is required for <tool>` | schema |
| `params.host_not_allowed` | DENY | `host <h> is not in the allowed hosts (…)` | parameter policy |
| `params.command_not_allowed` | DENY | `command <argv0> is not in the allowed commands (…)` | parameter policy |
| `params.denied_pattern` | DENY | `arguments contain the denied pattern "<pat>"` | parameter policy |
| `params.bad_url` | DENY | `url is not parsable: …` / `url has no host` | URL check |
| `params.bad_scheme` | DENY | `scheme "<s>" is not permitted; use http or https` | URL check |
| `params.ssrf` | DENY | `<host> is a link-local, loopback or private address …` / `… is an instance metadata hostname` | SSRF check |
| `approval.required` | NEEDS_APPROVAL | `"<tool>" requires human approval before it runs` | approval |
| `args.invalid` | DENY | schema type error from `ValidateArgs` (applied after the grant, before execution) | schema |
| `default.granted` | ALLOW | — | — |

## 4. Go error sentinels

| Sentinel | Package | Raised when | Handled by |
|---|---|---|---|
| `types.ErrNotFound` | store | row missing (`AcquireLease` with an empty queue returns it too) | API → 404; worker → sleep |
| `types.ErrLeaseLost` | store | `RenewLease`/`Commit` found `lease_owner ≠ me` or expired | worker abandons the step silently ("another worker has taken over") |
| `types.ErrConflict` | store | `Commit` found `next_seq ≠ expected` | worker abandons the step (stale replay) |
| `types.ErrDenied` | authz/gateway | policy denial surfaced as an error | gateway → 403 |
| `llm.ErrQuotaUnavailable` | llm | `Reserve()` returned false | worker requeues, `wake_at = now+2 s` |
| `llm.ErrRateLimited` | llm provider | provider 429 | retryable → backoff |
| `llm.ErrOverloaded` | llm provider | provider 529/503 "overloaded" | retryable → backoff |
| `creds.ErrNoSuchRef` | creds | tenant has no root for the tool's `CredRef` | gateway → result "needs a credential that is not configured for your tenant", `is_error` |
| `sandbox.ErrUnsupported` | sandbox | driver cannot run here (no docker, not in-cluster) | `sandbox.New` fails at startup |
| `sandbox.ErrSetup` | sandbox | jail/cgroup/pod setup failed (exit 125 + marker) | gateway → 502 "platform failed" |
| `jwtmini.ErrMalformed`, `ErrSignature`, `ErrExpired`, `ErrAudience` | jwtmini | run-token verification: not three base64url segments / bad HMAC / past `exp` / wrong `aud` | gateway → 401 |
| `llm.ErrBadRequest` | llm provider | the provider rejected the request permanently (4xx other than 429: bad model id, context too long) | **not retried**; the run's step fails with the reason |
| `types.ErrInFlight` | types/gateway | an idempotency key is still `IN_FLIGHT` | gateway → 409 (replay response) |
| `types.ErrQuotaExceeded` | types | a tenant budget/quota check failed outside the limiter path (reserved for admission-style checks) | API → 429 |
| `creds.ErrRevoked` | creds | `Verify` was asked about a credential id that has been revoked | the fake API refuses the token; a real provider would return 401 to `gh` |

## 5. Run `status_reason` values

| State | `status_reason` | Set by |
|---|---|---|
| `QUEUED` | `""` (new) | API `createRun` |
| `QUEUED` | `yielded after 4 steps` | worker (`MaxStepsPerLease`) |
| `QUEUED` | `worker yielded mid-step` | worker's deferred `YieldRun` |
| `QUEUED` | `lease expired; reclaimed` | reaper |
| `QUEUED` | `waiting for LLM quota` | worker (`ErrQuotaUnavailable`, `wake_at +2 s`) |
| `QUEUED` | `model provider unavailable: <first line>` | worker (`wake_at +10 s`) |
| `WAITING_HUMAN` | `waiting for human input` | worker (`stop_reason = human_input_required`) |
| `WAITING_APPROVAL` | `waiting for human approval of a tool call` | worker (`NEEDS_APPROVAL`) |
| `SUCCEEDED` | `completed` | worker (no tool calls in the response) |
| `FAILED` | `<budget> budget exhausted (<used>/<max>)` | worker (`ExceedsReason`) |
| `CANCELLED` | `cancelled by <user>` | API `cancelRun` |

## 6. `tool_calls.state`

| State | Meaning | Transition |
|---|---|---|
| `IN_FLIGHT` | reserved; side effect may be happening | `BeginToolCall` |
| `DONE` | executed, result recorded | `FinishToolCall` |
| `FAILED` | executed with `is_error`, infrastructure failure, or reaped as "outcome unknown" | `FinishToolCall` / reaper (`started_at < now − stuck_after`) |
| `DENIED` | reserved for calls denied after journaling (unused today: denials happen before stage 3) | — |

## 7. Sandbox exit-code classification

| Exit | Meaning | Reported as |
|---|---|---|
| `0` | payload succeeded | `is_error=false` |
| `1–124`, `128+n` | payload failed / killed by signal n | `is_error=true`, the model sees exit code and stderr |
| **`125` + stderr marker `agentorch-sandbox-setup-error:`** | **our** jail failed to build | `ErrSetup` → 502 "platform failed", never blamed on the agent |
| `126` / `127` | shell: not executable / not found | `is_error=true`, "your script exited 127" |
| — | wall-clock exceeded | `TimedOut=true`, "killed after <wall>" |
| — | OOM | `OOMKilled=true`, "killed after exceeding its <mem> memory limit" |
| — | output cap | `Truncated=true`, the model is told output was capped |

## 8. Kubernetes-driver pod failures

`ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError`,
`InvalidImageName` are detected while waiting on the pod and returned as
`ErrSetup` (502 to the agent). `activeDeadlineSeconds = wall + 5 s` bounds a
pod that never reports.

## 9. Fake-provider scenarios (`model: "fake:<scenario>"`)

`report-writer` (default), `github-publisher`, `needs-human`, `poison-loop`,
`injected`, `load`. Unknown scenarios behave like `report-writer`. `--model-fail-every N`
makes every Nth call return a retryable error to exercise the backoff and
requeue paths. See the [demo walkthrough](../06-guides/08-demo-walkthrough.md).

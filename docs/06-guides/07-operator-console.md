# The operator console

> A React single-page app served by the control plane at `/`, built from
> `ui/` with Vite and embedded into the binary (`--ui-dir` for development).
> It is a projection of the same API and event log the platform itself uses:
> nothing in it is computed client-side from a parallel data source, so it
> cannot disagree with the audit trail or the model's context.
>
> Code: [`ui/src/`](../../ui/src/) — `App.tsx`, `api.ts`, `components/{Overview,Runs,RunDetail,Audit,Quota,common}.tsx` · Related: [observability](../architecture/10-observability.md) · [API](../02-architecture/03-api.md)

## Identity

The header carries an identity switcher backed by the seeded API keys:
**operator (all tenants)** `demo-operator-key`, **alice @ acme** `acme-key`,
**bob @ globex** `globex-key`. The chosen key is sent as `Authorization:
Bearer` on every call and as `?key=` on the SSE stream (EventSource cannot set
headers — README gap 4). Switching identity re-fetches every view; a tenant
identity sees only its own runs, audit records and quota row; the operator
sees all and can pick a tenant filter.

## Views

| View | What it shows | Source | Actions |
|---|---|---|---|
| **Fleet overview** | summary cards (runs, active, tool calls, spend); **Cost by tenant** (runs, tool calls, tokens, USD); **Runs by state** | `GET /v1/overview` (aggregates `runs.usage`) | — |
| **Runs** | table: state, agent, tenant, triggered by, cost, worker (lease owner), age; filter by state/tenant | `GET /v1/runs?limit=…` | open a run |
| **Run detail** | header with state and `status_reason`, pinned definition digest, lease owner; **Cost and budget** (usage vs each budget field); **Timeline (N events)** rendered live | `GET /v1/runs/{id}`, `GET …/events`, `GET …/stream` (SSE, resumes from last seq) | **Resume** (when `WAITING_HUMAN`; sends input) · **Approve** (when `WAITING_APPROVAL`) · **Cancel** |
| **Audit log** | table: #, time, decision, tool, agent, triggered by, arguments (redacted), reason; filter by tenant/run/time; chain verification result | `GET /v1/audit` | verify |
| **LLM quota and fairness** | per tenant: weight, guaranteed tokens/min, bucket fill bar, in-flight calls, throttled status; the shared spare pool | `GET /v1/quota` (`limiter.Snapshot()`) | — |

## Reading the timeline

Each event row shows its `seq`, type, and the payload fields that matter for
that type ([events reference](../05-reference/07-events-and-states.md)):
`MODEL_RESPONSE` shows text, tokens and cost; `TOOL_CALL` the tool and
arguments; `TOOL_RESULT` the result and duration; `TOOL_DENIED` the rule and
reason in red; `APPROVAL_NEEDED` the pending call with the approve button;
`NOTE` and `LEASE_LOST` in grey (operator-only information the model never
saw). Because the console rebuilds the timeline from the same rows the worker
rebuilds the model's context from, "what did the agent see when it did that?"
is answered by scrolling up.

## What operators can and cannot do from the console

Can: watch any run live, resume/approve/cancel, read every decision with its
rule, see who is being throttled and why, verify a tenant's audit chain.
Cannot: create runs or definitions for a tenant (the API refuses operator
writes on a tenant's behalf), see credentials (there are none to see), change
policy (definitions are immutable; a new one is a `POST` with the tenant's
key).

## Development

```bash
make build-ui            # vite build → ui/dist (embedded by make run / the image)
cd ui && npm run dev     # dev server with proxy to :8080
cd ui && npm run typecheck
```

The UI is **not** part of the trusted computing base: it runs in the
operator's browser with the operator's key and can do nothing the API would
not let that key do. Its npm dependency tree is therefore outside the
one-external-module policy that applies to the binary
([language and dependencies](../reasoning/12-language-and-dependencies.md)).

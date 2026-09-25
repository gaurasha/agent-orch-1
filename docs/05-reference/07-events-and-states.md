# Events and states — the complete reference

> The fourteen event types, who emits each, what its payload carries, how
> `Rebuild()` turns it into a model message, and how the console shows it; the
> seven run states and every transition with the actor that performs it; the
> SSE frame format. Complements the [data model](../02-architecture/02-data-model.md)
> (storage shape) and [02-scheduling-runtime](../architecture/02-scheduling-runtime.md)
> (the loop).

## 1. Event types

`events(run_id, seq, type, payload JSONB, created_at)`. `seq` starts at 0 and is
dense; `runs.next_seq` is always `max(seq)+1`. The payload is one flat union
type (`types.EventPayload`); each event uses a subset of its fields.

| Type | Emitted by | When | Payload fields used | `Rebuild()` → model | Console |
|---|---|---|---|---|---|
| `RUN_CREATED` | API `createRun` | seq 0, in the same txn as the row | `text` (the input, default "Begin.") | **user** message | "created by <user>" |
| `USER_MESSAGE` | API (reserved for multi-turn input) | — | `text` | user message | shown |
| `MODEL_REQUEST` | worker (optional; not written by default to keep the log small) | before a model call | `model` | — | shown if present |
| `MODEL_RESPONSE` | worker `step()` | after every model call | `text`, `model`, `input_tokens`, `output_tokens`, `cost_usd`, `stop_reason`, `worker`; tool calls are carried by the following `TOOL_CALL` events | **assistant** message (+ its tool calls) | text, tokens, cost |
| `TOOL_CALL` | worker, before calling the gateway | one per tool call the model requested | `tool_call_id`, `tool`, `args`, `idem_key`, `worker` | attached to the preceding assistant message as a tool call | tool + args |
| `TOOL_RESULT` | worker, after the gateway answered `ALLOW` | | `tool_call_id`, `tool`, `result`, `is_error`, `duration_ms`, `decision`, `idem_key` | **tool** message (`tool_call_id`, `is_error`) | result, duration |
| `TOOL_DENIED` | worker, after `DENY` | | same as above with `decision=DENY`, `reason` | tool message: `Tool call refused: <reason>` — so the model can re-plan | red badge + rule |
| `APPROVAL_NEEDED` | worker, after `NEEDS_APPROVAL` | the run parks | `tool_call_id`, `tool`, `args`, `reason`, `idem_key` | — (the call has not happened) | approve button |
| `APPROVAL_GIVEN` | API `approveRun` | human approved | `text` (who), `tool_call_id` | — | shown |
| `HUMAN_PAUSE` | worker | model returned `stop_reason=human_input_required` | `text` (the model's question), `worker` | assistant message | resume form |
| `HUMAN_RESUME` | API `resumeRun` | human answered | `text` (the answer) | **user** message | shown |
| `LEASE_LOST` | worker (best effort — only if it can still write, which by definition it usually cannot) | fence rejected a commit | `worker`, `reason` | — | shown |
| `RUN_FINISHED` | worker (success / budget failure), API (`cancelRun`) | terminal | `text`, `reason`, `worker` | — | final state |
| `NOTE` | worker | yields after the step budget, quota waits, requeues | `text`, `worker` | — (operators only) | grey note |

**What the model never sees:** `NOTE`, `LEASE_LOST`, worker ids, idem keys,
costs. **What the operator sees that the model does not:** all of those. Both
projections come from the same rows.

### Payload field reference (`types.EventPayload`)

| Field | JSON | Type | Used by |
|---|---|---|---|
| Text | `text` | string | RUN_CREATED, USER_MESSAGE, MODEL_RESPONSE, HUMAN_PAUSE, HUMAN_RESUME, RUN_FINISHED, NOTE, APPROVAL_GIVEN |
| Model | `model` | string | MODEL_REQUEST, MODEL_RESPONSE |
| InputTokens / OutputTokens | `input_tokens`, `output_tokens` | int64 | MODEL_RESPONSE |
| CostUSD | `cost_usd` | float | MODEL_RESPONSE |
| StopReason | `stop_reason` | string | MODEL_RESPONSE (`end_turn`, `tool_use`, `human_input_required`) |
| ToolCallID | `tool_call_id` | string | TOOL_CALL, TOOL_RESULT, TOOL_DENIED, APPROVAL_NEEDED, APPROVAL_GIVEN |
| Tool | `tool` | string | same |
| Args | `args` | object | TOOL_CALL, APPROVAL_NEEDED |
| IdemKey | `idem_key` | string | TOOL_CALL, TOOL_RESULT, TOOL_DENIED, APPROVAL_NEEDED (`run:step:i`) |
| Result | `result` | string | TOOL_RESULT, TOOL_DENIED |
| IsError | `is_error` | bool | TOOL_RESULT |
| DurationMS | `duration_ms` | int64 | TOOL_RESULT |
| Decision | `decision` | string | TOOL_RESULT (`ALLOW`), TOOL_DENIED (`DENY`), transport failure (`ERROR`) |
| Reason | `reason` | string | TOOL_DENIED, APPROVAL_NEEDED, RUN_FINISHED, LEASE_LOST |
| Worker | `worker` | string | anything a worker writes |

## 2. Run states and transitions

| From | To | Actor | Mechanism | `status_reason` |
|---|---|---|---|---|
| — | `QUEUED` | API | `CreateRun` (one txn with `RUN_CREATED`) | |
| `QUEUED` | `RUNNING` | worker | `AcquireLease` (`FOR UPDATE SKIP LOCKED`; `wake_at ≤ now()`; tenant eligible) | |
| `RUNNING` | `RUNNING` | worker | `Commit` with `step+1` (tool calls happened, run continues) | |
| `RUNNING` | `QUEUED` | worker | yield after `MaxStepsPerLease` (`Commit` + `NOTE`) | `yielded after 4 steps` |
| `RUNNING` | `QUEUED` | worker | deferred fenced `YieldRun` on shutdown/error | `worker yielded mid-step` |
| `RUNNING` | `QUEUED` | worker | `requeue(wake_at)` on quota / provider outage | `waiting for LLM quota` / `model provider unavailable: …` |
| `RUNNING` | `QUEUED` | reaper | `ReapExpiredLeases` (`lease_expires_at < now()`, or ownerless > 60 s) | `lease expired; reclaimed` |
| `RUNNING` | `WAITING_HUMAN` | worker | `HUMAN_PAUSE`, lease released | `waiting for human input` |
| `RUNNING` | `WAITING_APPROVAL` | worker | `APPROVAL_NEEDED`, lease released | `waiting for human approval of a tool call` |
| `WAITING_HUMAN` | `QUEUED` | API | `resumeRun` → `HUMAN_RESUME` (`AppendSystemEvents`, no fence needed) | |
| `WAITING_APPROVAL` | `QUEUED` | API | `approveRun` → `APPROVAL_GIVEN`; the worker retries the **same** idem key with `approved=true` | |
| `RUNNING` | `SUCCEEDED` | worker | model response with no tool calls → `RUN_FINISHED` | `completed` |
| `RUNNING` | `FAILED` | worker | `Budget.ExceedsReason` at step start → `RUN_FINISHED` | `<budget> exhausted (…)` |
| any non-terminal | `CANCELLED` | API | `cancelRun` → `RUN_FINISHED`; a straggling worker's next tool call is refused (`run.terminal`) and its commit is fenced | `cancelled by <user>` |

Terminal states: `SUCCEEDED`, `FAILED`, `CANCELLED` (`RunState.Terminal()`).
Parked states (no lease, no worker): `QUEUED`, `WAITING_HUMAN`,
`WAITING_APPROVAL`. Only `RUNNING` has an owner.

Invariants a reader can check with SQL (the [runbook](../03-operations/04-runbook.md) has the queries):

* `state='RUNNING'` ⇒ `lease_owner IS NOT NULL` — violated only transiently;
  the reaper's second clause repairs it within 60 s.
* `next_seq = (SELECT max(seq)+1 FROM events WHERE run_id = runs.id)` — always.
* a `TOOL_RESULT` with `decision='ALLOW'` has exactly one `tool_calls` row with
  that `idem_key` in `DONE`/`FAILED`.

## 3. Lease columns

| Column | Set | Cleared |
|---|---|---|
| `lease_owner` | `AcquireLease` (worker id) | `Commit` with `ReleaseLease`, `YieldRun`, reaper |
| `lease_expires_at` | `AcquireLease` (`now()+TTL`), `RenewLease` every TTL/3 | same |
| `wake_at` | `requeue()` (`now()+2 s` / `+10 s`) | next `AcquireLease` ignores it once passed |

## 4. SSE frame format (`GET /v1/runs/{id}/stream`)

```
: connected
data: {"run_id":"run_…","seq":0,"type":"RUN_CREATED","payload":{"text":"…"},"created_at":"…"}

data: {"run_id":"run_…","seq":1,"type":"MODEL_RESPONSE","payload":{…}}

: keepalive
```

One `data:` frame per event, JSON-encoded exactly as `GET /v1/runs/{id}/events`
returns it; frames are emitted in `seq` order; the server polls the store every
400 ms; a client reconnects with `?since=<last seq + 1>` and sees no gap and no
duplicate. Keepalive comments are sent while idle. The stream ends when the
client disconnects; it does not end when the run finishes (the final
`RUN_FINISHED` is delivered and the client may close).

# The tool call path

Every tool call in the system goes through `gateway.process`. This is the
sequence, with the answer to "what can the agent see at each step" beside it.

```mermaid
sequenceDiagram
  autonumber
  participant MODEL as LLM
  participant W as agentd worker
  participant G as Tool gateway
  participant P as Policy engine
  participant J as Idempotency journal
  participant A as Audit chain
  participant B as Credential broker
  participant X as Sandbox / third party

  MODEL-->>W: tool_call{name, args, id}
  Note right of MODEL: The model saw ONLY tool SCHEMAS:<br/>name, description, parameter shapes.<br/>No URL template, no credential ref,<br/>no internal hostname.

  W->>W: local pre-check against granted tools
  Note right of W: Defence in depth only. The worker<br/>holds a run-scoped JWT and NO<br/>credentials of any kind.

  W->>G: POST /v1/toolcalls + Bearer(run JWT)<br/>idem_key = run:step:index

  G->>G: 1. verify JWT (sig, exp, audience)
  G->>G: 2. reload run + PINNED definition digest
  Note right of G: Permissions come from the digest the<br/>run pinned, so editing an agent cannot<br/>widen a run already in flight.
  G->>G: 3. cross-check token tenant == run tenant

  G->>P: 4. evaluate
  Note right of P: terminal? → budget? → tool exists?<br/>→ GRANTED? → required args?<br/>→ parameter policy (host allowlist,<br/>argv allowlist, denied patterns)<br/>→ human approval needed?

  alt DENY
    P-->>G: DENY + human-readable reason
    G->>A: audit(DENY, rule, redacted args)
    Note right of A: Denials are audited as thoroughly<br/>as successes. NO credential was<br/>minted — the decision happens first.
    G-->>W: 403 + the reason
    W-->>MODEL: "Tool call refused: <reason>"
    Note right of MODEL: Telling the model exactly why stops<br/>the retry-the-same-thing loop.
  else ALLOW
    G->>J: 5. BeginToolCall(idem_key)
    alt key already present
      J-->>G: recorded result
      G-->>W: replayed result, side effect NOT repeated
    else fresh
      G->>A: 6. audit(pre) — before any side effect
      G->>B: 7. mint credential (≤60s, tenant+ref scoped)
      B-->>G: Secret (redacts through fmt/JSON/errors)
      G->>X: 8. execute
      Note right of X: API tool → gateway makes the call<br/>with an injected header.<br/>Exec tool → sandbox, NO network.<br/>CLI tool → SEPARATE broker sandbox<br/>that holds the credential.
      X-->>G: result
      G->>G: 9. cap size, scrub the credential value
      G->>J: FinishToolCall(DONE/FAILED)
      G->>A: 10. audit(post) — status, bytes, duration, cred id
      G->>B: revoke the minted credential
      G-->>W: result
    end
  end
  W->>W: append TOOL_RESULT to the event log
  W-->>MODEL: result enters context on the next turn
```

## What the agent can and cannot see, per step

| Step | Agent sees | Agent does **not** see |
|---|---|---|
| Model request | Tool schemas for its **granted** tools only | Other tools, URL templates, credential refs, internal hostnames |
| Worker process | Tool args and results; a run-scoped JWT (≤5 min, audience-bound) | Any tenant credential — the worker has none to leak |
| Policy decision | The human-readable reason for a denial | The rule set, other tenants' policy, whether a credential exists |
| Exec sandbox | `/work`, the command, a fixed safe environment | The network, the host filesystem, any credential, other processes |
| Broker sandbox | — *(the agent has no handle on this process at all)* | n/a |
| Result | Output, capped and scrubbed | Credential values, response headers, gateway internals |

## Where authorization lives — and why in three places

```mermaid
flowchart LR
  A["1. Compiled into<br/>the tool schema"] -->|"cost + UX<br/>NOT security"| B
  B["2. THE GATEWAY<br/>authoritative"] -->|"blast radius<br/>if (2) is wrong"| C
  C["3. At the tool<br/>(fine-grained token,<br/>scoped IAM role)"]

  style B fill:#2a1f3a,stroke:#9f4fc9,stroke-width:3px,color:#e8d0f0
```

1. **In the schema** — the model is only shown tools it may call. Saves tokens
   and avoids noise. **Not** security: a model can emit any name it likes, and
   an injected prompt actively will.
2. **At the gateway** — the enforcement point, because it is the only place
   that sees every call, cannot be bypassed (NetworkPolicy gives workers egress
   to nothing else), and is small enough to audit as one component.
3. **At the tool** — a GitHub fine-grained token scoped to one repository bounds
   the damage if our own policy is wrong. Necessary but **not sufficient alone**:
   it gives no central audit trail and delegates correctness to N third parties.

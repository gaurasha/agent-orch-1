# System diagram and trust boundaries

```mermaid
flowchart TB
  subgraph EXT["UNTRUSTED — outside our control"]
    USER["Human user<br/>(OIDC)"]
    LLM["LLM provider<br/>external, rate-limited,<br/>largest cost line"]
    THIRD["Third-party APIs<br/>GitHub, internal services"]
  end

  subgraph CP["TRUST BOUNDARY 1 — PLATFORM (namespace: agentorch, PSA restricted)"]
    API["Control plane<br/>REST + SSE<br/>tenancy enforced per route"]
    WORKER["agentd workers<br/>STATELESS pool<br/>no credentials<br/>egress: gateway + DB only"]
    MGW["Model gateway<br/>quota · backoff · cost"]
    FAIR["Fairness limiter<br/>weighted max-min"]
    GW["TOOL GATEWAY<br/>== THE CHOKE POINT ==<br/>authz · credentials · audit"]
    BROKER["Credential broker<br/>mints short-TTL,<br/>tenant-scoped tokens"]
    PROXY["Egress proxy<br/>per-run domain allowlist"]
  end

  subgraph DATA["TRUST BOUNDARY 2 — STATE"]
    PG[("Postgres<br/>runs · event log ·<br/>idempotency journal ·<br/>audit hash chain")]
  end

  subgraph SBX["TRUST BOUNDARY 3 — SANDBOXES (namespace: agentorch-sandboxes)"]
    direction LR
    AGENTSBX["AGENT SANDBOX<br/>model-authored code<br/>NO network<br/>NO credentials<br/>read-only rootfs<br/>gVisor + seccomp"]
    BROKERSBX["BROKER SANDBOX<br/>trusted binary only<br/>egress via proxy<br/>HOLDS the credential<br/>separate pid/user ns"]
  end

  USER -->|"HTTPS + OIDC"| API
  API -->|"create run"| PG
  WORKER <-->|"lease · replay · commit<br/>(fenced by lease)"| PG
  WORKER -->|"run-scoped JWT"| GW
  WORKER --> MGW
  MGW <--> FAIR
  MGW -->|"tokens"| LLM

  GW -->|"decide BEFORE minting"| BROKER
  GW -->|"hash-chained record"| PG
  GW -->|"exec tool"| AGENTSBX
  GW -->|"credentialed CLI"| BROKERSBX
  GW -->|"API tool + injected cred"| THIRD

  BROKERSBX -->|"allowlisted hosts only"| PROXY
  PROXY --> THIRD
  AGENTSBX <-.->|"shared /work<br/>(the ONLY channel)"| BROKERSBX

  classDef untrusted fill:#3a1f1f,stroke:#c94f4f,color:#f0d0d0
  classDef platform  fill:#1f2a3a,stroke:#4f8fc9,color:#d0e0f0
  classDef state     fill:#1f3a2a,stroke:#4fc97f,color:#d0f0e0
  classDef sandbox   fill:#3a2f1f,stroke:#c9964f,color:#f0e4d0
  classDef chokepoint fill:#2a1f3a,stroke:#9f4fc9,color:#e8d0f0,stroke-width:3px

  class USER,LLM,THIRD untrusted
  class API,WORKER,MGW,FAIR,BROKER,PROXY platform
  class GW chokepoint
  class PG state
  class AGENTSBX,BROKERSBX sandbox
```

## What crosses each boundary, and what cannot

| Boundary | Crosses inward | Crosses outward | Enforced by |
|---|---|---|---|
| User → Platform | OIDC identity, agent definition, run input | Run status, event log, audit records — **tenant-scoped on every route** | `api.mustRun` resolves the caller to a tenant; cross-tenant reads return **404, not 403**, so run ids cannot be probed |
| Worker → Gateway | Tool name + arguments + deterministic idempotency key + run-scoped JWT | Tool result (size-capped, credential-scrubbed) | HMAC JWT bound to `audience=tool-gateway`; NetworkPolicy `agentd-egress` permits the gateway and Postgres only |
| Gateway → Sandbox | argv, environment allowlist, workspace bind-mount | stdout/stderr (capped), exit code | Empty network namespace; read-only rootfs; cgroup + rlimit; seccomp |
| Agent sandbox ↔ Broker sandbox | **Nothing but files in `/work`** | **Nothing but files in `/work`** | Different pid + user namespaces: no `/proc/<pid>/environ`, no `ptrace` |
| Gateway → Third party | Request with an injected, freshly minted, ≤60s credential | Response body | Credential exists only in the gateway's memory or the broker sandbox's environment |

## The three things that never cross a boundary

1. **A credential never reaches the agent.** Not in the model context, not in the worker process, not in the agent's sandbox environment. The `creds.Secret` type redacts through `fmt`, `json.Marshal`, `%#v` and error wrapping; `grep -rn '.Reveal()'` enumerates every plaintext use site (**three**: output scrubbing, gateway-side header injection, and the broker sandbox's environment — asserted by a test).
2. **A tenant never reaches another tenant.** Every store query is tenant-scoped, workspaces are `<root>/<tenant>/<run>`, credential root secrets are per tenant, audit chains are per tenant, and sandbox host uids are per tenant.
3. **A sandbox never reaches the network.** Not a firewall rule that can be misconfigured — an empty network namespace with no interface, no address and no route.

# Onboarding (and offboarding) a tenant

> A tenant is a row, a fairness weight, an identity mapping and a set of
> credential roots. Nothing else in the platform is per tenant: NetworkPolicy,
> sandboxes, the gateway and the workers are shared and isolate by the tenant
> id carried on every run. This guide is the exact sequence, what each step
> changes, and how to verify it.
>
> Related: [multi-tenancy](../01-concepts/03-multi-tenancy.md) · [fairness](../architecture/06-model-plane.md) · [credential plane](../architecture/05-credential-plane.md) · [limits and defaults §7](../05-reference/05-limits-and-defaults.md)

## 1. The tenant row

```sql
INSERT INTO tenants (id, name, weight, tokens_per_minute, max_concurrent_runs)
VALUES ('initech', 'Initech LLC', 1, 150000, 300);
```

| Column | Effect | How to choose |
|---|---|---|
| `id` | appears on every run, event, audit record, metric label and workspace path; **immutable** | short, lowercase, stable |
| `weight` | guaranteed share of the provider rate = `weight / Σ weights × provider_rate` | plan tier: 1 = standard, 2 = double share |
| `tokens_per_minute` | the tenant's own plan cap; `min(cap, share)` is the guarantee, the remainder becomes spare | the contract |
| `max_concurrent_runs` | admission control: `POST /v1/runs` → 429 beyond it | expected peak agents + headroom; this bounds queue depth, not throughput |

The limiter re-reads `tenants` every **5 s** (`serve.go`), so the new tenant's
bucket exists within seconds and every other tenant's guarantee shrinks
accordingly — adding a tenant must reduce the others, or guarantees would sum
to more than the provider gives. The PoC seeds tenants in `seed.go`; production
inserts through migration tooling or an admin endpoint (not built).

## 2. Identity → principal

Today: `api.Server.AddAPIKey(key, Principal{TenantID, User, Operator})`, an
in-memory map (seeded with `acme-key`, `globex-key`, `demo-operator-key`).
Production: OIDC — the principal is built from the token's claims (`tenant`
claim → `TenantID`, `sub`/`email` → `User`, a group → `Operator`). The rest of
the API only ever sees a `Principal`, so this is the single seam.

Rules that hold regardless of source: a principal without `Operator` can
read and write **only** its tenant; foreign runs are 404, not 403; operators
read across tenants with `?tenant=` and never act on a tenant's behalf.

## 3. Credential roots

For every `CredRef` the tenant's agents will use, register a root with the
broker: `broker.AddRoot(tenantID, ref, kind, key)` — e.g. `github/token`
(`github_token`), `http/default` (`bearer`). A tenant with no root for a
tool's `CredRef` gets *"needs a credential that is not configured for your
tenant"*; nothing is minted. In production the root is the tenant's GitHub
App installation / STS role / Vault path, and `Mint` calls it; the interface
(`Mint`, `Revoke`, `Verify`) is the seam.

## 4. First agent definition

`POST /v1/agents` with the tenant's key ([authoring guide](01-authoring-agent-definitions.md)).
Definitions are per tenant; a tenant cannot run another tenant's definition
(403 at `POST /v1/runs`).

## 5. Verify

```bash
curl -s -H 'Authorization: Bearer <initech-key>' localhost:8080/v1/quota      # the bucket exists, guaranteed tpm as expected
curl -s -H 'Authorization: Bearer <initech-key>' localhost:8080/v1/runs       # [] — and acme's runs are not visible
curl -s -H 'Authorization: Bearer <acme-key>'    localhost:8080/v1/runs/<initech run> # 404
```

Then one run through the whole stack, and `GET /v1/audit` shows records with
`tenant_id = initech` and nothing else.

## 6. What does **not** change per tenant

NetworkPolicy, RBAC, the sandbox namespace, node pool and quota, the gateway,
the workers, the store. Tenant isolation is a property of the tenant id on
every row and every check, not of per-tenant infrastructure. The one
production consideration is the sandbox **ResourceQuota**: it is a namespace
ceiling shared by all tenants; a per-tenant cap on concurrent sandboxes is
`max_concurrent_runs` × tool concurrency, which the capacity doc sizes.

## 7. Offboarding

1. Set `max_concurrent_runs = 0` (new runs → 429) and cancel active runs
   (`POST …/cancel`; stragglers are refused at the gateway with `run.terminal`).
2. Revoke the tenant's credential roots at the provider; remove them from
   the broker.
3. Export what retention requires: `events` (run history) and `audit_log`
   (the chain — export **with** `prev_hash`/`hash` so it stays verifiable).
4. `DELETE FROM tenants WHERE id = …` cascades to `agent_definitions`, `runs`
   and `events`. **`audit_log` and `tool_calls` have no FK on purpose** — the
   security record and the idempotency journal outlive the tenant; purge them
   by the retention policy, not by cascade.
5. Remove the identity mapping; delete the workspace directory tree
   `<workspace-dir>/<tenant>/`.

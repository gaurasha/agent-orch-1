# Authoring agent definitions

> An agent definition is the **contract** between a tenant and the platform:
> what the model is told, which tools it may call, how each tool's arguments
> are constrained, how much it may spend, and how urgent it is. It is
> content-addressed (its digest is its identity) and immutable; a run pins the
> digest it was created with. This guide gives the full schema, the six seeded
> examples, and the policy-writing rules that keep an agent contained.
>
> Related: [tools catalogue](02-tools-catalogue.md) · [authorization](../01-concepts/04-authorization.md) · [API: `POST /v1/agents`](../02-architecture/03-api.md) · [data model](../02-architecture/02-data-model.md)

## 1. The schema

```jsonc
POST /v1/agents            Authorization: Bearer <tenant api key>
{
  "name": "release-publisher",             // unique per tenant; used by POST /v1/runs {"agent_name": …}
  "spec": {
    "system_prompt": "You prepare and publish release notes to GitHub.",
    "model": "claude-sonnet-5",            // a provider model id; "fake:<scenario>" in the PoC
    "tools": ["fs.write", "fs.read", "exec.bash", "github.cli"],   // the GRANT — anything else is refused
    "tool_params": {                       // per-tool parameter policy, keyed by tool name
      "github.cli": {
        "allowed_commands":    ["pr", "issue", "repo"],       // argv[0] allowlist (CLI tools)
        "denied_arg_patterns": ["auth token", "auth status", "secret", "variable"],
        "requires_approval":   false
      },
      "http.get": {
        "allowed_hosts":       ["api.github.com", ".acme.example"]   // exact host or ".suffix"
      }
    },
    "budget": {
      "max_steps":        10,              // model calls
      "max_tool_calls":   20,
      "max_tokens":       100000,          // input + output
      "max_cost_usd":     1.0,
      "max_wall_seconds": 600              // from run creation, including time parked
    },
    "priority": "interactive"              // interactive | normal | batch
  }
}
→ 201 { "digest": "sha256:…", "tenant_id": "acme", "name": "…", "spec": {…}, "created_at": "…" }
```

### Field by field

| Field | Required | Meaning | Enforced where |
|---|---|---|---|
| `system_prompt` | yes | the fixed instructions; the run's input is the first user message | sent to the model each step (built by `buildSystemPrompt`) |
| `model` | yes | provider model id; priced from the [price table](../05-reference/05-limits-and-defaults.md) — unknown ids are billed at the most expensive rate | model gateway |
| `tools` | yes (may be empty) | the **only** tools the agent may call; names must exist in the registry or `POST /v1/agents` returns 400 | gateway rule `tool.not_granted` |
| `tool_params.<tool>.allowed_hosts` | no | for `http.*`: URL host must equal an entry or end with a `.suffix` entry; empty = no host restriction from this layer (SSRF checks still apply) | `params.host_not_allowed` |
| `tool_params.<tool>.allowed_commands` | no | for CLI tools: `argv[0]` must be in the list | `params.command_not_allowed` |
| `tool_params.<tool>.denied_arg_patterns` | no | substrings that may not appear anywhere in the flattened arguments | `params.denied_pattern` |
| `tool_params.<tool>.requires_approval` | no | park the run for a human before this tool runs (tools marked `Dangerous`, e.g. `http.post`, always require approval) | `approval.required` |
| `budget.*` | yes (zero = unlimited for that field — avoid) | hard stops; checked at every step start (worker) and every tool call (gateway) | `budget.exhausted` |
| `priority` | yes | scheduling lane and fairness floor: `interactive` may use the 20 % reserve and is leased first | `AcquireLease` order, `Reserve` floor |

### Content addressing

`digest = sha256(canonical_json(spec))` (sorted keys, no whitespace,
`internal/canon`). Two tenants posting identical specs get the same digest
under different `(tenant_id, name)` rows. Re-posting the same spec is a no-op
(`ON CONFLICT DO NOTHING`). Changing one character produces a new digest and a
new definition; runs already created keep the old digest **forever**, which is
what makes "edit the agent to widen its grants" impossible for a running agent.

## 2. The six seeded definitions, as worked examples

| Tenant / name | Model | Tools | Policy | Budget | Priority | What it demonstrates |
|---|---|---|---|---|---|---|
| `acme/report-writer` | `fake:report-writer` | `fs.write fs.read fs.list doc.convert exec.bash` | none needed — no network tool, no credential | 10 / 20 / 100k / $1 / 600 s | normal | the normal path: write, convert in a sandbox, verify |
| `acme/release-publisher` | `fake:github-publisher` | `fs.write fs.read exec.bash github.cli` | `github.cli.denied_arg_patterns: ["auth token","auth status","secret","variable"]` | 10 / 20 / 100k / $1 / 600 s | interactive | the credential boundary: `gh` runs in the broker sandbox |
| `acme/change-approver` | `fake:needs-human` | `fs.write fs.read` | — | default (25 / 50 / 200k / $5 / 3600 s) | interactive | human in the loop: parks in `WAITING_HUMAN` |
| `globex/poison-loop` | `fake:poison-loop` | `exec.bash` | — | **6 / 5** / 50k / $0.50 / 120 s | batch | a poison agent stopped by `max_tool_calls` |
| `globex/injected-agent` | `fake:injected` | `fs.write fs.read exec.bash` (**no** `http.get`, **no** `github.cli`) | `http.get.allowed_hosts` present but the tool is not granted → `tool.not_granted` wins | 10 / 12 / 80k / $1 / 300 s | batch | prompt injection contained by grants, patterns and SSRF checks |
| `globex/load-agent` | `fake:load` | `fs.write` | — | 5 / 5 / 20k / $0.20 / 120 s | batch | the scheduler under load |

Note the `injected-agent`: a `tool_params` entry for a tool that is **not**
granted is harmless — the grant check runs first. Policy narrows; it never
widens.

## 3. Writing policy that contains

1. **Grant the minimum.** An agent that only writes reports needs `fs.*` and
   `doc.convert`. Adding `exec.bash` "just in case" adds a sandbox; adding
   `http.get` adds an exfiltration path that only an allowlist closes.
2. **Every `http.*` grant gets `allowed_hosts`.** Without it, the only
   protection is the SSRF blocklist. Use exact hosts; a `.suffix` entry
   matches every subdomain, so `.example.com` is wide.
3. **Every CLI grant gets `allowed_commands`** naming the subcommands the
   task needs (`pr`, `issue`), and `denied_arg_patterns` for the ways that
   binary can print or move credentials (`auth token`, `auth status`,
   `secret`, `variable` for `gh`).
4. **Set every budget field.** Zero means unlimited. `max_tool_calls` is the
   one that stops loops fastest; `max_cost_usd` is the one finance cares
   about; `max_wall_seconds` counts time parked, so give human-in-the-loop
   agents hours, not minutes.
5. **Use `requires_approval` for anything that changes the world** beyond the
   workspace, unless the tool is already `Dangerous` (then approval is
   automatic).
6. **Prefer `batch` unless a human is waiting.** Interactive work draws on
   the reserve every tenant holds back; marking batch work interactive
   starves your own humans.
7. **Version by digest, not by name.** CI that starts runs should pass
   `agent_digest`, so a definition change cannot silently change what CI
   runs; humans in the console use `agent_name` (resolves to the newest
   definition of that name).

## 4. Validation you get for free

`POST /v1/agents` returns 400 when: `name` is empty; `model` or
`system_prompt` is empty; a tool name is not in the registry; `priority` is
not one of the three values. It does **not** validate policy quality — a
missing `allowed_hosts` is accepted. A policy linter (reject `http.*` grants
without hosts, reject wildcard suffixes) is on the
[production hardening checklist](../03-operations/07-production-hardening-checklist.md).

## 5. Running it

```bash
curl -s -X POST -H 'Authorization: Bearer acme-key' -H 'Content-Type: application/json' \
  -d '{"agent_name":"release-publisher","input":"Publish the v1.2 release notes from /work/notes.md"}' \
  localhost:8080/v1/runs
# → 201 {"id":"run_…","state":"QUEUED","def_digest":"sha256:…",…}
```

Then watch `GET /v1/runs/{id}/stream`, or open the console. The run's
`def_digest` tells you exactly which definition governed it, and `GET
/v1/audit?run=<id>` shows every decision the gateway made against that
definition.

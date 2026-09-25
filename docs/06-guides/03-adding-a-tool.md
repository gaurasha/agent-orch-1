# Adding a tool

> A tool is a `Schema` (what the model sees) plus a `Backend` (what the gateway
> runs). Adding one of an existing kind is a registry entry, a test and a
> catalogue row. Adding a new *kind* of tool touches the invoker and the policy
> engine, and this guide says exactly where. Nothing about the gateway's
> pipeline changes: every new tool inherits the grant check, budgets,
> parameter policy, journaling and audit for free.
>
> Related: [tools catalogue](02-tools-catalogue.md) · [tool gateway](../architecture/03-tool-gateway.md) · [authorization reasoning](../reasoning/05-authorization.md)

## 0. Decide which kind it is

| The tool… | Kind | Runs in | Credential | Examples |
|---|---|---|---|---|
| runs code the model wrote | `exec` | agent sandbox, no network | never | `exec.bash`, `exec.python`, `doc.convert` |
| reads/writes the run's files | `fs` | the gateway process, path-confined to `/work` | never | `fs.read`, `fs.write`, `fs.list` |
| calls an HTTP API with a bearer token | `http` | the gateway process | minted per call, header injected | `http.get`, `http.post` |
| runs a trusted binary that needs a token in its environment | `cli` | broker sandbox | minted per call, env var | `github.cli` |

If none fits — a tool that needs a *long-lived* connection, streaming output,
or a credential in a file — the design has a gap and the answer is a new kind,
not a workaround inside an existing one.

## 1. Add the registry entry (`backend/internal/tools/registry.go`)

```go
Tool{
    Schema: Schema{
        Name:        "http.put",                           // <domain>.<verb>; the name is permanent (it is in audit records)
        Description: "Perform an HTTP PUT against an allowed host. Authentication is handled by the platform; do not include credentials.",
        Parameters: map[string]Param{
            "url":  {Type: "string", Description: "Absolute https URL."},
            "body": {Type: "string", Description: "Request body."},
        },
        Required: []string{"url"},
    },
    Backend: Backend{
        Kind: KindHTTP, Method: "PUT", CredRef: "http/default",
        Timeout: 20 * time.Second,
        Dangerous: true,      // changes state outside the workspace → human approval
        UnsafeRetry: true,    // the target may not accept idempotency keys → "may have taken effect"
    },
},
```

Rules that the reviewer will check:

* **Description tells the model the truth** about its situation ("authentication
  is handled by the platform") — it reduces probing.
* **`Dangerous` for anything that changes the world** beyond `/work`.
  `UnsafeRetry` for anything without an idempotency key on the far side.
* **`Timeout` ≤ the reaper's `stuck_after` (2 min) with margin**, and the
  worker→gateway HTTP timeout (120 s) must exceed it.
* **`CredRef` names a root every tenant that grants the tool must have.** A
  tenant without it gets a clear error, not a leak.

## 2. If it is a new kind: the invoker (`invoke.go`)

`Invoker.Invoke` switches on `Backend.Kind`. A new kind is a new
`invokeX(ctx, iv) (Outcome, error)` with these obligations:

1. **Mint late, revoke always.** Mint only after the gateway has already
   decided and journaled (you are inside stage 5); `defer broker.Revoke`.
2. **Never return the credential.** Everything that leaves the function goes
   through `creds.Scrub(content, cred.Value)`; the credential's `Value` is a
   `creds.Secret` and any new `.Reveal()` call site **fails
   `TestSecret_RevealCallSitesAreFewAndIntentional`** until the expected count
   is deliberately changed in review.
3. **Cap output** (`clamp`) and set `Truncated`.
4. **Classify failures**: return an `error` only for *infrastructure* failure
   (the gateway turns it into 502 and, for `UnsafeRetry`, the "may or may not"
   message); a tool's own failure is `Outcome{IsError: true}`.
5. **Fill `Meta`** with what the audit record should carry: driver, exit code,
   `credential_id`, `credential_ttl_s` — never values.

For a second **CLI** tool: `invokeCLI` today injects the token as `GH_TOKEN`.
Generalise with a `Backend.CredEnv string` field (`"GH_TOKEN"`,
`"SLACK_BOT_TOKEN"`) and use it in `SafeEnv(...)` — a ten-line change. The
binary must be either our own image dispatched on `argv[0]` (add a `case` in
`cmd/agentorch/main.go` and a shim like `gh.go`) or an allowlisted path in
the sandbox image bind-mounted read-only; never something fetched at runtime.

## 3. Policy (`backend/internal/authz/authz.go`, `types.ParamPolicy`)

Existing policy fields cover most tools: `allowed_hosts` (http), `allowed_commands`
(cli), `denied_arg_patterns` (any), `requires_approval` (any). If the new tool
needs a new constraint (say, `max_body_bytes`):

1. add the field to `types.ParamPolicy` with a JSON tag (it is part of the
   canonical spec, so existing digests are unaffected only if the field is
   omitted when empty — use `omitempty`);
2. enforce it in `authz.checkParams` with a new rule name `params.<name>` and a
   reason that names the limit and the value;
3. add the rule to [errors and codes §3](../05-reference/06-errors-and-codes.md)
   and to the [authoring guide](01-authoring-agent-definitions.md);
4. test both directions (allowed and denied) in `authz_test.go`.

Rule names are permanent: they appear in audit records and dashboards.

## 4. Tests to add

| Test | Where | Proves |
|---|---|---|
| schema-only serialisation | `tools/registry_test.go` | `json.Marshal(tool)` contains no `Binary`, `CredRef`, `Timeout`, `Dangerous` |
| grant + params | `authz/authz_test.go` | an agent without the grant → `tool.not_granted`; with it and a bad argument → your `params.*` rule |
| gateway end to end | `gateway/gateway_test.go` | ALLOW path journals + audits twice; DENY path audits once and mints nothing; `Dangerous` → 202 |
| invoker | `tools/invoke_test.go` with a fake broker/driver | credential minted after decision, revoked on every path, scrubbed from output |
| safety (exec tools) | `sandbox_linux_test.go` | the new binary runs under the same jail; if it needs a new syscall, the denylist test tells you |
| demo | `cmd/agentorch/demo.go` | one assertion that the tool did its job through the whole stack |

## 5. Documentation to update

* [tools catalogue](02-tools-catalogue.md) — a row and the policy matrix
* [authoring guide](01-authoring-agent-definitions.md) — if the tool needs a policy pattern (like `gh auth token`)
* [errors and codes](../05-reference/06-errors-and-codes.md) — new rules or statuses
* [limits and defaults](../05-reference/05-limits-and-defaults.md) — the timeout
* the seeded agents, if the demo should exercise it

## 6. Rolling it out

A new tool is **opt-in per agent definition**: no existing agent can call it
until a new definition (new digest) grants it. That is the point of
content-addressed definitions, and it means shipping a tool is low-risk; the
risk is in the definitions that grant it, which is why the
[hardening checklist](../03-operations/07-production-hardening-checklist.md)
wants a policy linter before those are accepted.

## 7. Removing or renaming

Removing a tool makes every definition that grants it fail at `POST /v1/agents`
(unknown tool) and every running agent that calls it get `tool.unknown`. Rename
= add the new name, mark the old description "deprecated, use X", wait for
definitions to migrate, remove. Audit history keeps the old name.

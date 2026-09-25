# Tools catalogue — every built-in tool, both faces

> Each tool has two faces. The **schema** is what the model sees
> (`Tool.MarshalJSON` emits it and nothing else). The **backend** is what the
> gateway knows: how the call is executed, with which credential, under which
> timeout, and whether it is dangerous or unsafe to retry. Keeping them apart
> is why a model cannot ask for "the tool that holds the GitHub token".
>
> Code: [`backend/internal/tools/registry.go`](../../backend/internal/tools/registry.go) · [`invoke.go`](../../backend/internal/tools/invoke.go) · Related: [tool gateway](../architecture/03-tool-gateway.md) · [adding a tool](03-adding-a-tool.md)

## The nine tools

| Tool | Kind | Parameters (required in bold) | Backend | Timeout | Credential | Flags | Network |
|---|---|---|---|---|---|---|---|
| `exec.bash` | exec | **script** | `/bin/bash -c <script>` in the agent sandbox | 30 s | none | — | none (empty netns) |
| `exec.python` | exec | **script** | `python3 -c <script>` in the agent sandbox | 30 s | none | — | none |
| `doc.convert` | exec | **input**, **output**, from ∈ {markdown, html}, to ∈ {html, plain} | `aoconvert` (our binary, busybox pattern) or `pandoc` if present, in the agent sandbox | 30 s | none | — | none |
| `fs.write` | fs | **path**, **content** | `WorkspaceManager.Write` — path-validated, UTF-8, inside `/work` | 5 s | none | — | n/a |
| `fs.read` | fs | **path** | `WorkspaceManager.Read` | 5 s | none | — | n/a |
| `fs.list` | fs | path (default root) | `WorkspaceManager.List` | 5 s | none | — | n/a |
| `http.get` | http | **url** (absolute https) | the **gateway** issues the request with `Authorization: Bearer <minted>` | 20 s | `http/default` | — | from the gateway pod |
| `http.post` | http | **url**, body | same, `POST` | 20 s | `http/default` | **Dangerous** (approval) · **UnsafeRetry** | from the gateway pod |
| `github.cli` | cli | **argv** (array of strings) | `/opt/agentorch/bin/gh <argv>` in the **broker sandbox** with `GH_TOKEN` in its environment; stdout scrubbed | 20 s | `github/token` | NeedsNetwork | via egress proxy (designed); host netns in the PoC |

### What each backend field means

| `Backend` field | Meaning |
|---|---|
| `Kind` | `exec` (agent sandbox) · `fs` (workspace, no sandbox) · `http` (gateway makes the call) · `cli` (broker sandbox) |
| `Binary` | argv prefix for exec/cli tools; for cli tools, `Binary[0]` is the name under which our own binary is bind-mounted at `/opt/agentorch/bin/` |
| `Method` | HTTP method for http tools |
| `CredRef` | the credential reference the broker mints for this call (`github/token`, `http/default`); a tenant without that root gets "not configured for your tenant" |
| `URLTemplate` | for `github.cli`: the API base handed to the shim as `GITHUB_API_BASE` (the fake API in the PoC) |
| `Timeout` | one invocation's wall clock; becomes the sandbox `Limits.Wall` |
| `Dangerous` | the gateway returns `NEEDS_APPROVAL` unless the run was approved — the model cannot bypass it |
| `UnsafeRetry` | an infrastructure failure is reported as "MAY OR MAY NOT have taken effect" rather than a clean failure |
| `NeedsNetwork` | the sandbox is created with `NetworkProxy` instead of `NetworkNone` |

### What the model sees

```json
{"name":"github.cli","description":"Run a GitHub CLI command (gh). Authentication is injected by the platform - you neither have nor need a token, and there is no way for you to read one.",
 "parameters":{"argv":{"type":"array","items":{"type":"string"},"description":"Arguments to gh, e.g. [\"pr\",\"create\",\"--title\",\"Fix\"]."}},
 "required":["argv"]}
```

No `Binary`, no `CredRef`, no `Timeout`, no `Dangerous`. The descriptions are
written to tell the model the truth about its situation ("there is no way for
you to read one"), which measurably reduces pointless probing.

## Argument validation (`Tool.ValidateArgs`)

Deliberately not a JSON Schema engine: required arguments present and
non-empty; primitive types match (`string`, `number`, `boolean`, `array` with
typed items); enum membership. Anything richer belongs in parameter policy,
which is where security decisions live. Validation runs only for **granted**
tools, so a denial is never masked as a schema error.

## Which policy applies to which tool

| Tool | `allowed_hosts` | `allowed_commands` | `denied_arg_patterns` | `requires_approval` | always-on |
|---|---|---|---|---|---|
| `exec.*`, `doc.convert` | — | — | ✓ (on the script text) | ✓ | the sandbox **is** the policy: no network, RO root, limits |
| `fs.*` | — | — | ✓ | ✓ | paths confined to `/work` (`..` and absolute paths rejected) |
| `http.*` | ✓ | — | ✓ | ✓ (`http.post` always) | scheme http(s); no loopback/link-local/private/`.internal`/metadata hosts |
| `github.cli` | — | ✓ (`argv[0]`) | ✓ | ✓ | token scoped to tenant + ref, 60 s, revoked; output scrubbed |

## The workspace (`/work`)

`<workspace-dir>/<tenant>/<run>/` on the gateway's node (default
`/var/lib/agentorch/workspaces`), created on first use, bind-mounted `rw` into
both sandboxes, path segments sanitised, `nosuid,nodev`. Removed when the run
reaches a terminal state. It is the only channel between the agent sandbox
and the broker sandbox, and therefore always treated as hostile input by
anything that reads it. Object-storage checkpoints of it at tool-call
boundaries are designed, not built.

## `doc.convert` in detail

`aoconvert --from markdown --to html|plain input output` is the platform
binary dispatched on `argv[0]` (like `gh`), bind-mounted into the sandbox at
`/opt/agentorch/bin/aoconvert`. If `/usr/bin/pandoc` or `/usr/local/bin/pandoc`
exists in the sandbox image it is preferred. Only `from: markdown` is
implemented; `html` as a source returns a clear error. This is the demo's
"a real converter ran inside the jail and wrote into `/work`" assertion.

## Adding, removing, renaming

A tool's name is part of every agent definition that grants it and of every
audit record. Renaming is therefore a new tool plus a deprecation period, not
an edit. See [adding a tool](03-adding-a-tool.md).

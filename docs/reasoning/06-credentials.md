# 06 · Credentials — how an agent uses a secret it can never read

> Decision: **credentials are minted per call by a broker inside the gateway
> (scoped to tenant + reference, TTL ≤ 60 s, revoked after use); API tools are
> executed by the gateway itself with the header injected; CLI tools run a
> trusted binary in a second sandbox that shares only `/work` with the agent;
> the plaintext type redacts itself everywhere and can be revealed at exactly
> three audited call sites.** Not environment variables in the agent's
> sandbox, not a mounted file, not a long-lived token, not a vault the agent
> can query.
>
> Diagrams: [05-credential-plane](../architecture/05-credential-plane.md) · Concept: [secrets](../01-concepts/05-secrets.md) · Related: [02-isolation](02-isolation.md), [05-authorization](05-authorization.md)

## Problem

The agent must open a pull request. Opening a pull request needs a GitHub
token. The agent's code was written by a model that read a web page written
by an attacker. State the constraint precisely:

> Any byte the model-authored process can read, it can exfiltrate through any
> channel it has. Therefore the token must be readable by **no process the
> model can influence the code of**, and must be usable only through a path
> whose parameters are constrained by policy.

Secondary constraints: the token must not appear in logs, traces, events or
the model's context; its blast radius when it does leak must be bounded in
time and scope; and the mechanism must work for both "call an API" and "run a
CLI".

## Options

| # | Option | Strongest case | Where it breaks | Who does it |
|---|---|---|---|---|
| A | **Token in the agent sandbox's environment** (`GH_TOKEN=…`) | every CLI just works; zero engineering | violates the constraint outright: `env`, `/proc/self/environ`, `printenv` — and the model will be told to run them | most demos; several 2025 incidents ([14](14-industry-learnings.md)) |
| B | **Token in a file the agent can read** (`~/.config/gh/hosts.yml`, `~/.netrc`, `~/.aws/credentials`) | same as A; "standard" locations | same as A: `cat` is a tool call away; and the file persists across calls | `gh auth login` on a dev box; agents that inherit a developer's home |
| C | **Long-lived token in a vault the agent queries** (Vault, Secrets Manager) | central rotation and audit of *reads* | the agent has a vault credential, which is a credential; every read is a leak opportunity; still lands in the agent's process | services, not agents |
| D | **Credential-injecting egress proxy** — the sandbox has no token; a proxy adds the header to allowed hosts | the agent never sees the token; works for any HTTP client; per-host allowlist is natural | needs an egress proxy on every sandbox's only network path (the design has this for the broker sandbox); does not help for tools that need the token in a non-HTTP form; TLS interception or per-host CONNECT rules | Anthropic `sandbox-runtime` (domain allowlist proxy), OpenAI Codex network allowlists, corporate egress proxies, Envoy credential injector |
| E | **The gateway makes API calls itself** (chosen for `http.*`) | the token is used inside the TCB and never crosses a process boundary; the allowlist and SSRF checks run first; the response body is the only thing returned | the gateway must implement the call shapes agents need (`GET`/`POST` with a bearer header today); not a general "any HTTP client" solution | AgentCore Gateway (credential provider + tool invocation), Composio/Arcade-style tool platforms, this repository |
| F | **A second sandbox running a trusted binary with the token in *its* environment, sharing only `/work` with the agent** (chosen for CLI tools) | the CLI just works (it reads `GH_TOKEN` as designed); the agent's process cannot see the broker process; policy constrains argv before the mint; the token lives ≤ 60 s | a second cold start (8.4 ms); the binary must be *ours* (bind-mounted from the platform image) or allowlisted; `/work` is an attacker-controlled input to a trusted program — the argv allowlist and the token's scope bound that | this repository; conceptually the same as running `gh` in a CI runner step the user's code cannot touch |
| G | **Short-lived scoped tokens from an STS** (GitHub App installation tokens, AWS STS `AssumeRole`, Vault dynamic secrets, SPIFFE SVIDs) | the token expires in minutes and is scoped to one repo/role; standard | orthogonal: it says *what* to mint, not *where it may be read*; the PoC's HMAC-derived tokens stand in for it and the fake API verifies scope the same way | GitHub Actions (`GITHUB_TOKEN` per job), AWS, Vault, SPIRE |
| H | **Confused-deputy-proof object capabilities** (the tool call carries an attenuated capability; no ambient token) | the strongest theory | tool servers must verify capabilities; the ecosystem (GitHub's API) does not; the mint is still a broker | research; Fly.io macaroons for its own API |

The chosen design is **E + F + G's shape**: a broker that mints scoped,
short-lived tokens; the gateway uses them in-process for API tools and in a
second jail for CLI tools; D is the designed network path for that jail.

## Deep dive

### Why two sandboxes and not one

The agent sandbox runs model-authored code; the broker sandbox runs our
binary. If they were one process tree, the token in the environment of the
trusted binary would be in `/proc/<pid>/environ` of a process the agent can
see — and PID namespaces are inherited by children. Two `clone`s with
separate `CLONE_NEWPID | CLONE_NEWUSER` give two trees; a fresh `/proc` in
each shows only its own. The only thing they share is a bind mount of
`/work`, and that is deliberate: it is how the agent hands the CLI its input
(a PR body it wrote) and how the CLI hands back an artefact. That makes
`/work` an attacker-controlled input to a trusted program, which is why:

* `argv[0]` is allowlisted (`pr`, `issue`, `repo`) — the agent chooses *which*
  operation, not *what program*;
* denied patterns (`auth token`) are checked before the mint;
* the token is scoped to one tenant and one reference, and the API verifies
  both;
* the token lives ≤ 60 s and is revoked in a `defer` that runs on every path.

A bug in `gh` that could be triggered by a crafted PR body could misuse the
token for those seconds against that tenant's repositories. That is the
residual risk, stated.

### Why the binary is bind-mounted from our own image

`/opt/agentorch/bin/gh` inside the broker sandbox *is* the platform binary,
read-only, dispatched on `argv[0]` (busybox pattern). Consequences: one
image to build, scan and pin; the CLI shim is reviewed and tested with the
platform; no `curl | sh` inside a sandbox; and the sandbox's read-only root
means the agent cannot have replaced it via `/work` (the binary is not under
`/work`).

### Why a `Secret` type

Leaks through logging are the most common credential leak in practice: a
`%+v` on a struct, a `json.Marshal` of a config, a panic dump. Making the
type render as `[REDACTED]` in `String`, `GoString`, `Format` and
`MarshalJSON` turns every one of those into a non-event. `Reveal()` is the
only way out; a test walks the source tree and fails if the count of call
sites changes (three: the HTTP header, the broker sandbox's environment, and
`Scrub`). This is not perfect — Go strings cannot be zeroed, a debugger can
read memory — but it converts the *likely* leak into a type error and the
unlikely one into a documented residual.

### Why `Scrub` exists if policy already prevents printing the token

Belt and braces. Policy denies `gh auth token`; a future `gh` subcommand that
echoes its environment would slip past a denied-pattern list. `Scrub`
removes the exact value from stdout/stderr before the result reaches the
event log or the model, so even that case does not put the token in the
context.

### Why short TTL *and* revoke

Revocation makes a stolen token useless once the call ends; the TTL bounds
the damage if revocation fails (the broker crashed). Belt and braces again;
each covers the other's failure.

### What production changes

The `DerivedBroker` derives tokens from per-tenant HMAC roots held in memory,
and `fakegithub` verifies them. Production swaps `Mint` for a GitHub App
installation token (scoped to a repository set, ~1 h TTL, revocable), an AWS
STS `AssumeRole` with a session policy, or a Vault dynamic secret — and the
platform's identity to *those* systems comes from SPIFFE/SPIRE or the cloud's
workload identity rather than a static secret in a Kubernetes Secret. The
broker interface (`Mint`, `Revoke`, `Verify`) is the seam.

## State of the art

* **Anthropic `sandbox-runtime` / Claude Code**: no credentials in the
  sandbox; network through a proxy with a domain allowlist; the recommended
  pattern for tokens is a proxy or a helper *outside* the sandbox.
* **OpenAI Codex**: agents run with network disabled by default; secrets are
  injected as environment variables into the *setup* phase only and removed
  before the agent phase (the docs distinguish "secrets" from "environment
  variables" for exactly this reason).
* **GitHub Actions**: `GITHUB_TOKEN` is per-job, scoped by `permissions:`,
  expires when the job ends — the model for G.
* **AWS Bedrock AgentCore Identity**: a credential provider that vends
  OAuth/API-key credentials to tools on the agent's behalf so the agent code
  never holds them — E as a managed service.
* **Composio / Arcade / Nango**: "auth for agents" products whose value is
  precisely that the agent calls the tool platform and the platform holds the
  OAuth tokens.
* **SPIFFE/SPIRE**: workload identity via short-lived SVIDs — the answer to
  "how does the *gateway* prove who it is to the vault/STS".

## Documented issues

* **GitHub MCP data leak** (2025): a broadly scoped PAT in the agent's reach
  plus an injected instruction = private data in a public PR.
* **Supabase MCP**: a service-role key usable by the agent; injected SQL
  exfiltrated a table.
* **Credential leaks via logs** are so common that GitHub's secret scanning,
  AWS's key-quarantine and every SAST vendor have detectors for them — the
  motivation for a type-level redaction.
* **`~/.netrc` / `.git-credentials` / cloud-credential file reads** are
  standard post-exploitation steps in every red-team report on coding agents
  (2025 write-ups on Cursor, Copilot and Claude Code prompt injection all
  include "read the user's credential files" as the payload).
* **The confused deputy** (Hardy, 1988): a program with more authority than
  its caller does what the caller says. A CLI with a token, fed attacker
  input, is the modern instance — bounded here by scope, TTL and argv policy,
  not eliminated.

## Evidence in this repository

* S12 (credentials never reach the agent), S13 (the `Secret` type cannot be
  printed) in [safety proofs](../04-evidence/02-safety-proofs.md).
* `TestSecret_NeverRendersItsValue`, `TestSecret_SurvivesStructPrintingInsideAContainer`,
  `TestSecret_RevealCallSitesAreFewAndIntentional`.
* The demo's injection scenario: `gh auth token` is refused before any mint;
  `cat /proc/*/environ` in the agent sandbox finds no broker process.
* B9: the docs said one `Reveal()` site while the code had three — the test
  now asserts the exact number.

## Would reverse if

* every tool an agent needs were HTTP → drop F and make D (the proxy) the
  only path, as Anthropic's runtime does;
* tool servers verified capabilities → H: the broker becomes a capability
  minter and the second sandbox disappears;
* the platform ran on a vendor sandbox with a built-in credential vault
  (AgentCore) → the broker becomes an adapter to it.

## References

* Hardy, *The Confused Deputy* — https://dl.acm.org/doi/10.1145/54289.871709
* Anthropic, `sandbox-runtime` (proxy-based network allowlist, no secrets in sandbox) — https://github.com/anthropic-experimental/sandbox-runtime
* OpenAI, *Codex: environment variables and secrets* — https://developers.openai.com/codex/cloud/environments
* GitHub, *Automatic token authentication (`GITHUB_TOKEN`)* — https://docs.github.com/en/actions/security-for-github-actions/security-guides/automatic-token-authentication · *GitHub App installation access tokens* — https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app
* AWS, *AgentCore Identity* — https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/identity.html · *STS AssumeRole* — https://docs.aws.amazon.com/STS/latest/APIReference/API_AssumeRole.html
* HashiCorp Vault, *Dynamic secrets* — https://developer.hashicorp.com/vault/docs/secrets/databases
* SPIFFE — https://spiffe.io/docs/latest/spiffe-about/overview/
* Invariant Labs, *GitHub MCP exploited* — https://invariantlabs.ai/blog/mcp-github-vulnerability
* Linux `user_namespaces(7)` / `pid_namespaces(7)` — https://man7.org/linux/man-pages/man7/pid_namespaces.7.html
* Fly.io, *Macaroons escalated quickly* — https://fly.io/blog/macaroons-escalated-quickly/

# What an agent actually is

> **Prerequisite:** none. This is the ground floor.
> **Read next:** [Use cases](02-use-cases.md) · [Requirements decoded](03-requirements-decoded.md)

---

## 1. The irreducible core

Strip away the vocabulary and an AI agent is a loop over a text-prediction
function:

```python
context = [system_prompt, user_request]

while True:
    response = model.complete(context)          # text in, text out

    if response.tool_calls:                     # the model ASKED for something
        for call in response.tool_calls:
            result = platform.execute(call)     # OUR code does the work
            context.append(result)
        continue

    return response                             # the model is done talking
```

Everything else in this repository exists to make `platform.execute(call)` safe,
fair, durable and observable when there are a thousand of these loops running at
once for mutually distrusting customers.

### The model cannot do anything

This is the single most important sentence in the document, and it is routinely
misunderstood.

A large language model is a function from a token sequence to a probability
distribution over the next token. It has no file handles, no sockets, no
processes. When a product says an agent "read the config file and opened a pull
request", what physically happened is:

1. The model emitted the literal characters
   `{"tool":"fs.read","args":{"path":"config.yaml"}}`.
2. **Our code** parsed that, decided whether to allow it, read the file, and
   appended the bytes to the conversation.
3. The model, on the next call, emitted
   `{"tool":"github.cli","args":{"argv":["pr","create","--title","..."]}}`.
4. **Our code** decided whether to allow it, minted a credential, ran the CLI,
   and appended the output.

The model is the thing that *decides what to ask for*. The platform is the thing
that *decides what actually happens*. Confusing those two is the root cause of
most agent security incidents.

---

## 2. Where the time goes

Take one iteration of the loop and instrument it. These are the real orders of
magnitude, measured on this system where noted:

| Phase | Duration | Who is working |
|---|---|---|
| `model.complete()` | 2–20 s | the provider's GPUs |
| HTTP tool call | 50–500 ms | a third party's servers |
| Sandbox exec (cold start) | **8.4 ms** *(measured)* | the kernel |
| Sandbox exec (payload) | 10 ms – 30 s | the payload |
| Human reply (when required) | minutes – **days** | a person |
| **Our orchestration code** | **~1–5 ms** *(measured p50 step latency)* | us |

Draw that as a timeline for a 30-second agent turn and our code is a hairline.

```
  agent turn (≈ 12 s)
  ├─────────────────────────────────────────────────────────────┤
  │                                                             │
  ▓                                                             ▓   our code (~3 ms, both ends)
   ████████████████████████████████████████████                     waiting for the model (11 s)
                                                ██████               waiting for a tool (900 ms)
```

**The operational consequence:** an agent is not a workload, it is a *pending
obligation*. Provisioning a process, container or pod per agent dedicates
memory and scheduler attention to something that is idle 99.9% of the time.

**The arithmetic that kills pod-per-agent:**

| | |
|---|---|
| Pod overhead (pause container, cgroup, CNI, kubelet bookkeeping) | ~100 MiB |
| kubelet default max pods/node | 110 |
| 1000 concurrent agents | **≥10 nodes, ~100 GiB**, before one sandbox exists |
| Agent parked 2 days on a human | holds its pod for 2 days |

See [Durable execution](../01-concepts/02-durable-execution.md) for what we do
instead.

---

## 3. The context window is the agent's entire memory

The model is stateless between calls. Everything it "knows" about the task is
in the token sequence you hand it. That has three consequences people
consistently underestimate.

### 3a. Every turn re-sends everything

Turn *n* sends the system prompt, the original request, and every message and
tool result from turns 1..*n-1*. Cost grows **quadratically** with conversation
length, not linearly.

Concretely, with a 40-step agent at ~800 tokens per step of new content:

| Turn | New tokens | Input tokens sent | Cumulative input |
|---|---|---|---|
| 1 | 800 | 800 | 800 |
| 10 | 800 | 8,000 | 44,000 |
| 25 | 800 | 20,000 | 260,000 |
| 40 | 800 | 32,000 | 660,000 |

Sending 660k input tokens at Sonnet-class pricing ($3/Mtok input) is ~$2.00 for
a single agent run — before output tokens. This is why
[budgets](../01-concepts/02-durable-execution.md#7-budgets-enforced-not-reported) are enforced in dollars
and tokens, not just in steps, and why
[output from tool calls is size-capped](../01-concepts/01-linux-isolation.md#9-output-caps--a-control-people-forget):
a tool that returns 2 MB of logs does not cost you once, it costs you on **every
subsequent turn** of that run.

> In this system, `sandbox.Limits.MaxOutputBytes` defaults to 256 KiB and the
> gateway caps a tool result at 64 KiB before it can enter the log. An agent
> that runs `yes` is truncated rather than allowed to poison its own context.

### 3b. Everything in the context is equally "trusted" to the model

The model does not have a privileged channel. Your system prompt and a hostile
sentence inside a PDF the agent was asked to summarise arrive as the same kind of
tokens. There is no reliable in-band way to mark one as instructions and the
other as data.

That is [prompt injection](../01-concepts/08-prompt-injection.md), and it is
architectural, not a bug to patch.

### 3c. Reconstructing the context is the whole durability problem

If the context is the agent's memory, then persisting the context *is*
persisting the agent. That observation is what makes a stateless worker pool
possible: any worker that can rebuild the message list can continue the run.

In this codebase that reconstruction is one pure function —
[`runtime.Rebuild`](../../backend/internal/runtime/context.go) — from the durable
event log to `[]llm.Message`. Two different workers replaying the same log
produce byte-identical context.

---

## 4. Agents are heterogeneous in ways that matter

The brief says agents differ in "system prompts, tool sets, models, owners, and
trust levels". Each of those is a real design constraint:

| Dimension | Range | What it forces |
|---|---|---|
| **Duration** | seconds → days | State cannot live in memory. Timeouts must be per-agent, not global. |
| **Tool set** | 1 tool → dozens | Grants must be per-agent-definition, not per-platform. |
| **Model** | cheap/fast → expensive/slow | Cost accounting must be per-model. Quota estimates must scale with the request. |
| **Owner** | different teams in different tenants | Every record needs a tenant and a triggering human. |
| **Trust level** | internal script → customer-authored | The platform cannot assume a well-behaved agent anywhere. |

The last row is the one that drives the architecture. **The platform must be
correct when the agent is actively hostile**, because "written by a model that
read a hostile document" and "written by an attacker" are the same thing from
the kernel's point of view.

---

## 5. Two kinds of tool, two kinds of risk

The brief splits tools into API tools and execution tools. They fail
differently:

### API tools — "call this HTTP endpoint with tenant credentials"

| | |
|---|---|
| **The risk** | A credential with real blast radius must be *used* without being *disclosed*. The agent decides the arguments; if arguments include the URL, the agent can point the credential anywhere. |
| **The mitigation** | The gateway makes the call. The URL is validated against a per-agent host allowlist. The credential is injected into a header the agent never observes and is never returned in the response. See [Secrets](../01-concepts/05-secrets.md). |
| **What fails if you get it wrong** | Confused deputy: the agent gets the platform to use its own authority for the attacker's purpose. Credential exfiltration. SSRF into the cluster's internal network or the cloud metadata service. |

### Execution tools — "run this code"

| | |
|---|---|
| **The risk** | Arbitrary code authored by an untrusted party, on your infrastructure, next to other tenants' data. |
| **The mitigation** | Kernel-level isolation: namespaces, cgroups, seccomp, read-only rootfs, no network. See [Linux isolation](../01-concepts/01-linux-isolation.md). |
| **What fails if you get it wrong** | Container escape, cross-tenant data access, cryptomining, using your egress as a proxy, node credential theft. |

They also combine badly, which is the interesting case: an execution tool that
needs a credential (`gh`, `aws`, `kubectl`, `git push`). That combination is
what [the credential boundary](../01-concepts/05-secrets.md#3-the-two-sandbox-pattern)
exists to solve.

---

## 6. What "1000 concurrent agents" actually means

"Concurrent" is doing a lot of work in that phrase. Three readings:

| Reading | Meaning | Implied load |
|---|---|---|
| 1000 agents **exist** | rows in a table, most parked | ~nothing |
| 1000 agents are **in flight** | each between start and finish | depends entirely on duty cycle |
| 1000 agents are **executing right now** | 1000 simultaneous model calls | impossible — no provider quota allows it |

The middle reading is the real one. If an agent takes one turn every ~10 s, then
1000 in-flight agents is **~100 turns/second**. At ~3 ms of our CPU per turn,
that is **0.3 cores of actual orchestration work**.

The thing that is *not* cheap is the sandboxes: if 20% of agents are executing
code at any instant, that is 200 concurrent sandboxes at 0.5 CPU / 512 MiB →
~100 cores → roughly 13 × (8 vCPU, 32 GiB) nodes.

**Therefore:** the orchestrator is not the scaling problem and must not be
treated as one; the sandbox fleet is. Full arithmetic in
[Capacity planning](../03-operations/05-capacity.md).

---

## 7. Summary — the three consequences

Everything in `DESIGN.md` traces back to these:

1. **Agents are overwhelmingly idle** → pool the compute, make the *record*
   durable, never bind a process to an agent.
2. **Model output is untrusted data, not control** → authorize centrally and
   contain the blast radius; never rely on the model behaving.
3. **Only code execution is dangerous** → spend the expensive isolation budget
   there and nowhere else.

---

**Next:** [Use cases](02-use-cases.md) — what people actually build with this,
walked through step by step.

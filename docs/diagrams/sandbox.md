# Sandbox design

## The layers, and what each one buys

Every layer will eventually have a CVE. The question is not "is this layer
sound" but "what still holds when this layer fails".

```mermaid
flowchart TB
  subgraph L["Defence in depth — agent sandbox"]
    direction TB
    L1["1. KERNEL — gVisor (runsc)<br/>syscalls served by a user-space kernel<br/><b>holds when:</b> a Linux kernel LPE exists<br/><b>costs:</b> ~2-15% CPU, worse on syscall-heavy I/O"]
    L2["2. NODE — dedicated tainted pool<br/>agentorch.io/workload=sandbox:NoSchedule<br/><b>holds when:</b> the sandbox is fully compromised<br/><b>buys:</b> no control-plane creds on that node"]
    L3["3. NAMESPACES — user, mount, pid, net, ipc, uts<br/><b>holds when:</b> a process escapes its container<br/><b>buys:</b> it still sees no host pids, mounts or network"]
    L4["4. IDENTITY — uid 1000 in a userns → host uid 100999<br/>empty capability BOUNDING set, no_new_privs<br/><b>buys:</b> root-in-container owns nothing on the host,<br/>and no execve can ever regain a capability"]
    L5["5. FILESYSTEM — pivot_root onto a read-only tmpfs<br/>synthesised /etc; only /work and /tmp writable, size-capped<br/><b>buys:</b> no host files, no secrets in the image"]
    L6["6. NETWORK — EMPTY network namespace<br/><b>buys:</b> not a firewall rule that can be misconfigured;<br/>there is no interface, no address and no route"]
    L7["7. SYSCALLS — seccomp-BPF denylist, 36 entries<br/>mount, ptrace, bpf, userfaultfd, io_uring,<br/>open_by_handle_at, unshare, setns, ...<br/><b>buys:</b> the primitives an escape actually needs"]
    L8["8. RESOURCES — cgroup cpu/memory/pids + rlimits + wall clock<br/><b>buys:</b> a fork bomb, a memory bomb and an infinite<br/>loop are all bounded, not merely monitored"]
    L1 --> L2 --> L3 --> L4 --> L5 --> L6 --> L7 --> L8
  end
```

## How `gh` gets a token the agent cannot read

This is the question the brief asks directly. The answer is that **`gh` does not
run in the agent's sandbox at all**.

```mermaid
flowchart LR
  subgraph AS["AGENT SANDBOX — pid ns A, user ns A"]
    CODE["model-authored code<br/>env: PATH, HOME, TMPDIR, LANG<br/><b>no credential</b><br/><b>no network</b>"]
  end

  subgraph BS["BROKER SANDBOX — pid ns B, user ns B"]
    GH["gh (trusted binary from our image)<br/>env: <b>GH_TOKEN=aot_...</b><br/>egress: allowlisted API only"]
  end

  WORK[("/work<br/>shared bind mount<br/><b>the ONLY channel</b>")]

  GW["Tool gateway"] -->|"1. policy: 'pr create' yes,<br/>'auth token' no"| GW
  GW -->|"2. mint ≤60s tenant-scoped token"| BS
  CODE -.->|"writes NOTES.md"| WORK
  WORK -.->|"--body-file reads it"| GH
  GH -->|"3. Authorization: Bearer"| API["api.github.com"]
  GH -->|"4. stdout only"| GW
  GW -->|"5. scrubbed result"| CODE

  style CODE fill:#3a2f1f,stroke:#c9964f,color:#f0e4d0
  style GH fill:#1f2a3a,stroke:#4f8fc9,color:#d0e0f0
  style WORK fill:#1f3a2a,stroke:#4fc97f,color:#d0f0e0
```

Because they are different processes in **different pid and user namespaces**,
the agent cannot:

- read `/proc/<pid>/environ` of the broker — that pid is not in its namespace
- `ptrace` it — blocked by seccomp *and* by the namespace
- see it in `ps` — different pid namespace
- receive the token in any response — the gateway scrubs the value from output
  as a last resort, after policy has already blocked the commands that would
  print it

**Verified by test.** `make demo` runs an agent whose script does
`env | sort`, `cat /proc/self/environ` and `env | grep -iE 'token|secret|key'`,
asserts the output contains `NO CREDENTIALS IN ENVIRONMENT`, and *then* asserts
that `gh pr create` succeeded and that the fake GitHub API **verified a real,
freshly minted, correctly scoped token**. Both must hold.

## Measured cold start

| Driver | Mean cold start | Measured how |
|---|---|---|
| `namespace` (this host) | **8.4 ms** (worst 19.4 ms over 10 runs) | `TestSandbox_ColdStartIsMeasured` |
| `docker` | ~150–400 ms | daemon round trip dominates |
| `kubernetes` + gVisor | ~1–3 s | scheduling + sandbox creation |

This number decides the sandbox lifecycle policy. At 8 ms, **per-call ephemeral
sandboxes** are affordable and give the strongest isolation. At 1–3 s they are
not, which is why the Kubernetes driver's design keeps a **warm per-run
sandbox** for interactive agents and falls back to per-call for batch ones.
The measurement drives the architecture, not the other way round.

## What happens when a sandbox is compromised

Assume it will be.

| | |
|---|---|
| **Blast radius** | One run, one tenant, one workspace. No network. No credential (or one that expires in ≤60s). |
| **Detection** | eBPF runtime rules (Falco/Tetragon) on the sandbox node pool; gVisor's own blocked-syscall log; egress-proxy denials; a rise in `agentorch_tool_calls_total{decision="DENY"}`. |
| **Containment** | The pod has no ServiceAccount token, runs on a tainted node with no control-plane credentials, and has a `ResourceQuota` ceiling above it. |
| **Response** | Kill the pod; cordon and drain the node with a snapshot for forensics; revoke every credential minted for that run; freeze the tenant's runs; replay the tenant's audit chain to establish exactly what ran. |
| **Why the credential barely matters** | Minted per call, ≤60s TTL, bound to one tenant and one credential reference, and explicitly revoked after use. A stolen one is near-worthless. |

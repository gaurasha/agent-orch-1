# Running locally

> **Prerequisite:** none
> **Read next:** [Kubernetes deployment](02-deployment.md)

---

## Pick your path

| You have | Command | What you get |
|---|---|---|
| Linux + root + Go | `make demo` | The full demonstration, real namespace sandboxes, 21 assertions |
| Docker (any OS) | `make demo-docker` | Same system, Docker sandboxes |
| The above + Node | `make run` | Everything plus the console at `:8080` |
| Docker + kind | `make kind-up` | A real Kubernetes deployment |

`./scripts/preflight.sh <demo|docker|ui|kind>` checks prerequisites and names
exactly what is missing.

---

## `make demo`

Six scenarios, 21 assertions, against a real Postgres. **It exits non-zero if
any assertion fails** — it is an acceptance test, not a slideshow.

```
1. A NORMAL AGENT          write a document, convert it in a sandbox, verify it
2. THE CREDENTIAL BOUNDARY the agent dumps its own environment and finds nothing,
                           then opens a PR with a token the API actually verified
3. PROMPT INJECTION        four exfiltration attempts, all refused
4. A POISON AGENT          an infinite tool-call loop, stopped by an enforced budget
5. HUMAN IN THE LOOP       parked with no worker and no quota, then resumed
6. THE AUDIT TRAIL         chain verifies; tampering is detected
```

### Prerequisites

| | |
|---|---|
| Linux | The namespace driver needs real namespaces and cgroups |
| root or `CAP_SYS_ADMIN` | To create namespaces and write cgroups |
| Go 1.24+ | |
| Postgres | Optional but **strongly** recommended |

```bash
createuser -s agentorch && createdb -O agentorch agentorch
export PG_DSN="postgres://agentorch:agentorch@127.0.0.1:5432/agentorch?sslmode=disable"
make demo
```

Without `--dsn` it falls back to the in-memory store and says so loudly:

```
no --dsn given: using the in-memory store. Runs will NOT survive a restart,
which defeats the durability guarantee. Use Postgres for anything you intend to believe.
```

### Expected output

```
  [PASS] run completes successfully
  [PASS] document conversion ran inside the sandbox
  [PASS] converted artifact persisted in the run workspace
  [PASS] agent's own sandbox contains no credential
  [PASS] credentialed CLI call succeeded through the broker sandbox
  [PASS] the third-party API verified a genuine minted credential
  [PASS] no credential material anywhere in the event log
  [PASS] every exfiltration attempt was refused by policy
  [PASS] cloud metadata endpoint unreachable from the sandbox
  [PASS] poison agent was stopped by the platform
  [PASS] stopped by an enforced budget, not by chance
  …
  21 checks, 0 failed
```

---

## `make run` — the console

```bash
make run     # then open http://localhost:8080
```

Builds the UI, starts everything, serves the console.

### Worth trying

| # | Do this | Look for |
|---|---|---|
| 1 | Launch **`release-publisher`**, open the run | The `exec.bash` result shows `NO CREDENTIALS IN ENVIRONMENT`; the **next** call opens a PR |
| 2 | Launch **`injected-agent`** | Denied calls in orange, each with the reason returned to the model |
| 3 | Launch **`poison-loop`** | Budget meters filling until the platform stops it |
| 4 | Open **Audit log** | The green chain-verified banner; denials recorded as carefully as successes |
| 5 | Open **Quota & fairness**, launch a dozen `acme` runs | `acme`'s bucket drains; `globex`'s does not |
| 6 | Switch **Acting as** to `bob @ globex` | `acme`'s runs vanish entirely — not "access denied", **absent** |

---

## Tests

```bash
make test             # unit, no infrastructure
make test-safety      # the sandbox isolation claims, one at a time (Linux + root)
make test-durability  # kill a worker mid-tool-call and watch a run recover
make test-load        # 500 concurrent agents
make test-all         # everything, including Postgres conformance
```

Postgres-backed suites read `AGENTORCH_TEST_DSN`:

```bash
createdb -O agentorch agentorch_test
export TEST_DSN="postgres://agentorch:agentorch@127.0.0.1:5432/agentorch_test?sslmode=disable"
make test-all
```

Without it, the Postgres half **skips loudly** rather than silently passing.

> Each test package uses its own Postgres **schema** (`internal/testsupport`),
> because `go test ./...` runs packages concurrently and all three truncate on
> entry. `-p 1` would also work and would hide the problem instead of fixing it.

### The most interesting single test

```bash
cd backend && go test ./internal/sandbox/ -run TestSandbox_HasNoNetworkAccess -v
```

It asserts `ENETUNREACH` from inside the sandbox **and** runs the identical probe
on the host, skipping if the host is also offline. Without that negative control
it would prove only that the machine has no network.

---

## `make demo-docker`

For macOS and Windows, where the namespace driver cannot run.

```bash
make demo-docker    # docker compose up --build
```

> **Honest trade-off, stated because it matters:** this mounts the Docker socket
> into the platform container, which gives that container root-equivalent control
> of the host daemon. Acceptable for a local demo; **not** acceptable in
> production — which is exactly why the Kubernetes driver talks to the API server
> with a narrowly scoped ServiceAccount instead.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `operation not permitted` creating namespaces | Not root | `sudo -E make demo` |
| `cgroup v1: pids controller required` | No `pids` controller | Use the Docker path |
| `sandbox: seccomp filter is only implemented for linux/amd64` | Different arch | Use the Docker path |
| `connection refused` on 5432 | Postgres not running | Start it, or omit `--dsn` |
| Every tool call returns `401` | `AGENTORCH_SECRET` differs across replicas | Set it explicitly |
| Sandbox pods stuck `Pending` | No gVisor RuntimeClass | Expected. Set `AGENTORCH_RUNTIME_CLASS=""` locally |
| Console shows no runs | Wrong tenant key | Switch "Acting as" |
| `read-only file system` writing a helper | A helper mounted under `/usr` | Helpers live in `/opt/agentorch/bin` |

### Reading the logs

```bash
make run 2>&1 | grep '"level":"ERROR"'
make run 2>&1 | grep '"run_id":"run_01a0…"'          # one run across components
make run 2>&1 | grep '"component":"tool-gateway"'     # one component
```

Structured JSON, tagged with `service`, `component`, `run_id`, `tenant_id`.

---

## What good looks like

```
21 checks, 0 failed                       # make demo
ok  …/internal/sandbox        7.8s        # 11 safety tests
ok  …/internal/store          1.7s        # 19 subtests × 2 stores
ok  …/test/durability         2.1s        # worker killed mid-call
ok  …/test/load               5.6s        # 500 agents
Summary: 30 resources … Valid: 30         # make k8s-validate
```

---

**Next:** [Kubernetes deployment](02-deployment.md).

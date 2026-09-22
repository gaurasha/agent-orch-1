# Kubernetes primitives: what we use and what we deliberately do not

> **Prerequisite:** [Linux isolation](01-linux-isolation.md) · [Durable execution](02-durable-execution.md)
> **Read next:** [Architecture: components](../02-architecture/01-components.md)
> **Code:** [`deploy/k8s/`](../../deploy/k8s/)

The brief says scheduling, isolation, scaling and failure handling should *"use
Kubernetes primitives or justify why not"*. The "or justify why not" is the real
instruction, so this document is an honest ledger: every concern, whether
Kubernetes handles it, and where it does not, why.

---

## 1. What Kubernetes is actually good at here

| Concern | Primitive | Why it is the right tool |
|---|---|---|
| Running a stateless pool | `Deployment` + `HPA` | Exactly the shape of the agent workers |
| Sandbox isolation | `Pod` + `RuntimeClass` + `securityContext` | The purpose-built mechanism |
| Sandbox placement | `nodeSelector` + taints/tolerations | Untrusted workloads get their own node pool |
| Network isolation | `NetworkPolicy` | Enforced by the CNI regardless of our code |
| Resource ceilings | `ResourceQuota` + `LimitRange` | A cluster-level bound above our own accounting |
| Disruption safety | `PodDisruptionBudget` | Keeps a gateway replica alive through drains |
| Least privilege | `ServiceAccount` + `Role` | Per-component identity |
| Config and secrets | `ConfigMap` + `Secret` | With External Secrets in production |

---

## 2. RuntimeClass — the most important object in the deployment

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
scheduling:
  nodeSelector:
    agentorch.io/workload: sandbox
  tolerations:
    - key: agentorch.io/workload
      operator: Equal
      value: sandbox
      effect: NoSchedule
```

This single object is the difference between *"isolated from other tenants by
namespaces"* and *"isolated from the host kernel"*.

[gVisor](https://gvisor.dev/docs/) implements the Linux syscall ABI in user space
(the *Sentry*, written in Go). A container's syscalls are serviced by that
process rather than by the host kernel directly, so a kernel privilege-escalation
CVE is not automatically a host compromise — an attacker must first break the
Sentry.

### The failure mode is deliberate

If the cluster has no gVisor shim, sandbox pods stay `Pending` with a clear
message. That is **correct**. Failing to start a sandbox is a safe failure;
silently falling back to `runc` would hand untrusted code a shared kernel while
the dashboard stayed green.

The `scheduling` block also means a pod requesting this RuntimeClass
automatically lands on the right node pool and tolerates its taint — placement
and isolation configured in one place.

---

## 3. Pod-level hardening — every field and its reason

```yaml
spec:
  runtimeClassName: gvisor
  automountServiceAccountToken: false
  activeDeadlineSeconds: <wall + 5>
  nodeSelector:    {agentorch.io/workload: sandbox}
  tolerations:     [{key: agentorch.io/workload, operator: Equal, value: sandbox, effect: NoSchedule}]
  securityContext:
    runAsNonRoot:   true
    runAsUser:      1000
    runAsGroup:     1000
    seccompProfile: {type: RuntimeDefault}
  containers:
    - securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem:   true
        privileged:               false
        capabilities: {drop: ["ALL"]}
      resources:
        requests: {cpu: 500m, memory: 268435456, ephemeral-storage: …}
        limits:   {cpu: 500m, memory: 268435456, ephemeral-storage: …}
```

| Field | What it stops |
|---|---|
| `automountServiceAccountToken: false` | An escaped payload finding an API-server credential. **Often forgotten**, and it is free |
| `activeDeadlineSeconds` | A sandbox outliving its wall-clock budget if our own timeout fails |
| `runAsNonRoot` + `runAsUser: 1000` | A uid-0 process one misconfigured mount away from being interesting |
| `allowPrivilegeEscalation: false` | The Kubernetes name for `no_new_privs` |
| `readOnlyRootFilesystem: true` | Persisting anything outside the explicit writable mounts |
| `capabilities.drop: [ALL]` | Dropping *everything*, rather than trimming the default set |
| `seccompProfile: RuntimeDefault` | Uses the runtime's profile — which under gVisor matters less, but costs nothing |
| **`requests == limits`** | Puts the pod in **Guaranteed** QoS: a noisy neighbour cannot steal its CPU and it is the **last** thing evicted under node pressure |

The Guaranteed QoS point is subtle and worth stating: if sandboxes were
Burstable, a node under memory pressure would evict *them* first — which means
one tenant's memory pressure would kill another tenant's in-flight work.

---

## 4. Namespaces and Pod Security Admission

```yaml
metadata:
  name: agentorch
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/enforce-version: latest
```

Two namespaces, because they have genuinely different trust levels:

| Namespace | Contents | Posture |
|---|---|---|
| `agentorch` | Control plane, workers, gateway, database | PSA `restricted` |
| `agentorch-sandboxes` | Untrusted, model-authored code | PSA `restricted` + default-deny NetworkPolicy + quota + tainted nodes |

[Pod Security Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/)
in `enforce` mode (not just `warn`) means the API server **rejects** a pod that
requests privilege escalation, host namespaces, or a root user. It is a
belt-and-braces check against our own manifests being wrong.

---

## 5. NetworkPolicy — where "cannot be bypassed" comes from

This is what turns "the gateway is the authorization choke point" from a
convention into a fact.

```yaml
# Agent workers may reach Postgres and the tool gateway. Nothing else.
metadata: {name: agentd-egress}
spec:
  podSelector: {matchLabels: {app.kubernetes.io/name: agentd}}
  policyTypes: [Egress]
  egress:
    - to: [{podSelector: {matchLabels: {app.kubernetes.io/name: postgres}}}]
      ports: [{protocol: TCP, port: 5432}]
    - to: [{podSelector: {matchLabels: {app.kubernetes.io/name: toolgateway}}}]
      ports: [{protocol: TCP, port: 8081}]
    - to: [{namespaceSelector: {…kube-system}, podSelector: {k8s-app: kube-dns}}]
      ports: [{protocol: UDP, port: 53}, {protocol: TCP, port: 53}]
```

A **fully compromised agent worker** has no route to GitHub, to the internet, or
to the metadata service. "Skip the gateway and call the API directly" is not an
available action, regardless of what the worker's code does.

The sandbox namespace goes further:

```yaml
metadata: {name: sandbox-default-deny, namespace: agentorch-sandboxes}
spec:
  podSelector: {}                      # every pod
  policyTypes: [Ingress, Egress]
  # No rules at all => nothing in, nothing out. Not even DNS.
```

And the gateway's egress explicitly excludes link-local:

```yaml
- to:
    - ipBlock:
        cidr: 0.0.0.0/0
        except:
          - 169.254.169.254/32     # cloud instance metadata
          - 169.254.0.0/16
```

### ⚠ The trap that makes all of this a lie

**kind's default CNI (kindnet) does not implement NetworkPolicy.** Applying
these manifests to a default kind cluster produces zero errors and zero
enforcement. Every isolation policy silently does nothing while `kubectl get
networkpolicy` looks perfect.

That is strictly worse than having no policies, because it produces false
assurance. So:

```yaml
# deploy/kind/kind-config.yaml
networking:
  disableDefaultCNI: true
  podSubnet: "192.168.0.0/16"
```

and `scripts/kind-up.sh` installs Calico. A policy you cannot demonstrate is
enforcing is not a control.

---

## 6. RBAC — one identity per component

```yaml
- ServiceAccount agentorch-controlplane   automountServiceAccountToken: false
- ServiceAccount agentorch-agentd         automountServiceAccountToken: false
- ServiceAccount agentorch-toolgateway    automountServiceAccountToken: true
```

Only the gateway needs an API token, because only it creates sandbox pods. The
other two talk to Postgres and each other, so they get **no token at all** — a
compromise of the control plane yields no Kubernetes API access whatsoever.

```yaml
kind: Role
metadata: {name: agentorch-sandbox-manager, namespace: agentorch-sandboxes}
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["create", "get", "list", "watch", "delete"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
```

**What is deliberately absent matters more than what is present:**

| Not granted | Why |
|---|---|
| `pods/exec` | Would let the gateway open a shell in **any** sandbox — trivially breaking the credential boundary |
| `secrets` | Credentials come from the broker, never from etcd |
| `pods/portforward` | Would create a tunnel into a sandbox |
| Anything cluster-scoped | The Role is namespaced |

Asserted by `scripts/kind-test.sh` in **both** directions — that the gateway
*can* create pods and *cannot* exec into them:

```bash
check     "tool gateway CAN create pods in the sandbox namespace"        …
check_not "tool gateway CANNOT exec into sandbox pods"                   …
check_not "tool gateway CANNOT read secrets"                             …
check_not "agent workers CANNOT create pods anywhere"                    …
```

Testing only the positive direction is how over-broad RBAC survives review.

---

## 7. Scaling

```yaml
kind: HorizontalPodAutoscaler
spec:
  minReplicas: 2
  maxReplicas: 40
  metrics: [{type: Resource, resource: {name: cpu, target: {averageUtilization: 70}}}]
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 30
      policies: [{type: Percent, value: 100, periodSeconds: 30}]    # double, fast
    scaleDown:
      stabilizationWindowSeconds: 300
      policies: [{type: Percent, value: 25, periodSeconds: 60}]     # shrink, slowly
```

Asymmetric on purpose: the traffic shape is bursty (300 agents at 09:00), so
scaling up late costs latency for everyone while scaling down late costs only a
little money.

**Honest limitation:** CPU is a poor proxy for an I/O-bound system. The right
signal is **queue depth** — `count(*) WHERE state='QUEUED'` — which needs a
custom metrics adapter (KEDA, or `prometheus-adapter`). Listed in
[what I cut](../../DESIGN.md#8-what-i-cut-and-why-it-was-right-to-cut-it).

---

## 8. Resource quotas — the last line of defence

```yaml
kind: ResourceQuota
metadata: {name: sandbox-ceiling, namespace: agentorch-sandboxes}
spec:
  hard:
    pods: "500"
    requests.cpu: "200"
    requests.memory: 400Gi
```

This exists for one scenario: **our own accounting is wrong**. If a bug makes the
platform try to create ten thousand sandboxes, the API server refuses.

A quota that never binds costs nothing. The one time it binds, it prevents an
outage. `LimitRange` supplies defaults and a per-pod ceiling so a sandbox created
without explicit limits cannot consume a whole node.

---

## 9. What is NOT a Kubernetes primitive, and why

### 9a. Agent run state is not a CRD

| | |
|---|---|
| etcd per-object limit | **1.5 MB** — a long agent's event history exceeds it |
| Recommended total etcd size | a few GB |
| Update frequency | status writes at agent-step rate generate **cluster-wide watch traffic** |
| Query capability | no SQL; "every command agent X ran last Tuesday" is not expressible |
| Fair scheduling | no per-tenant weighted fairness |

etcd is a coordination store for cluster metadata that changes rarely. Agent runs
are high-frequency application state with rich query requirements. Putting them
in etcd would degrade the *cluster*, not just the platform.

**The middle ground I did not build** and would build in production: a **thin**
`AgentRun` CRD carrying identity and terminal status only — no event history —
reconciled *from* Postgres. That gets `kubectl get agentruns` and
GitOps-expressible agents without putting the hot path in etcd.

### 9b. Agent scheduling is not the Kubernetes scheduler

The kube-scheduler places *pods on nodes*. We need to decide *which run a worker
picks up next*, which depends on per-tenant LLM quota, priority lanes and
per-tenant weighted fairness — none of which the scheduler models.

So: Kubernetes schedules the *workers* and the *sandboxes*; the application
schedules the *runs*. Each does what it is good at.

### 9c. Sandboxes are not Jobs

`Job` adds retry-and-backoff semantics we do not want (the gateway owns retry
policy, because it owns the idempotency journal) and a completion-tracking
lifecycle that duplicates our own. A bare `Pod` with `restartPolicy: Never` plus
`activeDeadlineSeconds` is a smaller, clearer contract.

---

## 10. The local overlay downgrades — stated in the file that causes them

```yaml
# SECURITY DOWNGRADE, DELIBERATE AND LOCAL ONLY:
#
# kind nodes run containerd without the gVisor (runsc) shim, so a
# RuntimeClass of "gvisor" would leave every sandbox pod Pending. Setting
# it empty falls back to the default runtime - which means sandboxes here
# share the host kernel and the strongest isolation layer is absent.
- op: replace
  path: /data/AGENTORCH_RUNTIME_CLASS
  value: ""
```

Everything else still applies locally: no network, read-only rootfs, non-root,
all capabilities dropped, seccomp, resource limits. But the top layer is gone,
and **the comment lives in the file that removes it**, not in a document nobody
reads. A local overlay that silently weakens the model is how a weaker model
ships to production.

---

## 11. GitOps as a security control

Argo CD is not just a deployment convenience here. The platform's security rests
on policy being **reviewable**: NetworkPolicies, RBAC, RuntimeClass, quotas. If
an operator can `kubectl edit` a NetworkPolicy at 3am and nobody notices, the
policy is advisory.

```yaml
syncPolicy:
  automated:
    prune:    true     # deleted from git ⟹ deleted from the cluster
    selfHeal: true     # manual kubectl edits are reverted
```

`selfHeal` makes "what is in git" and "what is running" the same statement.
`scripts/argocd-test.sh` verifies it by making a manual change and asserting the
cluster puts it back — because an Argo install that does not reconcile is
decoration.

### The AppProject is the part people skip

Without one, any `Application` in the `argocd` namespace can deploy anything
anywhere, which makes Argo the widest privilege in the cluster.

```yaml
kind: AppProject
spec:
  destinations:                       # only these two namespaces
    - {namespace: agentorch,           server: https://kubernetes.default.svc}
    - {namespace: agentorch-sandboxes, server: https://kubernetes.default.svc}
  clusterResourceWhitelist:           # only these cluster-scoped kinds
    - {group: '',           kind: Namespace}
    - {group: node.k8s.io,  kind: RuntimeClass}
  namespaceResourceBlacklist:         # never these
    - {group: rbac.authorization.k8s.io, kind: ClusterRole}
    - {group: rbac.authorization.k8s.io, kind: ClusterRoleBinding}
```

A compromised repository cannot escalate cluster-wide.

---

## 12. Verified, not asserted

`make k8s-validate` builds both kustomizations and checks them against real
Kubernetes 1.31 schemas:

```
base:  Summary: 30 resources found in 1 file - Valid: 30, Invalid: 0, Errors: 0
local: Summary: 30 resources found in 1 file - Valid: 30, Invalid: 0, Errors: 0
```

`scripts/kind-test.sh` then verifies the **deployed** cluster, because a manifest
that applies cleanly has proved nothing: it reads the hardening fields back from
the API server, checks RBAC in both directions, confirms the taint and the
default-deny policy exist, and runs an agent end to end.

---

## References

- [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
- [RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)
- [gVisor](https://gvisor.dev/docs/) · [GKE Sandbox](https://cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods)
- [NetworkPolicy](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
- [QoS classes](https://kubernetes.io/docs/concepts/workloads/pods/pod-qos/)
- [etcd limits](https://etcd.io/docs/v3.5/dev-guide/limit/)
- [Argo CD projects](https://argo-cd.readthedocs.io/en/stable/user-guide/projects/)

---

**Next:** [Architecture: components](../02-architecture/01-components.md).

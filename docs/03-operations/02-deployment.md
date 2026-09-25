# Kubernetes deployment and GitOps

> **Prerequisite:** [Running locally](01-running.md) · [Kubernetes primitives](../01-concepts/09-kubernetes.md)
> **Read next:** [Observability](03-observability.md)
> **Source:** [`deploy/k8s/`](../../deploy/k8s/) · [`deploy/argocd/`](../../deploy/argocd/)

---

## Quick start

```bash
make kind-up        # cluster + Calico + build/load images + deploy
make kind-test      # verify the DEPLOYED cluster enforces what it claims
make argocd-up      # install Argo CD and register this repository
make argocd-test    # prove selfHeal reverts a manual kubectl change
make kind-down
```

---

## What `kind-up` does, and why each step exists

### 1. Create the cluster

```yaml
networking:
  disableDefaultCNI: true          # ← the important line
  podSubnet: "192.168.0.0/16"
nodes:
  - role: control-plane
    extraPortMappings: [{containerPort: 30080, hostPort: 8080}]
  - role: worker                   # sandbox pool
    kubeadmConfigPatches: [node-labels: agentorch.io/workload=sandbox,
                           register-with-taints: agentorch.io/workload=sandbox:NoSchedule]
  - role: worker                   # platform pool
```

Three nodes shaped like production, so the manifests that work here work there.

### 2. Install Calico

**This is not optional.** kind's default CNI (kindnet) **does not implement
NetworkPolicy**. Applying the isolation policies to a default kind cluster
produces zero errors and zero enforcement — every policy silently does nothing
while `kubectl get networkpolicy` looks perfect.

That is strictly worse than having no policies, because it produces false
assurance.

### 3. Build and side-load images

```bash
docker build -f deploy/docker/Dockerfile         -t agentorch/platform:dev .
docker build -f deploy/docker/Dockerfile.sandbox -t agentorch/sandbox:dev  .
kind load docker-image agentorch/platform:dev --name agentorch
```

kind nodes cannot see the host's Docker images; they must be side-loaded.

### 4. Apply and wait

```bash
kubectl apply -k deploy/k8s/overlays/local
kubectl -n agentorch rollout status statefulset/postgres    --timeout=300s
kubectl -n agentorch rollout status deployment/controlplane --timeout=300s
…
```

---

## What gets deployed

30 resources:

| Kind | Count | Notes |
|---|---|---|
| `Namespace` | 2 | Both PSA `restricted`, **enforce** |
| `RuntimeClass` | 1 | `gvisor` |
| `ServiceAccount` | 3 | Only the gateway gets an API token |
| `Role` + `RoleBinding` | 2 | Create pods; **not** exec, **not** secrets |
| `NetworkPolicy` | 6 | Default-deny both namespaces, plus narrow allows |
| `ResourceQuota` + `LimitRange` | 2 | The ceiling above our own accounting |
| `Secret` | 2 | Postgres password, run-token signing key |
| `ConfigMap` | 1 | |
| `Service` | 3 | |
| `Deployment` | 3 | controlplane, toolgateway, agentd |
| `StatefulSet` | 1 | Postgres |
| `HPA` | 1 | agentd, 2–40 |
| `PodDisruptionBudget` | 2 | controlplane, toolgateway |

Every hardening field and its reason:
[Kubernetes primitives §3](../01-concepts/09-kubernetes.md).

---

## `kind-test` — verifying the deployment, not the manifests

**A manifest that applies cleanly has proved nothing.** These checks read the
properties back from the API server after the fact.

```bash
$ make kind-test

== workloads ==
  [PASS] control plane is available
  [PASS] tool gateway is available
  [PASS] agent workers are available

== pod hardening (read back from the API server) ==
  [PASS] controlplane runs as non-root
  [PASS] controlplane has a read-only root filesystem
  [PASS] controlplane drops all capabilities
  …

== isolation ==
  [PASS] sandbox namespace has a default-deny NetworkPolicy
  [PASS] a hard ceiling exists on sandbox resources
  [PASS] the sandbox node carries the untrusted-workload taint

== least privilege ==
  [PASS] tool gateway CAN create pods in the sandbox namespace
  [PASS] tool gateway CANNOT exec into sandbox pods
  [PASS] tool gateway CANNOT read secrets
  [PASS] agent workers CANNOT create pods anywhere

== end to end through the deployed API ==
  [PASS] API responds
  [PASS] agents are seeded
  [PASS] an unauthenticated caller is refused
  [PASS] launched run run_…
  [PASS] run completed in the cluster (sandboxed tool calls ran as Pods)
  [PASS] globex key cannot read acme's run
  [PASS] audit chain verifies
```

### The negative checks matter most

```bash
check_not "tool gateway CANNOT exec into sandbox pods" \
  kubectl auth can-i create pods/exec --namespace agentorch-sandboxes \
    --as=system:serviceaccount:agentorch:agentorch-toolgateway
```

Testing only that permissions *work* is how over-broad RBAC survives review. If
the gateway could `exec` into a sandbox, the entire credential boundary would be
decorative.

---

## Argo CD

### Why GitOps is a security control here

The platform's security rests on policy being **reviewable**: NetworkPolicies,
RBAC, RuntimeClass, quotas. If an operator can `kubectl edit` a NetworkPolicy at
3am and nobody notices, the policy is advisory.

```yaml
syncPolicy:
  automated:
    prune:    true    # deleted from git ⟹ deleted from the cluster
    selfHeal: true    # manual kubectl edits are reverted
```

`selfHeal` makes *"what is in git"* and *"what is running"* the same statement.

### `argocd-test` proves it reconciles

```bash
$ make argocd-test

  [PASS] argocd-server is installed
  [PASS] the agentorch Application exists
  sync=Synced health=Healthy
  [PASS] application is Synced
  [PASS] application is Healthy
  [PASS] AppProject restricts what Argo may deploy

== selfHeal: revert a manual change ==
  current image: agentorch/platform:dev
  simulating an out-of-band kubectl edit...
  [PASS] Argo reverted the manual change (git is the source of truth)
```

An Argo install that does not actually reconcile is decoration, so the test makes
a real change and asserts the cluster puts it back.

### The AppProject is the part people skip

Without one, any `Application` in the `argocd` namespace can deploy anything
anywhere — making Argo the widest privilege in the cluster.

```yaml
destinations:                    # only these two namespaces
  - {namespace: agentorch, …}
  - {namespace: agentorch-sandboxes, …}
clusterResourceWhitelist:        # only these cluster-scoped kinds
  - {group: '',          kind: Namespace}
  - {group: node.k8s.io, kind: RuntimeClass}
namespaceResourceBlacklist:      # never these
  - {group: rbac.authorization.k8s.io, kind: ClusterRole}
  - {group: rbac.authorization.k8s.io, kind: ClusterRoleBinding}
```

### `ignoreDifferences`

```yaml
ignoreDifferences:
  - group: apps
    kind: Deployment
    name: agentd
    jsonPointers: [/spec/replicas]     # the HPA owns this
```

Without it, Argo and the HPA fight forever and the app shows permanently
`OutOfSync` — which trains people to ignore the sync status entirely.

---

## Validating manifests without a cluster

```bash
$ make k8s-validate
base:  Summary: 30 resources found in 1 file - Valid: 30, Invalid: 0, Errors: 0
local: Summary: 30 resources found in 1 file - Valid: 30, Invalid: 0, Errors: 0
```

`kubectl kustomize` + [`kubeconform`](https://github.com/yannh/kubeconform)
against real Kubernetes 1.31 schemas, in strict mode (unknown fields are
errors). Fast enough for CI on every commit.

---

## Production differences

Everything below is deliberately **not** in the local overlay:

| Concern | Local | Production |
|---|---|---|
| **gVisor** | disabled (no shim in kind) | `RuntimeClass: gvisor`, **required** |
| **Postgres** | single-replica StatefulSet, no backups | managed (RDS/Cloud SQL) or CloudNativePG, with PITR + a read replica + PgBouncer |
| **Secrets** | `Secret` objects in git | External Secrets Operator → cloud secret manager |
| **TLS to Postgres** | `sslmode=disable` | `verify-full` with the CA mounted |
| **Ingress** | NodePort 30080 | Ingress/Gateway API + TLS + WAF |
| **Workload identity** | shared HMAC secret | **SPIFFE/SPIRE mTLS** — the top gap |
| **Sandbox nodes** | one shared worker | dedicated pool, Karpenter, per-tenant pools for high tiers |
| **Runtime detection** | none | Falco / Tetragon on the sandbox pool |
| **Autoscaling signal** | CPU | queue depth via KEDA |
| **Audit** | Postgres only | + shipped to a SIEM, head hash anchored externally |
| **Egress proxy** | not deployed | required for the credentialed path |

---

## Operational commands

```bash
# What is running
kubectl -n agentorch get pods,hpa
kubectl -n agentorch-sandboxes get pods          # live sandboxes

# Logs for one run, across every component
kubectl -n agentorch logs -l app.kubernetes.io/part-of=agentorch --tail=-1 \
  | grep '"run_id":"run_…"'

# Is a tenant being throttled?
kubectl -n agentorch port-forward svc/controlplane 8080:8080 &
curl -s -H 'Authorization: Bearer demo-operator-key' localhost:8080/v1/quota | jq

# Scale workers
kubectl -n agentorch scale deployment/agentd --replicas=10

# Drain a node — runs survive; verify
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
kubectl -n agentorch get pods -w
```

---

**Next:** [Observability](03-observability.md).

# 11 · Kubernetes — how deep to integrate, and what the cluster enforces

> Decision: **Kubernetes is the enforcement layer, not the runtime model:
> two namespaces under Pod Security *restricted*, default-deny NetworkPolicy
> with per-Deployment allowlists, one Role for the gateway, a gVisor
> RuntimeClass on a tainted node pool for sandboxes, ResourceQuota/LimitRange,
> HPA on workers, PDBs on the hot path, and Argo CD with `selfHeal` and a
> fenced AppProject as the only write path.** Not a pod per agent, not
> Kubernetes Jobs as the queue, not an operator/CRD, not Docker Compose in
> production, not Nomad/ECS.
>
> Diagrams: [08-network-topology](../architecture/08-network-topology.md) · Concept: [Kubernetes](../01-concepts/09-kubernetes.md) · Ops: [deployment](../03-operations/02-deployment.md) · Related: [02-isolation](02-isolation.md), [01-runtime-model](01-runtime-model.md)

## Problem

Two distinct questions get conflated under "Kubernetes":

1. **What is the unit of scheduling?** — answered in [01](01-runtime-model.md):
   a run is a row, a worker is a replica, a sandbox is a short-lived process
   or pod. Kubernetes schedules *replicas and sandboxes*, not agents.
2. **What does the cluster enforce that our code cannot?** — network
   reachability, privilege, resource ceilings, runtime isolation class, and
   who may change any of it. These are the reasons to be on Kubernetes at all.

The constraints: the platform's safety claims ("a worker cannot reach a third
party", "a sandbox cannot reach anything") must be true *regardless of bugs
in our code*; the deployer must not be able to escalate; and the whole thing
must run on a laptop (kind) and on a managed cluster without divergence.

## Options — integration depth

| # | Option | Strongest case | Where it breaks | Who does it |
|---|---|---|---|---|
| A | **Docker Compose / a single VM** | simplest; the demo runs this way (`make demo-docker`) | no NetworkPolicy, no PSA, no RuntimeClass, no HPA — the safety claims become "our code is correct"; fine for dev, not for a multi-tenant platform | dev and CI |
| B | **Nomad / ECS / Cloud Run** | simpler schedulers; ECS+Fargate gives Firecracker isolation per task for free | Fargate's per-task cost and 1–2 s starts for per-call sandboxes; NetworkPolicy-equivalents differ per platform; the manifests in this repository would be rewritten, not ported; smaller ecosystem for policy (no Gatekeeper/Kyverno) | teams already on them |
| C | **Kubernetes with the platform as plain Deployments + the sandbox driver creating pods** (chosen) | the enforcement primitives exist and are portable (PSA, NetworkPolicy, RuntimeClass, RBAC, quota); managed offerings everywhere; kind for local parity; GitOps tooling mature | NetworkPolicy needs a capable CNI; gVisor needs node support; pod-per-call sandboxes are seconds, not milliseconds; YAML volume | most platforms of this kind |
| D | **Kubernetes Jobs as the run queue** (a Job per agent run) | the scheduler does the queueing; `ttlSecondsAfterFinished`, backoff, parallelism built in | violates [01](01-runtime-model.md): the pod is the agent's identity; idle pods cost slots; hundreds of Job creations per minute load the API server; no exactly-once; no per-tenant fairness | batch pipelines (Argo Workflows) |
| E | **An operator with CRDs** (`AgentRun` resources reconciled by a controller) | declarative; `kubectl get agentruns`; the reconciler pattern is robust | etcd is the wrong database for 10 k events/min (object size limits, watch fan-out, no queries); the store would still be Postgres and the CRD a projection; heavy engineering for a nice `kubectl` | platform products that sell to Kubernetes-native buyers |
| F | **Knative / KEDA for scale-to-zero workers** | idle cost → zero | workers are already cheap when idle (they poll); KEDA on queue depth is the *designed* HPA improvement, not a runtime change | add-on, not an alternative |

## Deep dive: what each enforcement primitive buys

| Primitive | What it makes true | Without it |
|---|---|---|
| **Namespaces** `agentorch`, `agentorch-sandboxes` | two blast radii; separate quota, policy, node pools | one compromise reaches everything |
| **Pod Security Admission `restricted`** | no privileged pods, no host namespaces, no hostPath, non-root, dropped capabilities, seccomp `RuntimeDefault` — rejected at admission | a manifest typo grants a sandbox `hostPID` |
| **NetworkPolicy default-deny + allowlists** | "agentd cannot reach GitHub" is a CNI rule, not a code property; the gateway is the only egress; sandboxes have none | every safety claim reduces to "our code has no bugs" |
| **RuntimeClass `gvisor`** | the sandbox pod's syscalls hit a user-space kernel; a Linux LPE is not a host compromise | shared kernel with every tenant |
| **Node taint + selector** | sandboxes land only on the sandbox pool; platform pods never do | an escape lands next to the gateway |
| **RBAC: one Role** | the gateway can create/delete pods in one namespace and nothing else; workers and the API have no SA token | a compromised worker calls the API server |
| **ResourceQuota + LimitRange** | a hard ceiling on the sandbox namespace; every container has limits → Guaranteed QoS | one tenant's fork bombs take the node pool |
| **PDB `minAvailable: 1`** on controlplane and gateway | a drain never removes the last replica of a hot-path service | a node upgrade stalls every tool call |
| **HPA 2–40 on agentd** | a burst scales the worker pool; slow scale-down avoids thrash | manual capacity |
| **Argo CD `selfHeal` + `prune` + AppProject** | git is the only write path; drift is reverted; the deployer cannot grant itself the cluster | `kubectl edit` becomes undocumented production state |

### Why NetworkPolicy is the load-bearing primitive

The authorization argument ([05](05-authorization.md)) says the gateway is
un-bypassable *because the worker has no route to anything else*. That
sentence is `agentd-egress`. It is the one place where a security property
is delegated to infrastructure, and it has a known trap: **kind's default
CNI (kindnet) does not implement NetworkPolicy**, and neither do some managed
defaults without a flag. A policy applied to such a cluster is accepted and
silently does nothing. `kind-up.sh` therefore installs Calico, and the docs
say so in capitals, because false assurance is worse than none. The honest
gap: `kind-test.sh` verifies the policy *objects* and RBAC answers on the
live cluster; a probe that actually attempts a denied connection is the next
test to add.

### Why gVisor as a RuntimeClass and not a special node image

A `RuntimeClass` is a one-line change per pod and works on GKE (Sandbox),
on self-managed nodes with `runsc` installed, and on kind for testing. Kata
would be stronger but requires KVM on the node, which rules out many managed
node pools; [02](02-isolation.md) has the comparison and the reversal
condition.

### Why Argo CD and not Flux or CI-driven `kubectl apply`

All three work. Argo was chosen for `selfHeal` (drift is reverted, and the
test proves it by `kubectl set image`-ing the worker and watching it
revert), for the AppProject as a *deployer fence* (source repos,
destinations, cluster-scoped whitelist, RBAC blacklist), and for the UI that
shows the diff during a review. Flux has equivalents (Kustomization
reconciliation, multi-tenancy lockdown); CI-driven apply has neither drift
correction nor a fence. The fence matters because a GitOps deployer is the
most privileged identity in the cluster: a malicious PR that adds a
`ClusterRoleBinding` is refused at sync, not after.

### Why one image and one binary

Every Deployment runs the same image; the sandbox init and the CLI shim are
the same binary. One digest to pin, one SBOM, one vulnerability scan; a
rollback is a one-line revert; and the version skew between components is
zero by construction.

## State of the art

* **GKE Sandbox** (gVisor as a RuntimeClass) and **EKS with Kata/Firecracker
  via Bottlerocket or bare-metal** are the two mainstream "stronger than runc"
  paths on managed Kubernetes.
* **Pod Security Admission** replaced PodSecurityPolicy in 1.25; `restricted`
  is the documented baseline for untrusted workloads.
* **Calico / Cilium** are the CNIs that implement NetworkPolicy fully;
  Cilium adds L7 and FQDN-based egress policy (which would let the broker
  sandbox's allowlist be a CNI rule rather than a proxy).
* **Argo CD** and **Flux** are the CNCF-graduated GitOps controllers; the
  OpenGitOps principles (declarative, versioned, pulled, reconciled) are the
  shared model.
* **KEDA** is the standard for scaling workers on queue depth rather than
  CPU — the designed HPA improvement.
* **Kyverno / Gatekeeper** for policy-as-code on manifests (e.g. "every pod
  in `agentorch-sandboxes` must set `runtimeClassName: gvisor`") — the
  production step beyond PSA.

## Documented issues

* **NetworkPolicy silently unenforced** on CNIs without support — documented
  in the Kubernetes NetworkPolicy page ("your networking solution must
  support NetworkPolicy"); the single most common false-assurance bug in
  cluster hardening.
* **HPA vs GitOps replica fights** — Argo's documentation on
  `ignoreDifferences` exists because HPA-managed replica counts show as
  drift and get reverted; this repository sets it.
* **PDBs blocking drains** — a `minAvailable` on a single-replica dev
  Deployment blocks node maintenance forever; the overlay for kind is where
  that is relaxed.
* **gVisor incompatibilities and overhead** — documented by the project;
  sandboxes here run short tool calls, which is the workload gVisor is best
  at.
* **Argo CD AppProject escalation** — Argo's own security docs describe the
  project restrictions precisely because an unrestricted Argo is
  cluster-admin.
* **API-server load from pod-per-call** — hundreds of pod creations per
  minute is fine; thousands per minute is a control-plane capacity question,
  which is why the design warms per-run pods for interactive agents.

## Evidence in this repository

* `make k8s-validate`: kubeconform validates both the base and the local
  overlay (30/30 resources).
* `scripts/kind-test.sh`: Deployments Available; `runAsNonRoot`, read-only
  root, `drop: ALL` read back from the API server; `sandbox-default-deny`
  and `sandbox-ceiling` present; the taint present; RBAC `can-i` answers
  (gateway *can* create pods, *cannot* exec, *cannot* read secrets; workers
  cannot create pods); an end-to-end run through the deployed API; a
  cross-tenant read returns 404.
* `scripts/argocd-test.sh`: Application Synced/Healthy; AppProject present;
  an out-of-band `kubectl set image` is reverted by `selfHeal`.

## Would reverse if

* the organisation runs ECS/Fargate and nothing else → the sandbox driver
  becomes a Fargate task launcher and the policies become security groups;
  the platform code does not change;
* the cluster has Cilium → the broker sandbox's egress allowlist can be an
  FQDN NetworkPolicy instead of a proxy;
* Kubernetes-native buyers demand `kubectl get agentruns` → an operator that
  *projects* Postgres state into a CRD, never the reverse.

## References

* Kubernetes, *Network Policies* — https://kubernetes.io/docs/concepts/services-networking/network-policies/ · *Pod Security Admission* — https://kubernetes.io/docs/concepts/security/pod-security-admission/ · *Pod Security Standards* — https://kubernetes.io/docs/concepts/security/pod-security-standards/ · *RuntimeClass* — https://kubernetes.io/docs/concepts/containers/runtime-class/ · *Taints and tolerations* — https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/ · *Resource quotas* — https://kubernetes.io/docs/concepts/policy/resource-quotas/ · *PodDisruptionBudget* — https://kubernetes.io/docs/tasks/run-application/configure-pdb/ · *HorizontalPodAutoscaler* — https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/ · *RBAC* — https://kubernetes.io/docs/reference/access-authn-authz/rbac/
* GKE Sandbox (gVisor) — https://cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods
* kind — https://kind.sigs.k8s.io/ · *kind and CNI (disableDefaultCNI)* — https://kind.sigs.k8s.io/docs/user/configuration/#disable-default-cni
* Calico — https://docs.tigera.io/calico/latest/about/ · Cilium — https://docs.cilium.io/
* Argo CD — https://argo-cd.readthedocs.io/ · *Projects* — https://argo-cd.readthedocs.io/en/stable/user-guide/projects/ · *Automated sync (prune, selfHeal)* — https://argo-cd.readthedocs.io/en/stable/user-guide/auto_sync/ · *Diffing customization (ignoreDifferences)* — https://argo-cd.readthedocs.io/en/stable/user-guide/diffing/
* Flux — https://fluxcd.io/flux/ · OpenGitOps — https://opengitops.dev/
* KEDA — https://keda.sh/docs/ · Kyverno — https://kyverno.io/docs/ · Gatekeeper — https://open-policy-agent.github.io/gatekeeper/website/
* kubeconform — https://github.com/yannh/kubeconform · kustomize — https://kubectl.docs.kubernetes.io/references/kustomize/
* AWS ECS on Fargate (Firecracker) — https://docs.aws.amazon.com/AmazonECS/latest/developerguide/AWS_Fargate.html

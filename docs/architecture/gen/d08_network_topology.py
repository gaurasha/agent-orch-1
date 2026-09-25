from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE, AMBER, K8S = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf", "#b8860b", "#326ce5"

def build():
    d = Diagram(1560, 1330, "08 · Kubernetes topology — namespaces, NetworkPolicy, RBAC and the GitOps loop",
                "What the cluster enforces regardless of what our code does. Every arrow here is a NetworkPolicy rule; anything not drawn is dropped by the CNI.")

    d.zone(40, 86, 1000, 640, "namespace agentorch · PSA restricted · default-deny in both directions", "k8s",
           "deploy/k8s/base/{platform,networkpolicy,rbac,postgres}.yaml · every pod: runAsNonRoot, RO rootfs, drop ALL, seccomp RuntimeDefault", subtitle_inside=True)
    d.zone(1080, 86, 440, 640, "namespace agentorch-sandboxes", "sandbox",
           "PSA restricted · RuntimeClass gvisor · tainted pool · quota · no SA token", subtitle_inside=True)

    d.box("ingress", 70, 132, 260, 60, "Ingress / LB (yours)", kind="external",
          lines=["TLS termination · OIDC in production", "→ Service controlplane :8080"])
    d.box("cp", 70, 226, 260, 128, "controlplane · Deployment ×2", kind="platform",
          lines=["Service controlplane :8080", "SA: none mounted (automount=false)",
                 "ingress: {} (anything) — put an Ingress in front", "egress: postgres:5432 · kube-dns:53",
                 "PDB minAvailable=1", "readiness + liveness: GET /healthz"])
    d.box("agentd", 70, 388, 260, 128, "agentd · Deployment ×3 · HPA 2–40", kind="platform",
          lines=["no Service (initiates only)", "SA: none mounted",
                 "egress: postgres:5432 · toolgateway:8081 · dns", "NOTHING else — no internet, no API server",
                 "HPA cpu 70 % · up 100 %/30 s · down 25 %/60 s", "graceful stop → fenced YieldRun"])
    d.box("gw", 400, 226, 300, 150, "toolgateway · Deployment ×2", kind="tcb",
          lines=["Service toolgateway :8081", "SA agentorch-toolgateway (the ONLY SA)",
                 "ingress: from agentd:8081 only", "egress: postgres · dns · 443/6443 to 0.0.0.0/0",
                 "  EXCEPT 169.254.0.0/16 (cloud metadata)", "PDB minAvailable=1 — on every tool call's path"])
    d.box("pg", 400, 410, 300, 106, "postgres · StatefulSet ×1", kind="state",
          lines=["Service postgres :5432", "ingress: from controlplane, agentd, toolgateway",
                 "egress: [] — the database initiates nothing", "production: managed HA Postgres instead"])
    d.box("proxy", 770, 226, 240, 106, "egress-proxy (designed)", kind="tcb",
          lines=["Service :3128", "the broker sandbox's only route out",
                 "per-run allowlist · CONNECT only", "not in the PoC manifests yet"])
    d.box("rbac", 770, 388, 240, 128, "RBAC · Role sandbox-manager", kind="k8s", mono=True,
          lines=["namespace: agentorch-sandboxes", "pods: create get list watch delete", "pods/log: get",
                 "NOT: pods/exec · secrets · configmaps", "     pods/portforward · cluster-*", "bound to SA agentorch-toolgateway"])
    d.box("cfg", 70, 550, 630, 70, "Secret agentorch-signing (AGENTORCH_SECRET) · Secret agentorch-postgres · ConfigMap agentorch-config", kind="k8s",
          lines=["the run-token HMAC key and the DB password are the only secrets in the cluster — tenant credentials NEVER live in etcd",
                 "envFrom configMapRef; secretKeyRef for the two secrets; ArgoCD manages the manifests, a sealed-secrets/ESO layer is the production step"])
    d.box("dns", 770, 550, 240, 70, "kube-system · kube-dns", kind="neutral",
          lines=["UDP/TCP 53, selected by label", "the only cross-namespace egress", "the platform pods are allowed"])

    d.box("asbx", 1110, 132, 380, 150, "sandbox pod (agent) · sbx-<run>-<rand>", kind="sandbox",
          lines=["runtimeClassName: gvisor (handler runsc)", "NetworkPolicy sandbox-default-deny:",
                 "  podSelector {} · Ingress+Egress · no rules → nothing", "  not even DNS",
                 "created by toolgateway via the API server (RBAC above)", "pod-per-call: 1–3 s; the design warms per-run pods"])
    d.box("bsbx", 1110, 316, 380, 128, "sandbox pod (credential broker)", kind="broker",
          lines=["label agentorch.io/sandbox-role=credential-broker",
                 "NetworkPolicy sandbox-broker-egress-via-proxy:", "  egress → agentorch/egress-proxy:3128",
                 "  egress → kube-system/kube-dns:53", "  nothing else — the token can only go through the proxy"])
    d.note(1110, 470, 380, kind="sandbox", title="Node pool + quota (runtimeclass.yaml, quota.yaml, namespaces.yaml)",
           lines=["RuntimeClass scheduling: nodeSelector agentorch.io/workload=sandbox",
                  "  + toleration for the NoSchedule taint → sandboxes land only there",
                  "ResourceQuota sandbox-ceiling: hard cap on pods / cpu / memory per namespace",
                  "LimitRange sandbox-limits: defaults + max per container → Guaranteed QoS",
                  "PSA restricted: no privileged, no hostPath, no host namespaces, non-root",
                  "a successful escape lands on a node with no platform pods and no SA tokens"])

    # allowed flows = NetworkPolicy rules
    d.arrow("ingress", "cp", "", src_side="s", dst_side="n", color=K8S)
    d.arrow("cp", "pg", ":5432", src_side="e", dst_side="w", src_off=16, dst_off=-30, via=[(365, 306), (365, 433)], color=GREEN, label_at=(365, 318))
    d.arrow("agentd", "pg", ":5432", src_side="e", dst_side="w", src_off=20, dst_off=10, color=GREEN, label_at=(365, 470))
    d.arrow("agentd", "gw", ":8081 · the only ingress the gateway accepts", src_side="e", dst_side="w", src_off=-52, dst_off=40,
            via=[(380, 400), (380, 341)], color=PURPLE, width=2, label_at=(300, 372))
    d.arrow("gw", "pg", ":5432", src_side="s", dst_side="n", src_off=-100, dst_off=-100, color=GREEN, label_at=(480, 393))
    d.arrow("gw", "asbx", "API server :6443 · create / watch / delete pods", src_side="n", dst_side="w", src_off=0, dst_off=-40,
            via=[(550, 205), (1060, 205), (1060, 167)], color=K8S, style="dashed", label_at=(800, 205))
    d.arrow("gw", "proxy", "", src_side="e", dst_side="w", src_off=20, dst_off=20, style="dotted")
    d.arrow("bsbx", "proxy", ":3128 only", src_side="w", dst_side="e", src_off=0, dst_off=-20,
            via=[(1045, 380), (1045, 259)], style="dashed", color=RED, label_at=(1045, 330))
    d.arrow("gw", "rbac", "uses", src_side="e", dst_side="n", src_off=60, dst_off=0, via=[(890, 361)], style="dotted", label_at=(780, 361))
    d.arrow("agentd", "dns", ":53", src_side="s", dst_side="w", src_off=100, dst_off=10, via=[(300, 535), (740, 535), (740, 595)], style="dotted", label_at=(520, 535))

    d.zone(40, 760, 1480, 240, "GITOPS · deploy/argocd/{project,application}.yaml · scripts/argocd-{up,test}.sh", "platform",
           "git is the only write path to the cluster; kubectl edits are reverted within one sync", subtitle_inside=True)
    d.box("repo", 70, 806, 300, 150, "git · deploy/k8s/overlays/local", kind="neutral",
          lines=["kustomize base + overlay", "30 resources, validated by kubeconform", "  (30/30 in make k8s-validate)",
                 "a change is a PR; a rollback is a revert", "images pinned by tag (digest in prod)"])
    d.box("argo", 410, 806, 380, 150, "Argo CD Application agentorch", kind="platform",
          lines=["polls git / webhook → syncPolicy.automated: prune, selfHeal", "ignoreDifferences: HPA-managed replicas on agentd",
                 "  (otherwise every scale event is 'drift')", "CreateNamespace=true · retry with backoff",
                 "health: Deployments Available, StatefulSet Ready"])
    d.box("proj", 830, 806, 660, 150, "Argo CD AppProject agentorch — the blast-radius fence for the deployer itself", kind="tcb",
          lines=["sourceRepos: this repository only · destinations: agentorch, agentorch-sandboxes only",
                 "clusterResourceWhitelist: Namespace, RuntimeClass — the two cluster-scoped kinds we need, nothing else",
                 "namespaceResourceBlacklist: ClusterRole, ClusterRoleBinding — a manifest that tries to grant itself the cluster is refused at sync",
                 "the deployer cannot be turned into cluster-admin by a malicious PR; argocd-test asserts selfHeal reverts an out-of-band `kubectl set image`"])
    d.arrow("repo", "argo", "", src_side="e", dst_side="w", color=K8S)
    d.arrow("argo", "proj", "", src_side="e", dst_side="w", color=PURPLE)

    d.table(40, 1030, [280, 400, 400, 400], [
        [["Kubernetes-level failure"], ["What limits the damage"], ["What you see"], ["Trade-off"]],
        [["node drain / rolling deploy"], ["PDB minAvailable=1 on controlplane and toolgateway; agentd yields fenced"], ["no tool call fails; SSE reconnects; runs continue on other pods"],
         ["drains wait for a second replica; single-replica dev clusters must override the PDB"]],
        [["09:00 burst: 300 agents"], ["admission control 429 + HPA 100 %/30 s scale-up + Eligible() fairness"], ["queue depth grows, per-tenant guarantees hold; workers scale to 40"],
         ["CPU-based HPA lags; the design adds a queue-depth metric (KEDA / custom metrics)"]],
        [["sandbox escape (kernel LPE)"], ["gVisor RuntimeClass, tainted pool, no SA token, default-deny, quota"], ["a pod on a node with nothing to steal and nowhere to send it"],
         ["gVisor: ~1–3 s starts, syscall overhead; not every workload runs under runsc"]],
        [["CNI without NetworkPolicy support"], ["kind-up installs Calico; kind-test reads the policy, quota, taint and RBAC can-i back from the API server"], ["the objects the claims depend on, verified on the live cluster"],
         ["kindnet is faster to start; the safety claim is worth the extra minute"]],
    ], kind="k8s")

    d.legend = [("k8s", "Kubernetes object"), ("platform", "platform pod"), ("tcb", "TCB / guard"), ("state", "state"),
                ("sandbox", "agent sandbox pod"), ("broker", "broker sandbox pod"), ("external", "outside"), ("neutral", "shared")]
    return d

if __name__ == "__main__":
    build().save("../svg/08-network-topology.svg")

#!/usr/bin/env bash
# Verifies the DEPLOYED cluster, not the local build.
#
# The point of this script is that a manifest which applies cleanly has proved
# nothing. These checks confirm the properties the manifests are supposed to
# produce, in the cluster, after the fact.
set -uo pipefail
cd "$(dirname "$0")/.."

pass=0; fail=0
check() {
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then
    printf '  [PASS] %s\n' "$name"; pass=$((pass+1))
  else
    printf '  [FAIL] %s\n' "$name"; fail=$((fail+1))
  fi
}
check_not() {
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then
    printf '  [FAIL] %s (the operation SUCCEEDED and should not have)\n' "$name"; fail=$((fail+1))
  else
    printf '  [PASS] %s\n' "$name"; pass=$((pass+1))
  fi
}

echo "== workloads =="
check "control plane is available"  kubectl -n agentorch wait --for=condition=Available deployment/controlplane --timeout=60s
check "tool gateway is available"   kubectl -n agentorch wait --for=condition=Available deployment/toolgateway  --timeout=60s
check "agent workers are available" kubectl -n agentorch wait --for=condition=Available deployment/agentd       --timeout=60s

echo
echo "== pod hardening (the manifests' claims, read back from the API server) =="
for dep in controlplane toolgateway agentd; do
  check "$dep runs as non-root" bash -c \
    "kubectl -n agentorch get deploy $dep -o jsonpath='{.spec.template.spec.securityContext.runAsNonRoot}' | grep -q true"
  check "$dep has a read-only root filesystem" bash -c \
    "kubectl -n agentorch get deploy $dep -o jsonpath='{.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem}' | grep -q true"
  check "$dep drops all capabilities" bash -c \
    "kubectl -n agentorch get deploy $dep -o jsonpath='{.spec.template.spec.containers[0].securityContext.capabilities.drop[0]}' | grep -q ALL"
done

echo
echo "== isolation =="
check "sandbox namespace has a default-deny NetworkPolicy" \
  kubectl -n agentorch-sandboxes get networkpolicy sandbox-default-deny
check "a hard ceiling exists on sandbox resources" \
  kubectl -n agentorch-sandboxes get resourcequota sandbox-ceiling
check "the sandbox node carries the untrusted-workload taint" bash -c \
  "kubectl get nodes -o jsonpath='{.items[*].spec.taints[*].key}' | grep -q agentorch.io/workload"

echo
echo "== least privilege =="
# The gateway needs to create sandbox pods...
check "tool gateway CAN create pods in the sandbox namespace" \
  kubectl auth can-i create pods --namespace agentorch-sandboxes \
    --as=system:serviceaccount:agentorch:agentorch-toolgateway
# ...and must not be able to do the things that would let it escalate.
check_not "tool gateway CANNOT exec into sandbox pods" \
  kubectl auth can-i create pods/exec --namespace agentorch-sandboxes \
    --as=system:serviceaccount:agentorch:agentorch-toolgateway
check_not "tool gateway CANNOT read secrets" \
  kubectl auth can-i get secrets --namespace agentorch \
    --as=system:serviceaccount:agentorch:agentorch-toolgateway
check_not "agent workers CANNOT create pods anywhere" \
  kubectl auth can-i create pods --all-namespaces \
    --as=system:serviceaccount:agentorch:agentorch-agentd

echo
echo "== end to end through the deployed API =="
API="http://localhost:8080"
check "API responds" curl -fsS "$API/healthz"
check "agents are seeded" bash -c \
  "curl -fsS -H 'Authorization: Bearer demo-operator-key' $API/v1/agents | grep -q report-writer"
check_not "an unauthenticated caller is refused" curl -fsS "$API/v1/runs"

RUN=$(curl -fsS -X POST -H 'Authorization: Bearer acme-key' -H 'Content-Type: application/json' \
      -d '{"agent_name":"report-writer","input":"Q3 report."}' "$API/v1/runs" 2>/dev/null \
      | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
if [ -n "$RUN" ]; then
  printf '  [PASS] launched run %s\n' "$RUN"; pass=$((pass+1))
  for _ in $(seq 1 45); do
    STATE=$(curl -fsS -H 'Authorization: Bearer acme-key' "$API/v1/runs/$RUN" 2>/dev/null \
            | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')
    [ "$STATE" = "SUCCEEDED" ] && break
    [ "$STATE" = "FAILED" ] && break
    sleep 2
  done
  if [ "${STATE:-}" = "SUCCEEDED" ]; then
    printf '  [PASS] run completed in the cluster (sandboxed tool calls ran as Pods)\n'; pass=$((pass+1))
  else
    printf '  [FAIL] run ended in state %s\n' "${STATE:-unknown}"; fail=$((fail+1))
  fi
  check_not "globex key cannot read acme's run" bash -c \
    "curl -fsS -H 'Authorization: Bearer globex-key' $API/v1/runs/$RUN"
  check "audit chain verifies" bash -c \
    "curl -fsS -H 'Authorization: Bearer acme-key' '$API/v1/audit?limit=200' | grep -q '\"chain_verified\":true'"
else
  printf '  [FAIL] could not launch a run\n'; fail=$((fail+1))
fi

echo
printf '%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]

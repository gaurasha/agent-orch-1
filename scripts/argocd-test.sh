#!/usr/bin/env bash
# Verifies that Argo CD is not merely installed but actually reconciling.
#
# The test that matters is selfHeal: make a manual change the way a tired
# operator would, and confirm the cluster puts it back. An Argo install that
# does not do this is decoration.
set -uo pipefail

pass=0; fail=0
ok()   { printf '  [PASS] %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  [FAIL] %s\n' "$1"; fail=$((fail+1)); }

echo "== argo cd is running =="
kubectl -n argocd get deployment argocd-server >/dev/null 2>&1 \
  && ok "argocd-server is installed" || bad "argocd-server is not installed"

kubectl -n argocd get application agentorch >/dev/null 2>&1 \
  && ok "the agentorch Application exists" || { bad "no agentorch Application"; exit 1; }

echo
echo "== sync status =="
for _ in $(seq 1 60); do
  SYNC=$(kubectl -n argocd get application agentorch -o jsonpath='{.status.sync.status}' 2>/dev/null)
  HEALTH=$(kubectl -n argocd get application agentorch -o jsonpath='{.status.health.status}' 2>/dev/null)
  [ "$SYNC" = "Synced" ] && [ "$HEALTH" = "Healthy" ] && break
  sleep 5
done
printf '  sync=%s health=%s\n' "${SYNC:-unknown}" "${HEALTH:-unknown}"
[ "${SYNC:-}" = "Synced" ]   && ok "application is Synced"  || bad "application is not Synced"
[ "${HEALTH:-}" = "Healthy" ] && ok "application is Healthy" || bad "application is not Healthy"

echo
echo "== the guardrail is in place =="
kubectl -n argocd get appproject agentorch >/dev/null 2>&1 \
  && ok "AppProject restricts what Argo may deploy" || bad "no AppProject"

echo
echo "== selfHeal: revert a manual change =="
BEFORE=$(kubectl -n agentorch get deployment agentd -o jsonpath='{.spec.template.spec.containers[0].image}')
echo "  current image: $BEFORE"
echo "  simulating an out-of-band kubectl edit..."
kubectl -n agentorch set image deployment/agentd agentd=nginx:latest >/dev/null 2>&1

REVERTED=false
for _ in $(seq 1 60); do
  sleep 5
  NOW=$(kubectl -n agentorch get deployment agentd -o jsonpath='{.spec.template.spec.containers[0].image}')
  if [ "$NOW" = "$BEFORE" ]; then REVERTED=true; break; fi
done
if [ "$REVERTED" = true ]; then
  ok "Argo reverted the manual change (git is the source of truth)"
else
  bad "the manual change survived; selfHeal is not working"
  kubectl -n agentorch set image deployment/agentd "agentd=$BEFORE" >/dev/null 2>&1 || true
fi

echo
printf '%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]

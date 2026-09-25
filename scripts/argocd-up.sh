#!/usr/bin/env bash
# Installs Argo CD into the kind cluster and hands it this repository.
#
# Why GitOps here at all: the whole platform's security story rests on policy
# being reviewable. If an operator can kubectl-edit a NetworkPolicy at 3am and
# nobody notices, the policy is advisory. Argo's selfHeal reverts that within
# seconds, which makes "what is in git" and "what is running" the same thing.
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

if ! kubectl cluster-info >/dev/null 2>&1; then
  echo "No reachable cluster. Run ./scripts/kind-up.sh first." >&2
  exit 1
fi

step "Installing Argo CD"
kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n argocd -f \
  https://raw.githubusercontent.com/argoproj/argo-cd/v2.13.2/manifests/install.yaml

step "Waiting for Argo CD to come up"
kubectl -n argocd rollout status deployment/argocd-server          --timeout=300s
kubectl -n argocd rollout status deployment/argocd-repo-server     --timeout=300s
kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=300s

step "Registering the AppProject guardrail"
kubectl apply -f deploy/argocd/project.yaml

step "Registering the Application"
REPO_URL="${AGENTORCH_REPO_URL:-}"
if [ -z "$REPO_URL" ]; then
  REPO_URL="$(git config --get remote.origin.url 2>/dev/null || true)"
fi
if [ -z "$REPO_URL" ]; then
  echo "Could not determine the repository URL."
  echo "Set it explicitly:  AGENTORCH_REPO_URL=https://github.com/you/agent-orch-1.git $0"
  exit 1
fi
echo "using repository: $REPO_URL"
# Argo clones over the network from inside the cluster, so it needs a URL the
# cluster can reach - a local path will not work.
sed "s#repoURL:.*#repoURL: ${REPO_URL}#" deploy/argocd/application.yaml | kubectl apply -f -

step "Triggering the first sync"
kubectl -n argocd patch application agentorch --type merge \
  -p '{"operation":{"initiatedBy":{"username":"bootstrap"},"sync":{"revision":"HEAD"}}}' || true

PASSWORD=$(kubectl -n argocd get secret argocd-initial-admin-secret \
           -o jsonpath='{.data.password}' 2>/dev/null | base64 -d || echo '<already rotated>')

cat <<MSG

Argo CD is installed and watching this repository.

  UI       : kubectl -n argocd port-forward svc/argocd-server 8090:443
             then open https://localhost:8090
  user     : admin
  password : ${PASSWORD}

  Status   : kubectl -n argocd get application agentorch -o wide

Prove that GitOps is actually enforcing (selfHeal):

  ./scripts/argocd-test.sh
MSG

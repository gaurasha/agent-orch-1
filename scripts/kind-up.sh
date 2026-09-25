#!/usr/bin/env bash
# Creates the local Kubernetes cluster, builds and loads the images, and
# deploys the platform.
#
# This is intentionally one linear script with no hidden state: every step
# prints what it is doing, and each is individually re-runnable.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=agentorch
step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

./scripts/preflight.sh kind

step "Creating the kind cluster (control-plane + platform node + sandbox node)"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "cluster '$CLUSTER' already exists; reusing it"
else
  kind create cluster --config deploy/kind/kind-config.yaml --wait 120s
fi

step "Installing Calico"
# kind's built-in CNI does not implement NetworkPolicy. Every isolation policy
# in deploy/k8s/base would silently do nothing, so we install a CNI that
# enforces them rather than shipping a demo that only appears to be isolated.
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml
echo "waiting for Calico to be ready (this takes a minute or two)..."
kubectl -n kube-system rollout status daemonset/calico-node --timeout=300s
kubectl wait --for=condition=Ready nodes --all --timeout=300s

step "Building images"
docker build -f deploy/docker/Dockerfile         -t agentorch/platform:dev .
docker build -f deploy/docker/Dockerfile.sandbox -t agentorch/sandbox:dev  .

step "Loading images into the cluster"
# kind nodes cannot see the host's Docker images; they must be side-loaded.
kind load docker-image agentorch/platform:dev --name "$CLUSTER"
kind load docker-image agentorch/sandbox:dev  --name "$CLUSTER"

step "Applying manifests"
kubectl apply -k deploy/k8s/overlays/local

step "Waiting for the platform to be ready"
kubectl -n agentorch rollout status statefulset/postgres    --timeout=300s
kubectl -n agentorch rollout status deployment/controlplane --timeout=300s
kubectl -n agentorch rollout status deployment/toolgateway  --timeout=300s
kubectl -n agentorch rollout status deployment/agentd       --timeout=300s

cat <<'MSG'

Cluster is up.

  Console : http://localhost:8080
  API     : curl -H 'Authorization: Bearer demo-operator-key' http://localhost:8080/v1/overview

Verify the deployment actually enforces what it claims:

  ./scripts/kind-test.sh

Tear down:

  kind delete cluster --name agentorch
MSG

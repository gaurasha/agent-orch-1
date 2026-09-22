#!/usr/bin/env bash
# Checks the tools a given target needs, and says exactly what to install.
# Failing early with a clear message beats failing halfway through a cluster
# bootstrap with a cryptic error.
set -euo pipefail

need() {
  local bin="$1" hint="$2"
  if ! command -v "$bin" >/dev/null 2>&1; then
    printf '  MISSING  %-12s %s\n' "$bin" "$hint"
    return 1
  fi
  printf '  ok       %-12s %s\n' "$bin" "$(command -v "$bin")"
}

target="${1:-demo}"
echo "Preflight for target: $target"
rc=0

case "$target" in
  demo)
    need go      "https://go.dev/dl/ (1.24+)"           || rc=1
    if [ "$(uname -s)" != "Linux" ]; then
      echo "  WARNING  the namespace sandbox driver is Linux-only."
      echo "           On macOS or Windows use: make demo-docker"
      rc=1
    fi
    if [ "$(id -u)" != "0" ]; then
      echo "  WARNING  the namespace sandbox driver needs root (or CAP_SYS_ADMIN)"
      echo "           to create namespaces and cgroups. Try: sudo -E make demo"
    fi
    ;;
  docker)
    need docker  "https://docs.docker.com/get-docker/"  || rc=1
    ;;
  ui)
    need node    "https://nodejs.org/ (20+)"            || rc=1
    need npm     "ships with node"                      || rc=1
    ;;
  kind)
    need docker  "https://docs.docker.com/get-docker/"  || rc=1
    need kind    "https://kind.sigs.k8s.io/docs/user/quick-start/#installation" || rc=1
    need kubectl "https://kubernetes.io/docs/tasks/tools/" || rc=1
    ;;
  *)
    echo "unknown target: $target (want: demo|docker|ui|kind)"; exit 2 ;;
esac

if [ "$rc" -ne 0 ]; then
  echo
  echo "Install the missing tools above and re-run."
  exit 1
fi
echo "All good."

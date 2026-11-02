#!/usr/bin/env bash
# Verifies that the operator can reach a model-router's :9091 metrics through the
# per-run NetworkPolicy, which is how the cost dashboard reads in-flight spend.
#
# NetworkPolicies match on labels, so this launches a throwaway pod carrying the
# operator's labels in the operator namespace and fetches the router's metrics
# from it. It also checks that an UNlabeled pod in the same namespace is refused,
# to prove the rule does not open :9091 to the whole namespace.
#
# The operator-labelled probe also checks the scrape carries the routing
# telemetry series (agentorca_modelrouter_telemetry_events_dropped_total is
# exported from the first scrape), so a router image that predates them fails.
#
# Usage: hack/verify-router-metrics.sh <run-namespace> [router-pod]
#   OPERATOR_NAMESPACE  namespace the operator runs in (default: agent-orca-system)
#   IMAGE               image with wget (default: busybox:1.36)
set -euo pipefail

RUN_NS="${1:?usage: $0 <run-namespace> [router-pod]}"
POD="${2:-}"
OP_NS="${OPERATOR_NAMESPACE:-agent-orca-system}"
IMAGE="${IMAGE:-busybox:1.36}"
# A series every current router exports from its first scrape.
REQUIRED_SERIES="agentorca_modelrouter_telemetry_events_dropped_total"

if [[ -z "$POD" ]]; then
  # A running pod that has a model-router container (sidecar or split-pod router).
  POD="$(kubectl -n "$RUN_NS" get pods -l 'agentorca.io/run' -o json |
    jq -r '[.items[] | select(.status.phase=="Running")
            | select(([.spec.containers[]?.name, .spec.initContainers[]?.name] | index("model-router")))
            | .metadata.name][0] // empty')"
  [[ -n "$POD" ]] || { echo "no running run pod with a model-router in $RUN_NS (start an AgentRun first)" >&2; exit 2; }
fi
IP="$(kubectl -n "$RUN_NS" get pod "$POD" -o jsonpath='{.status.podIP}')"
echo "router pod $RUN_NS/$POD at $IP"

probe() { # probe <name> <extra labels...>
  local name="$1"; shift
  kubectl -n "$OP_NS" delete pod "$name" --ignore-not-found --wait=true >/dev/null
  kubectl -n "$OP_NS" run "$name" --restart=Never --image="$IMAGE" --labels="$(IFS=,; echo "$*")" \
    --command -- sh -c "wget -q -T 5 -O - http://$IP:9091/metrics | grep -q '^$REQUIRED_SERIES'" >/dev/null
  local phase=""
  for _ in $(seq 1 40); do
    phase="$(kubectl -n "$OP_NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [[ "$phase" == Succeeded || "$phase" == Failed ]] && break
    sleep 2
  done
  kubectl -n "$OP_NS" delete pod "$name" --wait=false >/dev/null 2>&1 || true
  echo "$phase"
}

trap 'kubectl -n "$OP_NS" delete pod verify-router-allowed verify-router-denied --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT

allowed="$(probe verify-router-allowed app.kubernetes.io/name=agent-orca app.kubernetes.io/component=operator)"
denied="$(probe verify-router-denied app=unrelated)"

echo "operator-labelled pod -> :9091 with routing telemetry series: $allowed (want Succeeded)"
echo "unlabelled pod        -> :9091: $denied (want Failed)"
[[ "$allowed" == Succeeded && "$denied" == Failed ]] || { echo "FAIL" >&2; exit 1; }
echo "OK"

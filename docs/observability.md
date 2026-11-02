# Observability

agent-orca exposes standard Kubernetes-style probes, Prometheus metrics, and
structured audit logging on every external HTTP surface.

## Health probes

All probes are unauthenticated (`publicPaths` in `internal/apiserver/auth.go`).

| Endpoint | Port | Purpose |
|---|---|---|
| `GET /healthz` | 8084, 8000 | Liveness — returns 200 if the server process is alive |
| `GET /readyz` | 8084, 8000 | Readiness — checks K8s API connectivity and Redis (if configured) |
| `GET /version` | 8084, 8000 | Returns build version, commit, and uptime |
| `GET /metrics` | 8084, 8000 | Prometheus-format metrics |

```bash
curl http://localhost:8084/healthz
curl http://localhost:8084/readyz
curl http://localhost:8084/version
curl http://localhost:8084/metrics
```

## Prometheus metrics

Metrics are served from a dedicated registry (`externalReg` in
`internal/apiserver/metrics.go`) so they don't collide with the
controller-runtime manager metrics on `:8443`.

### HTTP API metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `agentorca_external_requests_total` | Counter | `server`, `path`, `method`, `status` | Total HTTP requests to external API surfaces |
| `agentorca_external_request_duration_seconds` | Histogram | `server`, `path` | Request latency distribution |
| `agentorca_external_auth_failures_total` | Counter | `server` | Failed authentication attempts |

### Egress metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `agentorca_egress_published_total` | Counter | `sink_type` | Successfully published results to egress sinks |
| `agentorca_egress_failed_total` | Counter | `sink_type` | Failed egress publishes |

### Model-router metrics

Each run's model-router serves these on `:9091` (see [Reaching agent-pod metrics
behind the NetworkPolicy](#reaching-agent-pod-metrics-behind-the-networkpolicy)).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `agentorca_modelrouter_spend_usd_total` | Counter | `namespace`, `run`, `provider`, `model`, `outcome` | USD spent on LLM calls, by how the call ended |
| `agentorca_modelrouter_llm_tokens_total` | Counter | `direction` (`prompt`/`completion`), `provider` | Tokens providers reported (the billed figures) |
| `agentorca_modelrouter_llm_latency_seconds` | Histogram | `provider` | Routing a call to the end of its response stream |
| `agentorca_modelrouter_llm_ttft_seconds` | Histogram | `provider` | Routing a call to its first token |
| `agentorca_modelrouter_llm_errors_total` | Counter | `provider`, `class` | Failed calls (`rate_limited`, `provider_5xx`, `provider_4xx`, `timeout`, `request_too_large`, `cancelled`, `stream_interrupted`, `provider_error`) |
| `agentorca_modelrouter_routing_decisions_total` | Counter | `strategy`, `path`, `stage` | Selections, by the rule or meta-router path that made them and the task stage. Capability rules report as `rule:capability` |
| `agentorca_modelrouter_routing_fallbacks_total` | Counter | `from`, `to` | Calls served by a fallback after another provider failed |
| `agentorca_modelrouter_tool_calls_total` | Counter | | Tool calls the router dispatched |
| `agentorca_modelrouter_stream_duration_seconds` | Histogram | | Duration of a streaming chat response |
| `agentorca_modelrouter_telemetry_events_dropped_total` | Counter | | Routing events dropped because the operator was unreachable and the queue was full |
| `agentorca_modelrouter_tokens_total` | Counter | | Streamed output **bytes**, a live gauge for the UI; use `llm_tokens_total` for real token counts |

### Routing telemetry

With PostgreSQL configured, every LLM call a router makes also becomes a row in
`routing_events`: the provider and the deciding path, the task stage, tokens,
cost, latency and time to first token, any fallback or error, and how its tool
calls went. It holds metadata only, never prompts or responses. Each run's outcome
(phase, failure class, turns, cost, tool-approval rejections) is written to
`run_outcomes` when the run is archived. Later routing work reads these tables to
judge whether a routing choice was worth its cost.

- Rows older than `--routing-events-retention` (default `720h`, `0` keeps them) are
  deleted by the operator.
- Without PostgreSQL the operator acknowledges and drops posted events; the metrics
  above still work, and a run's page shows the latest 100 routing decisions kept in
  `status.routingDecisions`.
- `GET /api/runs/{namespace}/{name}/routing-events` returns a run's full history,
  oldest first (the run page's "Load full history").

### Controller metrics

The controller-runtime manager exposes additional metrics on `:8080` in the
Helm chart (`--metrics-bind-address=:8080 --metrics-secure=false`; kubebuilder
scaffold default is `:8443` over HTTPS) — these are server-internal and not on
the external API surfaces. They include standard Kubernetes controller metrics
(reconciliation counts, errors, workqueue depth, etc.). These are the metrics
served by the `<release>-metrics` Service and scraped by the chart's
`ServiceMonitor`.

## Audit logging

Every authenticated external API request is written to the operator log as a
structured JSON line with the key `external_api_request`. The log entry
includes:

| Field | Description |
|---|---|
| `tenant` | Tenant name from the authenticated JWT |
| `namespace` | Tenant target namespace |
| `method` | HTTP method |
| `path` | Request path |
| `status` | HTTP response status code |
| `latency_ms` | Request processing time in milliseconds |

Example log line:

```json
{"time":"2024-01-01T12:00:00Z","level":"INFO","msg":"external_api_request","tenant":"acme","namespace":"tenant-acme","method":"POST","path":"/v1/tasks","status":201,"latency_ms":42}
```

## Scraping with Prometheus

agent-orca ships a dedicated `Service` exposing the operator's
controller-runtime metrics on port **8080** (`<release>-metrics`), plus
Prometheus-format `/metrics` endpoints on the External Task API (**8084**) and
the ACP API (**8000**). Use the `server` label to distinguish the API metrics.

You do **not** need a separate external service to collect these metrics — they
are plain Prometheus/OpenMetrics HTTP endpoints. The only question is how a
scraper discovers and reaches them. In-cluster Prometheus (via the Prometheus
Operator) is the supported default; Datadog and any OpenMetrics consumer can scrape
the *same* Service/Pod endpoints with no code changes.

### Prometheus Operator (`ServiceMonitor` / `PodMonitor`)

When the `monitoring.coreos.com` CRDs are installed (the project's `charts/cluster`
and `cloudnative-pg` charts already depend on them if they are enabled). The `agent-orca` Helm chart
renders the following when `metrics.serviceMonitor.enabled` and `metrics.modelRouter.podMonitor.enabled` are both `true`:

- a `ServiceMonitor` scraping the operator metrics `Service` (port 8080), and
- a `PodMonitor` scraping every model-router sidecar on port 9091.

Toggle them in `values.yaml` under `metrics.`:

```bash
helm upgrade agent-orca ./charts/agent-orca \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.modelRouter.podMonitor.enabled=true
# or disable the whole metrics feature (Service + annotations + monitors):
#   --set metrics.enabled=false
```

### Reaching agent-pod metrics behind the NetworkPolicy

The model-router `PodMonitor` scrapes agent pods by PodIP on `:9091`. Each
AgentRun's `NetworkPolicy` is deny-by-default, so agent-orca opens `:9091` only
from namespaces labeled `metrics: enabled` — the same convention already used
for the operator's own metrics port (8080). Label your scraper namespace:

```bash
kubectl label namespace prometheus-operator metrics=enabled
```

### In-flight spend in the cost dashboard

The operator scrapes the model-router's `:9091` itself to show spend for runs that
have not finished (`inFlightUSD` in `GET /api/costs`). A running run's
`status.spendUSD` stays empty until the controller stamps it at a terminal state or
retry, so the router's `agentorca_modelrouter_spend_usd_total` counter is the only
live source.

- Only **unfinished** runs are read, one request per run's (router) pod, in parallel
  with a 3s overall budget and at most 50 pods per request. Finished runs use their
  stamped status, so a warm pod that still holds an old run's series is not counted twice.
- Every per-run NetworkPolicy therefore also admits the operator's own pods on `:9091`
  (namespace `POD_NAMESPACE`, pods labeled `app.kubernetes.io/name: agent-orca` and
  `app.kubernetes.io/component: operator`), in addition to `metrics: enabled`
  namespaces. No other pod in the operator's namespace and no other port is opened.
- When a run's router cannot be read (pod not scheduled yet, scrape timeout, policy
  mismatch), the response reports it in `inFlightUnscraped` instead of treating the
  run as $0.

To check that a cluster's policies let the operator reach a router, run
`hack/verify-router-metrics.sh <namespace> [pod]`.

### Datadog / external scrapers

Point a Datadog OpenMetrics integration at the **same** targets:

- Operator metrics: `http://<release>-metrics.<namespace>.svc:8080/metrics`
- model-router metrics: discover pods with the `metrics` container port (9091)
  via the Datadog Kubernetes/Agent integration, or set a pod-level
  `prometheus.io/scrape: "true"` check.

The `prometheus.io/scrape`, `prometheus.io/port`, and `prometheus.io/path`
annotations are added to the operator pod by default (`metrics.enabled=true`),
so Datadog auto-discovery works without a ServiceMonitor.

### Static scrape config (plain Prometheus, no Operator)

```yaml
scrape_configs:
  - job_name: "agent-orca-operator"
    static_configs:
      - targets: ["<release>-metrics.<namespace>.svc:8080"]
    metrics_path: /metrics
  - job_name: "agent-orca-external"
    static_configs:
      - targets: ["<release>-internal-api.<namespace>.svc:8084"]
    metrics_path: /metrics
```

Both the External Task API (8084) and ACP API (8000) expose metrics on the
same path. Use the `server` label to distinguish them.

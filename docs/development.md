# Development Guide

## Project Structure

**Single-group layout (default):**
```
cmd/main.go                    Manager entry (registers controllers/webhooks)
api/<version>/*_types.go       CRD schemas (+kubebuilder markers)
api/<version>/zz_generated.*   Auto-generated (DO NOT EDIT)
internal/controller/*          Reconciliation logic
internal/webhook/*             Validation/defaulting (if present)
config/crd/bases/*             Generated CRDs (DO NOT EDIT)
config/rbac/role.yaml          Generated RBAC (DO NOT EDIT)
config/samples/*               Example CRs (edit these)
Makefile                       Build/test/deploy commands
PROJECT                        Kubebuilder metadata Auto-generated (DO NOT EDIT)
```

**Multi-group layout** (for projects with multiple API groups):
```
api/<group>/<version>/*_types.go       CRD schemas by group
internal/controller/<group>/*          Controllers by group
internal/webhook/<group>/<version>/*   Webhooks by group and version (if present)
```

Multi-group layout organizes APIs by group name (e.g., `batch`, `apps`). Check the `PROJECT` file for `multigroup: true`.

**To convert to multi-group layout:**
1. Run: `kubebuilder edit --multigroup=true`
2. Move APIs: `mkdir -p api/<group> && mv api/<version> api/<group>/`
3. Move controllers: `mkdir -p internal/controller/<group> && mv internal/controller/*.go internal/controller/<group>/`
4. Move webhooks (if present): `mkdir -p internal/webhook/<group> && mv internal/webhook/<version> internal/webhook/<group>/`
5. Update import paths in all files
6. Fix `path` in `PROJECT` file for each resource
7. Update test suite CRD paths (add one more `..` to relative paths)

## Critical Rules

### Never Edit These (Auto-Generated)
- `config/crd/bases/*.yaml` - from `make manifests`
- `config/rbac/role.yaml` - from `make manifests`
- `config/webhook/manifests.yaml` - from `make manifests`

## Documentation Index

| Doc | Audience | What it covers |
|---|---|---|
| [README.md](../README.md) | All | High-level overview, quickstart, API endpoint table |
| [integrating.md](integrating.md) | Developers / integrators | End-to-end integration guide (CLI, SDKs, ACP API, observability) |
| [openapi-spec.md](openapi-spec.md) | Developers | OpenAPI contract reference (external + ACP specs) |
| [acp-api.md](acp-api.md) | Developers | ACP API reference (agent discovery, run execution, sessions) |
| [admin-api.md](admin-api.md) | Operators | Admin API reference (tenant lifecycle management) |
| [rate-limiting.md](rate-limiting.md) | Operators | Rate limiting, budget enforcement, quota responses |
| [aoctl-reference.md](aoctl-reference.md) | Developers | Complete `aoctl` CLI reference (all subcommands) |
| [observability.md](observability.md) | Operators | Health probes, Prometheus metrics, audit logging |
| [egress-sinks.md](egress-sinks.md) | Operators | Kafka/PubSub/Redis egress configuration + AgentDeployment input sources |
| [crds.md](crds.md) | Operators | All CRD definitions and field references |
| [auth.md](auth.md) | Operators | Authentication model (OAuth2, OIDC, ServiceAccount, trust) |
| [oauth-login.md](oauth-login.md) | Operators | Interactive OIDC login (auth-code flow, session cookies, ID-token mapping) |
| [enterprise-integration.md](enterprise-integration.md) | Enterprise | End-to-end enterprise integration (tenant setup, webhooks, guardrails) |
| [rag.md](rag.md) | Developers | KnowledgeBase / RAG integration |
| [redis.md](redis.md) | Operators | Redis setup and configuration |
| [mcp-access-control.md](mcp-access-control.md) | Operators | MCP server access control |
| [filesystem-access.md](filesystem-access.md) | Developers / Operators | ACP `fs/*` relay — agents drive the user's local workspace via the editor bridge |
| [agentworkflow.md](agentworkflow.md) | Developers | AgentWorkflow CRD (declarative DAG) |
| [alternatives.md](alternatives.md) | Architects | agent-orca vs OpenClaw comparison |
| [cost-tracking.md](cost-tracking.md) | Operators | LLM spend tracking |
| [demos.md](demos.md) | Developers | Demo catalog |
| [local-model-selection.md](local-model-selection.md) | Developers | Local model selection |
| [mcp-apps.md](mcp-apps.md) | Developers | MCP Apps (sandboxed iframe UI) |
| [serviceaccount-iam.md](serviceaccount-iam.md) | Operators | ServiceAccount & IAM identity |
| [testing-tools.md](testing-tools.md) | Developers | Testing MCP and tool calls |
| [ui-proxy.md](ui-proxy.md) | Operators | UIProxy (B4F for the UI) |
| [ui-testing.md](ui-testing.md) | Developers | UI testing (unit, component, E2E) |
| [upgrading-qdrant.md](upgrading-qdrant.md) | Operators | Upgrading Qdrant for KnowledgeBases |
| [vector-database-selection.md](vector-database-selection.md) | Architects | Why Qdrant |

## OpenAPI spec generation

The OpenAPI specs live in `internal/apiserver/schemas/`:

- `openapi-external.yaml` — External Task API (port 8084)
- `openapi-acp.yaml` — ACP API (port 8000)

```bash
make openapi            # copy specs to ./openapi/
make openapi-validate   # validate specs (requires redocly or swagger-cli)
```

Both specs are served at runtime at `GET /openapi.json` on their respective
ports and are also listed in the [openapi-spec.md](openapi-spec.md) reference.
- `**/zz_generated.*.go` - from `make generate`
- `PROJECT` - from `kubebuilder [OPTIONS]`

### Never Remove Scaffold Markers
Do NOT delete `// +kubebuilder:scaffold:*` comments. CLI injects code at these markers.

### Keep Project Structure
Do not move files around. The CLI expects files in specific locations.

### Always Use CLI Commands
Always use `kubebuilder create api` and `kubebuilder create webhook` to scaffold. Do NOT create files manually.

### E2E Tests Require an Isolated Kind Cluster
The e2e tests are designed to validate the solution in an isolated environment (similar to GitHub Actions CI).
Ensure you run them against a dedicated [Kind](https://kind.sigs.k8s.io/) cluster (not your “real” dev/prod cluster).

### Dockerfile Source COPY Lines Are Generated
Each Go image copies only the packages its binary imports, so an edit to `internal/rag`
does not rebuild the model-router (and skaffold does not rebuild it either). The
`# BEGIN generated go-deps` … `# END generated go-deps` block in `Dockerfile`,
`cmd/model-router/Dockerfile`, `cmd/oauth-proxy/Dockerfile`, `Dockerfile.mcp-ingester`,
`Dockerfile.ui-proxy` and `Dockerfile.dev-sandbox` is written by
`hack/gen-docker-copies` from `go list -deps`; never edit it by hand.

```
make docker-deps          # Rewrite the blocks after adding or removing an import
make verify-docker-deps   # What CI runs: fails when a block is stale
```

If an image build fails with a missing package, the import is new: run `make docker-deps`.
A new Dockerfile that builds Go code gets the two marker lines where its `COPY` lines go,
and an entry in `targets` in `hack/gen-docker-copies/main.go`. `.dockerignore` stays an
allowlist: a directory a Dockerfile needs must also be re-included there.

## After Making Changes

**After editing `*_types.go` or markers:**
```
make manifests  # Regenerate CRDs/RBAC from markers
make generate   # Regenerate DeepCopy methods
```

**After editing `*.go` files:**
```
make lint-fix   # Auto-fix code style
make test       # Run unit tests
make docker-deps  # If you added or removed an import of a repo package
```

## CLI Commands Cheat Sheet

### Create API (your own types)
```bash
kubebuilder create api --group <group> --version <version> --kind <Kind>
```

### Deploy Image Plugin (scaffold to deploy/manage ANY container image)

Generate a controller that deploys and manages a container image (nginx, redis, memcached, your app, etc.):

```bash
# Example: deploying memcached
kubebuilder create api --group example.com --version v1alpha1 --kind Memcached \
  --image=memcached:alpine \
  --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Scaffolds good-practice code: reconciliation logic, status conditions, finalizers, RBAC. Use as a reference implementation.


### Create Webhooks
```bash
# Validation + defaulting
kubebuilder create webhook --group <group> --version <version> --kind <Kind> \
  --defaulting --programmatic-validation

# Conversion webhook (for multi-version APIs)
kubebuilder create webhook --group <group> --version v1 --kind <Kind> \
  --conversion --spoke v2
```

### Controller for Core Kubernetes Types
```bash
# Watch Pods
kubebuilder create api --group core --version v1 --kind Pod \
  --controller=true --resource=false

# Watch Deployments
kubebuilder create api --group apps --version v1 --kind Deployment \
  --controller=true --resource=false
```

### Controller for External Types (e.g., from other operators)

Watch resources from external APIs (cert-manager, Argo CD, Istio, etc.):

```bash
# Example: watching cert-manager Certificate resources
kubebuilder create api \
  --group cert-manager --version v1 --kind Certificate \
  --controller=true --resource=false \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

**Note:** Use `--external-api-module=<module>@<version>` only if you need a specific version. Otherwise, omit `@<version>` to use what's in go.mod.

### Webhook for External Types

```bash
# Example: validating external resources
kubebuilder create webhook \
  --group cert-manager --version v1 --kind Issuer \
  --defaulting \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

## Testing & Development

```bash
make test              # Run unit tests (uses envtest: real K8s API + etcd)
```

Tests use **Ginkgo + Gomega** (BDD style). Check `suite_test.go` for setup.

## Local Development with Skaffold

Skaffold is the primary tool for local development. It builds all container images,
deploys via Helm, and watches for file changes — rebuilding and redeploying automatically.

### Prerequisites

- A local Kind cluster named `agent-orca-dev`:
  ```bash
  kind create cluster --name agent-orca-dev
  ```
- [Skaffold](https://skaffold.dev/docs/install/) installed
- API keys exported as environment variables (used by the `dev` profile):
  ```bash
  export OPENAI_API_KEY=sk-...
  export ANTHROPIC_API_KEY=sk-ant-...
  export GOOGLE_API_KEY=AIza...
  ```

### Day-to-day workflow

```bash
# Start the dev loop (builds, deploys, watches for changes, port-forwards UI to localhost:8080)
skaffold dev

# The 'dev' profile auto-activates on the kind-agent-orca-dev context.
# It deploys: agent-orca operator + model-providers + UI, with webhooks in Ignore mode.
# It also deploys an in-cluster Ollama (nomic-embed-text) for local embeddings
# via config/samples/ollama-embedding-cluster.yaml (applied as raw manifests).
```

Skaffold watches for Go and Dockerfile changes. When you save a file, it rebuilds
the affected image(s), loads them into Kind, and re-deploys the Helm release.
The UI is port-forwarded to `http://localhost:8080`.

### Deploying demos

Demos are deployed as separate Skaffold profiles on top of a running `skaffold dev` session:

```bash
# Deploy a demo (one-shot, does not watch for changes)
skaffold run -p demo-soc-triage
skaffold run -p demo-escalation-chain
skaffold run -p demo-codebase-expert

# Remove a demo using skaffold
skaffold delete -p demo-soc-triage
skaffold delete -p demo-escalation-chain
skaffold delete -p demo-codebase-expert

# Remove a demo using helm
helm uninstall demo-soc-triage -n agent-orca-system
```

Each demo profile is documented in `skaffold.yaml` with its prerequisites.

#### The reference agent image

Demos and `hack/test-agents.sh` run agents from the openai reference image
`ghcr.io/heddles/agent-orca/openai-reference:latest` — a minimal OpenAI-compatible
agent (the model-router injects the system prompt, tools, prior context, and built-in
tool resolution, so the image itself is persona-free). `skaffold dev` builds and kind-loads
this image as `:latest` automatically (a non-fatal build hook; Skaffold's tag policy is
global SHA-256, so the reference image is built separately as `:latest` to match the demos'
pin). It is also **published to GHCR (public, no credentials needed)** on every release, so
`skaffold run -p demo-*` on its own, or production clusters, can pull it directly.

To run demos offline, or before the image has been published to a release, build and load
it into Kind once (this is also the command to force a rebuild after editing `agent.py`):

```bash
docker build -t ghcr.io/heddles/agent-orca/openai-reference:latest \
  -f examples/agent-sdk-template/Dockerfile .
kind load docker-image ghcr.io/heddles/agent-orca/openai-reference:latest --name agent-orca-dev
```

To run your own agent image instead, replace `ociRef` in the `Agent` manifest and set
`systemPrompt`/`tools` on the `Agent` CR — see [agent-images.md](agent-images.md) for the
full contract (framework tiers, injected env vars, built-in tools).


### Debugging

```bash
# Operator logs
kubectl logs -n agent-orca-system deployment/agent-orca-operator -f

# Model-router sidecar logs for a specific run
kubectl logs -n agent-orca-system <pod-name> -c model-router -f

# Watch AgentRun status
kubectl get agentrun -n agent-orca-system -w
```

### API Design

**Key markers for** `api/<version>/*_types.go`:

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"

// On fields:
// +kubebuilder:validation:Required
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:MaxLength=100
// +kubebuilder:validation:Pattern="^[a-z]+$"
// +kubebuilder:default="value"
```

- **Use** `metav1.Condition` for status (not custom string fields)
- **Use predefined types**: `metav1.Time` instead of `string` for dates
- **Follow K8s API conventions**: Standard field names (`spec`, `status`, `metadata`)

### Controller Design

**RBAC markers in** `internal/controller/*_controller.go`:

```go
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
```

**Implementation rules:**
- **Idempotent reconciliation**: Safe to run multiple times
- **Re-fetch before updates**: `r.Get(ctx, req.NamespacedName, obj)` before `r.Update` to avoid conflicts
- **Structured logging**: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- **Owner references**: Enable automatic garbage collection (`SetControllerReference`)
- **Watch secondary resources**: Use `.Owns()` or `.Watches()`, not just `RequeueAfter`
- **Finalizers**: Clean up external resources (buckets, VMs, DNS entries)

### Logging

**Follow Kubernetes logging message style guidelines:**

- Start from a capital letter
- Do not end the message with a period
- Active voice: subject present (`"Deployment could not create Pod"`) or omitted (`"Could not create Pod"`)
- Past tense: `"Could not delete Pod"` not `"Cannot delete Pod"`
- Specify object type: `"Deleted Pod"` not `"Deleted"`
- Balanced key-value pairs

```go
log.Info("Starting reconciliation")
log.Info("Created Deployment", "name", deploy.Name)
log.Error(err, "Failed to create Pod", "name", name)
```

**Reference:** https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines

### Webhooks
- **Create all types together**: `--defaulting --programmatic-validation --conversion`
- **When`--force`is used**: Backup custom logic first, then restore after scaffolding
- **For multi-version APIs**: Use hub-and-spoke pattern (`--conversion --spoke v2`)
  - Hub version: Usually oldest stable version (v1)
  - Spoke versions: Newer versions that convert to/from hub (v2, v3)
  - Example: `--group crew --version v1 --kind Captain --conversion --spoke v2` (v1 is hub, v2 is spoke)

### Learning from Examples

The **deploy-image plugin** scaffolds a complete controller following good practices. Use it as a reference implementation:

```bash
kubebuilder create api --group example --version v1alpha1 --kind MyApp \
  --image=<your-image> --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Generated code includes: status conditions (`metav1.Condition`), finalizers, owner references, events, idempotent reconciliation.

## Container Image References

The operator spawns several workloads at runtime. Their images are configurable via
Helm values so you can point at an internal registry or pin specific versions.

| Component | Helm Value | Env Var | Default |
|-----------|-----------|---------|---------|
| Operator | `operator.image.repository` / `tag` | — | `ghcr.io/heddles/agent-orca/operator:latest` |
| Model Router (sidecar) | `modelRouter.image.repository` / `tag` | `MODEL_ROUTER_IMAGE` | `ghcr.io/heddles/agent-orca/model-router:latest` |
| MCP Ingester (KB jobs) | `mcpIngester.image.repository` / `tag` | `MCP_INGESTER_IMAGE` | `ghcr.io/heddles/agent-orca/mcp-ingester:latest` |
| Qdrant (KB vector store) | `qdrant.image.repository` / `tag` | `QDRANT_IMAGE` | `qdrant/qdrant:v1.17.1` |

All image references support `global.imageRegistry` as a prefix (e.g. set to
`registry.internal.company.com/` to mirror all images).

During local development with `skaffold dev`, all image references are set automatically
via `setValueTemplates` in `skaffold.yaml`. You do not need to configure them manually.

### Production deployment

```bash
helm install agent-orca charts/agent-orca \
  --namespace agent-orca-system --create-namespace \
  --set mcpIngester.image.repository=my-registry/mcp-ingester \
  --set mcpIngester.image.tag=v0.2.0 \
  --set qdrant.image.tag=v1.13.0
```

## References

### Essential Reading
- **Kubebuilder Book**: https://book.kubebuilder.io (comprehensive guide)
- **controller-runtime FAQ**: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md (common patterns and questions)
- **Good Practices**: https://book.kubebuilder.io/reference/good-practices.html (why reconciliation is idempotent, status conditions, etc.)
- **Logging Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines (message style, verbosity levels)

### API Design & Implementation
- **API Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
- **Operator Pattern**: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
- **Markers Reference**: https://book.kubebuilder.io/reference/markers.html

### Tools & Libraries
- **controller-runtime**: https://github.com/kubernetes-sigs/controller-runtime
- **controller-tools**: https://github.com/kubernetes-sigs/controller-tools
- **Kubebuilder Repo**: https://github.com/kubernetes-sigs/kubebuilder

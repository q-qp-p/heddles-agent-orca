# Sandboxed agents (NVIDIA OpenShell)

> To give an agent a shell without running the agent itself in a sandbox, see [Sandbox shell](sandbox-shell.md).

An Agent with `spec.runtime.sandbox` runs its agent process in an
[NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) sandbox instead of a plain
pod. The agent gets a real shell: it can run builds and tests and install the tools
it needs. The sandbox confines what it can touch:

- **Filesystem**: Landlock. The workdir and `/tmp` are writable; system paths are read-only.
- **Syscalls**: seccomp, with a non-root user and no capabilities.
- **Network**: every outbound connection goes through a supervisor that allows only the
  hosts you list, with HTTP method/path checks per host and optionally per binary.

The model-router is unchanged. It runs in its own pod (split-pod topology) and remains
the only path to LLMs, so routing, fallbacks, compaction, quotas and guardrails work
as before. Provider API keys never enter the sandbox.

## Prerequisites

1. **Agent Sandbox** controller and CRDs (the Kubernetes SIG project that OpenShell's
   Kubernetes driver uses):

   ```shell
   kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/sandbox.yaml
   ```

2. **OpenShell gateway**, v0.1.5 (the version the operator's SDK is pinned to):

   ```shell
   helm upgrade --install openshell oci://ghcr.io/nvidia/openshell/helm-chart \
     --version 0.1.5 --namespace openshell --create-namespace \
     --set server.telemetryEnabled=false \
     --set server.drivers.kubernetes.allowDriverConfig=true
   ```

   `allowDriverConfig` lets gateway clients set Kubernetes pod settings on a sandbox
   (node selector, tolerations, runtime class). agent-orca uses it only for the
   operator-wide [placement](#placement), which only cluster admins set; Agents can't.
   Enable it when you configure placement. The operator should be the gateway's only
   client, because any client holding a gateway certificate gets the same power.
   Persistent-volume attachments still need the gateway's resource-admission labels.

3. **Kernel**: Landlock ABI v3+ (Linux 6.2+) on the nodes that run sandboxes. agent-orca
   sets `landlock.compatibility: hard_requirement`, so a sandbox on an older kernel fails
   rather than silently running without filesystem rules.

4. **CNI**: one that enforces NetworkPolicy (Calico, Cilium, ...). Both OpenShell's
   workload fence and agent-orca's router policies depend on it.

## Enable it in agent-orca

Give the operator a gateway client certificate. The OpenShell chart generates one in
`openshell-client-tls`; copy it to the agent-orca namespace:

```shell
kubectl get secret openshell-client-tls -n openshell -o json \
  | jq 'del(.metadata.namespace,.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp,.metadata.ownerReferences)' \
  | kubectl apply -n agent-orca-system -f -
```

Then set the chart values:

```yaml
openshell:
  enabled: true
  gateway:
    address: openshell.openshell.svc.cluster.local:8080
    namespace: openshell
  workspace: default
  sandboxNamespace: openshell   # the gateway chart's server.sandboxNamespace
  tls:
    secretName: openshell-client-tls
```

For an OIDC gateway, set `openshell.tokenSecret.name` to a Secret holding a bearer token
instead of (or in addition to) the client certificate.

### Placement

By default, sandbox pods schedule wherever the cluster puts them. Builds and tests are
heavy, so give sandboxes their own node pool. Make sure they can't land on the
control plane: an untainted control-plane node is a valid target. Then point the
operator at the pool:

```yaml
openshell:
  placement:
    nodeSelector:
      agentorca.io/sandbox: "true"
    tolerations:
      - {key: agentorca.io/sandbox, operator: Exists, effect: NoSchedule}
    runtimeClassName: ""     # e.g. a gVisor or Kata RuntimeClass
    priorityClassName: ""
```

Placement applies to every sandboxed agent and requires the gateway's
`allowDriverConfig` (see Prerequisites). A gateway without it rejects every sandbox
while placement is set. Placement is operator configuration rather than an Agent field
because a toleration can admit a pod to any tainted node.

### Resources

An Agent's `spec.resources` becomes the sandbox workload's container resources, as for
an agent pod. CPU and memory requests default to their limits when unset, and other
resources such as `ephemeral-storage` are passed through.

### Sandbox classes

A `SandboxClass` is a cluster-scoped sandbox profile written by cluster admins. Agents
use one with `runtime.sandbox.classRef`:

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: SandboxClass
metadata:
  name: coder
spec:
  namespaceSelector:                 # which namespaces' Agents may use it (empty: all)
    matchLabels: {agentorca.io/enable-sandboxes: "true"}
  enforcement: enforce               # Agents of this class cannot weaken it to audit
  egressPresets: [go, npm, pypi, github-read]
  egress:
    - host: artifacts.internal.example.com
  resources:                         # defaults for Agents without spec.resources
    requests: {cpu: "2", memory: 4Gi, ephemeral-storage: 20Gi}
    limits: {cpu: "4", memory: 8Gi, ephemeral-storage: 20Gi}
  nodeSelector: {pool: sandbox}      # replaces the operator-wide placement
  tolerations:
    - {key: openshell, operator: Equal, value: coder-agent, effect: NoExecute}
  credentials:                       # secrets the sandbox can use but never read
    - name: github
      source:
        githubApp:
          repositories: [heddles/agent-orca]
          permissions: {contents: write}
      bind: {host: github.com}
```

The Agent's own sandbox settings are added to the class's, never narrowing it:

| Setting | Effect |
|---|---|
| `egress`, `egressPresets` | Class and Agent rules are combined. Rules for the same host and ports merge into one that allows what either allowed. |
| `filesystem` | Paths are combined. |
| `enforcement` | The stricter of the two. |
| `spec.resources` | The Agent's own, or the class's when it sets none. |
| Placement | The class's when it sets any, else the operator's `openshell.placement`. Agents can't set placement. |
| `credentials` | Only the class has them; every sandbox of the class gets all of them. An Agent or workflow can't add, remove or widen one. See [class credentials](#class-credentials). |
| `runtime.sandbox.github` | Unioned with the class's `githubApp` credentials: all repositories, and push if either pushes. |

A run fails with a clear reason if its class doesn't exist or doesn't select the run's
namespace. Admission checks the same.

**Egress presets** are vetted host lists, usable on a class or directly on an Agent
(`runtime.sandbox.egressPresets`):

| Preset | Hosts |
|---|---|
| `go` | `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com` |
| `npm` | `registry.npmjs.org` |
| `pypi` | `pypi.org`, `files.pythonhosted.org` |
| `crates` | `crates.io`, `index.crates.io`, `static.crates.io` |
| `maven` | `repo.maven.apache.org`, `repo1.maven.org` |
| `github-read` | `github.com` (read-write: clone and fetch POST to `git-upload-pack`), `codeload.github.com`, `api.github.com`, `*.githubusercontent.com` |
| `mise` | `mise.jdx.dev`, `mise-versions.jdx.dev`, `api.github.com`, `*.githubusercontent.com` |

All presets are read-only (GET, HEAD, OPTIONS) except `github.com`, and any binary may
use them. Unauthenticated, a POST to `github.com` can't change anything, so `github-read`
never gives write access. To push, give the class a [`githubApp` credential](#class-credentials)
(or an Agent `sandbox.github`); the credential-bound endpoint replaces the plain one.

### Class credentials

A SandboxClass can hold the secrets its sandboxes need: a GitHub token, an API key, a
database password. They are defined once, on the class. A workload picks the class and an
image and declares nothing else, so anything in the image that can use a credential
(`git`, `curl`, `psql`, `terraform`, an SDK) can use it, whatever the workload does.

```yaml
spec:
  egress:
    - {host: api.example.com, access: read-write}   # the class must let the sandbox reach the host
  credentials:
    - name: warehouse
      source:
        secretRef: {name: warehouse-token, key: token}
      bind:
        host: api.example.com
        binaries: [/usr/bin/curl]
        paths: ["/v1/**"]
      inject: {style: bearer}
```

**The sandbox never holds the secret.** Each sandbox gets a placeholder in the environment
variable `AGENTORCA_CRED_<NAME>` (name upper-cased, dashes as underscores). The supervisor
replaces the placeholder with the real value on requests to the bound endpoint, and nowhere
else. The process sends the placeholder like the real thing, e.g.
`curl -H "Authorization: Bearer $AGENTORCA_CRED_WAREHOUSE" https://api.example.com/v1/jobs`.
The supervisor does not add headers, so the process must send it. A command that prints
the variable, or sends it to another host, gets the placeholder.

| Field | |
|---|---|
| `name` | Unique within the class: lower-case letters, digits and dashes. |
| `source` | Exactly one of `githubApp` or `secretRef` (below). |
| `bind.host`, `bind.port` | The only endpoint the value works on. An exact host, not a wildcard. Port defaults to 443. |
| `bind.binaries` | Executables (absolute paths) that may use the value. Empty lets any process use it, and admission warns. |
| `bind.paths` | URL path globs the value works for, e.g. `/v1/**`. Empty means any path. Methods stay limited by the egress access (below). |
| `inject.style` | Where the supervisor replaces the placeholder: `bearer` (the default; `Authorization: Bearer`), `basic` (the value is the password; the client chooses the user) or `header` (the value alone in the header named by `inject.name`). |

**Sources**

| Source | Value |
|---|---|
| `secretRef: {name, key, namespace}` | A key of a Kubernetes Secret, read by the operator when the sandbox is created. `namespace` is optional and defaults to the namespace of the workload that uses the class, so each tenant's workloads get their own value under the same name. Another namespace must be in `allowedNamespaces` of the TenantConfig the workload's namespace belongs to; a namespace in no TenantConfig can read only its own. Otherwise the run fails with a clear reason. |
| `githubApp: {repositories, permissions}` | A one-hour installation token of the operator's GitHub App, narrowed to the listed repositories and valid only for git over HTTPS to `github.com`. `permissions.contents` is `read` (the default: clone and fetch) or `write` (also push); no other permission is accepted. See [git access](#git-access-to-github). |

**Rules**

- A credential binds an endpoint, it does not open one. `bind.host:port` must be reachable
  under the class's `egress` or presets, and the credential gets the access that egress rule
  gives the host: read-only (GET, HEAD, OPTIONS) unless the rule says `read-write` (adds
  POST, PUT, PATCH) or `full` (every method). `bind.paths` narrow that, never widen it.
  Admission rejects the class otherwise.
- A destination carries one credential. Binding it removes the host and port from the
  class's other egress rules, so processes the credential doesn't name (see `bind.binaries`)
  lose access to it. A wildcard egress rule (`*.example.com`) that also covers the bound
  host makes the gateway reject the policy; use an exact host.
- A `githubApp` credential must bind `github.com:443` and takes no `bind.binaries`,
  `bind.paths` or `inject`: it is always git, by the git binary, to its repositories.
- A class holds at most 16 credentials, and a sandbox at most 32 providers with tool
  credentials and git.

**Lifecycle.** Credentials are resolved when the sandbox is created. The value goes into a
per-run OpenShell provider labeled with the run (or workflow), which is deleted when it
ends; the sandbox spec never contains it. A `secretRef` value is not re-read while the
sandbox runs: a rotated Secret reaches the next sandbox. A `githubApp` token is renewed
before it expires.

They apply to the agent's own sandbox (`runtime.sandbox.classRef`), to the
[`sandbox_exec` shell](sandbox-shell.md) (`shell.classRef`) and, for `githubApp`, to
workflow sandboxes. `secretRef` credentials are not wired into workflow-owned sandboxes yet.
Examples: [`sandboxclass-coder.yaml`](../config/samples/sandboxclass-coder.yaml) (GitHub) and
[`sandboxclass-credentials.yaml`](../config/samples/sandboxclass-credentials.yaml) (an API token).

### Network decisions in the run trace

The sandbox supervisor logs every network decision as an OCSF event. The operator copies
them into the run's trace as `egress` events, so the run timeline, live and archived,
shows what the agent reached and what was blocked. In the UI's trace, the **Network**
filter isolates them. There are two kinds:

| `kind` | Event | Fields |
|---|---|---|
| `net` | A connection | `destination` (host:port), `binary`, and the `policy` and `engine` that decided it |
| `http` | A request on an allowed connection, checked by method and path | `method`, `url`, `binary`, `policy`, `engine` |

Both carry `action` (`allowed` or `denied`), a `reason` for denials, and `raw`, the
supervisor's original line. The parser reads OpenShell's shorthand log format, so `raw`
is kept in case that format changes.

The operator reads new decisions on each poll (about every 10 s) and records the time of
the last one in `status.sandboxDecisionsSeen`, so nothing is recorded twice across polls
or operator restarts. Decisions from a run's final seconds are read before its sandbox
is deleted. The metric `agentorca_sandbox_egress_decisions_total{namespace,kind,action}`
counts them.

### Egress approvals

When a sandbox tries to reach a host its policy doesn't list, the connection is denied
and the OpenShell gateway drafts a rule that would allow it. For a class with
`spec.approval`, the operator turns each draft into an approval:

```yaml
spec:
  approval:
    approverGroups: [platform-oncall]   # IdP groups that may decide; empty: any user of the namespace
    timeout: 30m                        # undecided requests are rejected after this (default 30m)
    autoApprove:                        # allowed without a person
      - host: "*.githubusercontent.com"
        binaries: [/usr/bin/curl, /usr/bin/git]
      - host: registry.internal.example.com
        ports: [443, 8443]
```

- **Auto-approve:** a request matching a rule (host or `*.` wildcard, ports defaulting to
  443, optional binary globs) is approved at once. It's recorded as approved by the
  class. A request the gateway's policy prover flags with security notes is never
  auto-approved.
- **Waiting for a person:** any other request becomes a pending approval, and the run
  shows the condition `WaitingForApproval=True`. The sandbox keeps running, but the
  connection stays denied until someone decides.
- **Decisions** are applied to the live sandbox policy within one poll (about 10 s), with
  no restart. An approval lets the agent's next attempt through, and a rejection or
  timeout declines the draft. When the run ends, its pending requests are withdrawn.
- **Only connections are approvable.** A request denied by method or path on an allowed
  host (for example a POST to a read-only registry) isn't drafted, so it can't be
  approved this way.

Approvals are stored in PostgreSQL (the `approvals` table, which needs `postgres.dsn`)
and are the audit record of who decided what, when and why. Without PostgreSQL they're
off, and denied requests stay denied.

**In the UI**, the **Approvals** tab is the inbox. Its badge counts pending requests,
and each request shows:
- the destination and the process asking;
- the gateway's rationale and policy-prover result, plus any security notes;
- the attempt count;
- who may decide, and when the request expires.

Approve or Reject with a reason. **Recent** lists decided requests with who decided and
why. `?tab=approvals` links straight to the inbox.

**Notifications:** set the chart's `approvals.notifyWebhookSecret` to a Secret holding a
Slack incoming-webhook URL (key `url`) to get one message per request that needs a
person. Auto-approved requests and repeats don't send one. The message names the
namespace, run, request, approver groups and deadline. With `approvals.uiURL` set, it
links to the inbox. Any webhook receiver works: the body is `{"text": ..., "approval":
{...}}`.

The UI API serves them:

| Request | |
|---|---|
| `GET /api/approvals?status=pending&namespace=&run=` | Lists approvals in the caller's namespaces |
| `POST /api/approvals/{id}/approve` with body `{"reason": "..."}` | Approves; the deciding user is recorded |
| `POST /api/approvals/{id}/reject` with body `{"reason": "..."}` | Rejects |

Deciding needs a signed-in user: an OIDC session or user token, not a service account. The
user must have access to the approval's namespace and, when the approval names approver
groups, be a member of one. A request already decided by someone else gets 409.

Finally, opt in each namespace that may run sandboxed agents:

```shell
kubectl label namespace agents agentorca.io/enable-sandboxes=true
```

## Write a sandboxed Agent

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata:
  name: coder
  namespace: agents
spec:
  modelSelectorRef: default
  guardrailPolicyRef: default        # required for sandboxed agents
  networkIsolation:
    splitPod: true                   # required: the router stays in its own pod
  runtime:
    ociRef: ghcr.io/acme/coding-agent:1.4
    command: ["python", "-m", "agent"]
    sandbox:
      provider: openshell
      enforcement: audit             # start in audit; switch to enforce when ready
      egress:
        - host: pypi.org             # pip index (read-only: GET/HEAD)
        - host: files.pythonhosted.org
        - host: github.com           # git clone over HTTPS POSTs, so read-write
          access: read-write
          binaries: [/usr/bin/git]   # also matches git's child helper processes
        - host: "*.githubusercontent.com"
      tools:                         # the Agent's tools are reachable by default
        excludeTools: [jira]         # ...except these Tools
        excludeHosts: [uploads.github.com]  # ...and these hosts, whichever tool declares them
      filesystem:
        readOnly: [/opt/venv]        # image paths outside the default set
```

| Field | Default | Meaning |
|---|---|---|
| `provider` | `openshell` | Sandbox runtime. Only `openshell` exists today. |
| `enforcement` | `audit` | `audit` logs HTTP method/path violations and lets them through. `enforce` blocks them. Hosts not listed in `egress` are always denied, in either mode. |
| `egress[].host` | — | Hostname, or a wildcard with at least three labels (`*.example.com`). |
| `egress[].ports` | `[443]` | Destination ports. |
| `egress[].access` | `read-only` | `read-only` (GET/HEAD/OPTIONS), `read-write` (adds POST/PUT/PATCH), or `full`. |
| `egress[].binaries` | any binary | Restrict the rule to these executables (absolute paths or globs). A rule matches the process that connects or any of its parents. List the real path, not a symlink. For scripts, list the interpreter. |
| `tools.inherit` | `true` | Add the Agent's tool endpoints to the sandbox egress (see below). Set `false` to keep tools reachable only through the model-router. |
| `tools.excludeTools` | — | Tool names from `spec.tools` whose endpoints are not added. Unknown names are rejected at admission. |
| `tools.excludeHosts` | — | Hosts that are not added from any tool. Exact match; does not filter `egress`. |
| `filesystem.readOnly` / `readWrite` | — | Extra paths on top of the defaults: the workdir, read-only `/usr`, `/lib`, `/etc`, `/app`, `/proc` and `/var/log`, and read-write `/tmp`. |

HTTPS to listed hosts is decrypted and checked by the supervisor. OpenShell injects its CA
into the usual trust variables (`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`,
`GIT_SSL_CAINFO`, ...), so pip, npm, curl and git work unchanged.

### Tool endpoints

The agent's LLM tool calls always go through the model-router and are unaffected by
the sandbox. In addition, by default the agent process can reach its tools' endpoints
directly, for example to use a CLI or SDK against the same API. For each Tool in
`spec.tools` (builtins like `_clarify` excluded), the sandbox gets one rule,
`agentorca-tool-<name>`, allowing full HTTP access to:

- each `networkEgress` entry that names a host (TCP);
- the `networkEgress` of the MCPServer the tool was generated from;
- the URL of a remote (`http`/`sse`) MCP tool.

Port-only `networkEgress` entries are skipped, because sandbox rules are per host. Loopback
URLs are skipped too: they point at sidecars in the router pod. Stdio MCP tools and agent
tools have no endpoint.

### Authenticated tools

Remote MCP tools with static secret auth (`mcpConfig.auth.bearerToken`, `apiKey` or
`headers`) can be called directly from the sandbox with their credentials, without
the agent ever holding the secret. The operator registers each credential with the
OpenShell gateway as a provider, and the sandbox policy binds it to that tool's MCP
endpoint alone:

- The workload's environment gets an opaque placeholder per secret, named
  `AGENTORCA_<SOURCE>_TOKEN` for a bearer token, `AGENTORCA_<SOURCE>_API_KEY` for an
  API key, or `AGENTORCA_<SOURCE>_<HEADER>` for a custom header. `<SOURCE>` is the
  MCPServer the tool came from (its tools share one credential) or else the tool's
  name, upper-cased with non-alphanumerics replaced by `_`.
- When the agent sends a request carrying a placeholder to the tool's host and port,
  the supervisor substitutes the real value. Sent anywhere else, the request is
  refused with `403 credential_endpoint_mismatch` and logged. Exfiltrating the
  placeholder yields nothing.
- `AGENTORCA_TOOL_CREDENTIALS` describes them (no secrets), so an agent can build its
  MCP clients generically:

  ```json
  [{"tool":"github","url":"https://api.githubcopilot.com/mcp/",
    "headers":{"Authorization":"Bearer ${AGENTORCA_GITHUB_TOKEN}"}}]
  ```

  Expand `${VAR}` from the environment and send the header as-is.

Each run gets its own provider (`agentorca-run-<hash>`) holding the secret values, read
from the tool's Secrets when the sandbox starts. The provider is deleted when the run ends.
Each credential source also gets an endpointless provider profile
(`agentorca-tool-<hash>`) that declares its env var names. Profiles hold no secrets and
are shared across runs. The operator's gateway identity therefore needs permission to
import profiles and to create and delete providers.

Narrowing applies here too: excluding a tool (`excludeTools`) or its MCP host
(`excludeHosts`) also withholds its credential. If you list a credentialed tool's
host:port in `sandbox.egress` as well, the bound endpoint takes over that host:port, since
OpenShell does not allow conflicting endpoints for one destination.

Not covered yet: OAuth MCP tools (their access token rotates; OpenShell's refresh
support is the planned route), MCPServers that authenticate the caller with the
router's ServiceAccount token (`allowedAgents`), secrets in another namespace, and pod
tools' `secretRefs`. These remain usable through the model-router as always. A sandbox
can attach at most 32 credentialed tools.

Use `tools.inherit: false`, `tools.excludeTools` or `tools.excludeHosts` to narrow this.
They only affect what the sandbox can reach; the router still offers the tool to the
LLM.

### Git access to GitHub

`sandbox.github` lets the agent clone, fetch and (with `push: true`) push GitHub
repositories over HTTPS, using the operator's GitHub App:

```yaml
    sandbox:
      github:
        repositories: [heddles/agent-orca]
        push: true
```

To give every sandbox of a class the same access, put it on the class instead, as a
`githubApp` [credential](#class-credentials) (`permissions: {contents: write}` for push).
The two forms are the same thing: an Agent's `sandbox.github` and its class's `githubApp`
credentials are unioned (all repositories, push if either pushes) and compile to one
endpoint. Everything below applies to both. The class must also let the sandbox reach
`github.com`, with the `github-read` preset or an egress rule.

- **Token:** when the sandbox starts, the operator mints an installation token for the
  App. It is narrowed to these repositories and to `contents: read`, or `write` with
  `push`, and lasts one hour. The operator replaces it 15 minutes before it expires;
  `status.sandboxGitTokenExpiry` shows the current expiry. A refreshed token reaches the
  running sandbox from the next git command on.
- **Placeholder:** the token goes into a per-run OpenShell provider. The workload's git is
  configured, through `GIT_CONFIG_*` environment variables, to answer github.com
  credential prompts with `x-access-token` and the placeholder in
  `AGENTORCA_GIT_TOKEN`. The supervisor decodes the Basic `Authorization` header and
  substitutes the real token.
- **Where the token works:** only on `github.com:443`, only for `/usr/bin/git` (and its
  helpers), and only for the listed repositories' `info/refs`, `git-upload-pack` and
  `git-receive-pack` (the last one only with `push`). Anything else in `sandbox.egress`
  for `github.com:443` is replaced by this endpoint.
- **Cleanup:** when the run ends, the provider is deleted with the run's other providers.
  The token itself expires within the hour.
- **Branches:** limit which branches the App may push with a repository ruleset. For
  example, block creating, updating and deleting branches outside `factory/**` for
  everyone except repository roles, which the App does not have.

To set it up:

1. **Create the App:**
   - repository permissions Contents: write and Metadata: read, with no events or
     webhooks;
   - install it on the repositories agents may use.
2. **Store the App's credentials:** create a Secret in the operator namespace with
   `app-id`, `installation-id` and `private-key` (the PEM), and set the chart value
   `github.app.secretName` to its name. The key is mounted into the operator only.
3. **Operator egress:** with operator egress isolation
   (`operator.networkPolicy.oidcEgress`), the allowed CIDRs must include
   `api.github.com`.

A run whose Agent sets `sandbox.github`, or whose class has a `githubApp` credential, fails
with a clear reason when the operator has no App configured, or when GitHub refuses the token, e.g. because the App isn't
installed on a listed repository.

### Admission rules

The Agent webhook rejects a sandboxed Agent unless:

- the namespace has `agentorca.io/enable-sandboxes=true`;
- `guardrailPolicyRef` is set;
- `networkIsolation.splitPod` is `true`;
- it sets neither `runtime.securityContextOverride` nor `runtime.secretRefs` (a sandbox
  cannot mount Secrets; give credentials to a Tool so the router holds them);
- `runtime.inputMode` is `env` (the sandbox accepts no inbound connections);
- `runtime.command` is set: OpenShell runs it as the sandbox's main process and ignores
  the image's `ENTRYPOINT` (an empty command creates an idle sandbox);
- `runtime.framework` is `openai-compatible` or `langgraph`. `autogen` and
  `semantic-kernel` need init containers and `shim` needs hostname rewrites; sandboxes
  support neither;
- `sandbox.github.repositories` are `owner/name` slugs under one owner (the App
  installation's account);
- `egress[].binaries` and `filesystem` paths are absolute, and `filesystem.readWrite`
  does not include `/`.

AgentDeployments do not support sandboxed Agents yet. Their pods would run without the
sandbox, so the controller marks such a deployment `Failed`. Run the agent as AgentRuns.

## How a run works

```
AgentRun ─► operator ─┬─ router Service + router pod + NetworkPolicies   (run namespace, unchanged)
                      └─ OpenShell gateway (Go SDK) ─► Sandbox CR ─► supervisor pod + workload pod
                                                                      (gateway's sandbox namespace)
workload ──► supervisor ──► router Service :8080/:8082      (always allowed)
                       └──► sandbox.egress hosts            (policy-checked)
```

1. `startRun` builds the router config, RBAC, token Secret, router Service and router pod
   exactly as in split-pod mode. Instead of the agent pod, it creates a sandbox through
   the gateway:
   - the Agent image is the workload, and `command` + `args` is the main process;
   - the environment is the agent pod's: `AGENTORC_*`, `OPENAI_BASE_URL`, `OPENAI_API_KEY`
     and framework variables. The gateway rejects line breaks in environment values, so a
     multi-line value (usually the task) is passed exactly as `<NAME>_B64` (base64), and
     `<NAME>` holds a one-line version. The Go SDK's `agent.Input()` reads the exact one;
   - the policy is compiled from `sandbox` and the Agent's tools (see `internal/openshell`);
   - static tool credentials are registered as providers and attached first (see
     [Authenticated tools](#authenticated-tools)).
2. Once the gateway reports the sandbox ID, the operator adds a NetworkPolicy
   `agentorca-router-<run>-sandbox`. It admits only that sandbox's supervisor pod
   (`openshell.ai/sandbox-id=<id>` in the sandbox namespace) to the router.
3. The run is `Running` while the sandbox provisions and runs. A main-process exit code
   of 0 makes it `Succeeded`; the output is read from the workload pod's `agent`
   container. A nonzero exit, a sandbox `Error` or a rejected policy counts as a pod
   failure: the operator deletes the sandbox and retries with a fresh one, up to
   `restartPolicy.maxRetries`.
4. When the run ends, the operator deletes the sandbox and the run's credential providers. The gateway keeps completed
   sandboxes until they are deleted, and they sit outside ownerReference GC. If the
   gateway is unreachable at that point, the operator records a `SandboxDeleteFailed`
   event. Each sandbox carries `agentorca.io/run` and `agentorca.io/run-namespace`
   labels, so leftovers can be found with `openshell sandbox list`.

`AgentRun.status.sandboxName` holds the gateway's name for the current sandbox.

## Who enforces what

| Layer | Enforced by | Scope |
|---|---|---|
| LLM egress, routing, quotas, guardrails | model-router | All LLM traffic, sandboxed or not |
| Router pod egress/ingress | Kubernetes NetworkPolicy (agent-orca) | Router pod |
| Workload → supervisor only | Kubernetes NetworkPolicy (OpenShell) | Sandbox workload pod |
| Per-host, per-binary, HTTP method/path egress | OpenShell supervisor | Sandbox workload |
| Filesystem | Landlock (OpenShell) | Sandbox workload |
| Syscalls and identity | seccomp, non-root (OpenShell) | Sandbox workload |
| `execve`/`ptrace`/`mount` kill | Tetragon (optional, agent-orca) | Pods in the run namespace, i.e. the router pod. The sandbox workload is not affected: running binaries is the point, and OpenShell confines them. |

## Limitations and follow-ups

These are deliberate scope limits of this first version, tracked against
`plans/openshell-first-class-integration.md`:

- **Router credential**: the per-run router token is passed as plain sandbox environment,
  so it is stored in the gateway's sandbox spec. It is audience-bound to the model-router
  and short-lived, and the policy only lets it reach the router. Moving it to an OpenShell
  provider so the workload sees only a placeholder is still to do (tool credentials
  already work this way).
- **No http input mode, no AgentDeployments, no warm sandboxes** (Phase 4).
- **Class credentials in workflow-owned sandboxes:** `githubApp` works there; `secretRef`
  does not yet. A rotated `secretRef` Secret reaches only new sandboxes.
- **No `SandboxPolicy` CRD yet.** Egress and filesystem extras are inline on the Agent. A
  shared, reusable policy CRD and TenantConfig defaults are a follow-up.
- **No OCSF decision stream into egress sinks/UI timeline yet** (Phase 3). Use
  `openshell logs <sandbox>` to see denied connections while tuning `audit` →
  `enforce`.
- **Not yet exercised on a live cluster.** The gateway interaction is covered by unit
  tests against the SDK's fake client. The Phase 0 cluster spike (CNI enforcement, cold
  start, Landlock on the node pool) still needs to run before production use.

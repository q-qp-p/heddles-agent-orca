# Claude Code on a Claude subscription

Run the real Claude Code CLI inside an agent-orca [sandbox](openshell-sandbox.md) and bill it
to a Claude Pro/Max subscription instead of API keys. The sandbox never holds the token: it is
a [class credential](openshell-sandbox.md#class-credentials).

> **Read this first.**
> - Anthropic's terms allow a subscription token only in Anthropic's own clients. Running the
>   real `claude` binary is the supported shape. Sending the token from your own code, or from
>   the model-router, is not, and risks the account. Check the current Consumer Terms.
> - A subscription belongs to one person. Use this for your own deployment. Do not run it as a
>   shared service for other users. Use API keys for that.
> - This bypasses the model-router: the CLI talks to Anthropic directly, so router budgets,
>   guardrails and cost tracking do not see these calls (see [What you can track](#what-you-can-track)).

Verified end to end on a local kind cluster with OpenShell, 2026-11-05.

## Prerequisites

The [OpenShell sandbox setup](openshell-sandbox.md#prerequisites), and a namespace labelled
`agentorca.io/enable-sandboxes: "true"`.

## 1. Build the sandbox image

[`examples/claude-code.Dockerfile`](examples/claude-code.Dockerfile) installs the CLI:

```shell
docker build -t claude-code-sandbox:dev -f docs/examples/claude-code.Dockerfile docs/examples
kind load docker-image claude-code-sandbox:dev --name agent-orca-dev   # kind only; otherwise push to a registry
```

## 2. Create a token and store it

On a machine where you are signed in to Claude Code:

```shell
claude setup-token     # prints a long-lived OAuth token
kubectl create secret generic claude-oauth-token -n agent-orca-system --from-literal=token='<token>'
```

Create the Secret in the namespace of the Agents that use it. A `secretRef` without a namespace
reads the workload's own namespace, so each tenant can hold its own subscription.

## 3. Apply the class, Agent and a run

```shell
kubectl apply -f config/samples/sandboxclass-claude-code.yaml
kubectl get agentrun claude-code-demo -n agent-orca-system -w
```

[`sandboxclass-claude-code.yaml`](../config/samples/sandboxclass-claude-code.yaml) has the class,
an Agent and an AgentRun. The pieces that matter:

- `egress` allows only `api.anthropic.com`. The credential binds that host, so only the named
  binary can use it, and the other processes in the sandbox lose access to the host.
- The Agent's command sets `CLAUDE_CODE_OAUTH_TOKEN="$AGENTORCA_CRED_CLAUDE_OAUTH"`. That
  variable holds a placeholder like `openshell:resolve:env:…`. The CLI sends it as its bearer
  token and the supervisor replaces it on the wire.
- `< /dev/null` stops the CLI waiting 3 seconds for stdin.
- `--output-format json` makes the result and usage machine-readable.

## 4. Read the result

The run's `status.output` holds the CLI's stdout. `result` is the answer:

```shell
kubectl get agentrun claude-code-demo -n agent-orca-system -o jsonpath='{.status.output}' | jq '{result, usage, total_cost_usd, num_turns}'
```

## What you can track

**Network (works today).** The run trace has an `egress` event for each connection and request,
with the binary that made it. Filter the **Network** tab in the UI, or query the archive:

```shell
kubectl exec -i -n agent-orca-system postgres-1 -c postgres -- psql -U postgres -d agentorca -At -c \
 "select e->'event'->>'action', e->'event'->>'binary', e->'event'->>'destination', e->'event'->>'method', e->'event'->>'url'
  from archived_runs, jsonb_array_elements(trace_events) e
  where name='claude-code-demo' and e->'event'->>'type'='egress'"
```

Connection events carry the `binary`; per-request `http` events do not. The model call is
`POST https://api.anthropic.com/v1/messages`.

**Usage.** Take token counts from the CLI's JSON: `usage` and `num_turns`. Note
`total_cost_usd` is the **list-price equivalent**, not what the subscription is billed.
`status.spendUSD` stays empty, because the router never saw the calls.

**Not yet tracked.** The sandbox's process events (what the CLI spawned) are parsed away
(`internal/openshell/ocsf.go` keeps only `NET` and `HTTP` lines).

## Troubleshooting

| Symptom | Cause |
|---|---|
| `blocked api.anthropic.com:443 by …/claude.exe — transparent_tcp_policy_denied` | `bind.binaries` names the wrong executable. The npm package runs `…/node_modules/@anthropic-ai/claude-code/bin/claude.exe`, not `node` or the `/usr/local/bin/claude` symlink. Read the path from the denial message or the `egress` trace, fix the class, and start a **new** run: credentials resolve when the sandbox is created. |
| `denied … http-intake.logs.us5.datadoghq.com` | The CLI's telemetry. Harmless; the class does not allow it. |
| `Warning: no stdin data received in 3s` | Redirect stdin: `< /dev/null`. |
| Run fails with the class unusable | The Secret is missing in the run's namespace, or has no `token` key. |

## Codex

Not covered. A ChatGPT login is an `auth.json` holding a refresh token, which the CLI parses and
refreshes itself. Header substitution cannot carry that, so it needs a file-type credential, which
does not exist yet. Codex with an API key works with a `header` credential bound to
`api.openai.com`.

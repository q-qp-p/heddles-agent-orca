# Switchyard routing

`strategy: switchyard` lets an [NVIDIA NeMo Switchyard](https://github.com/NVIDIA-NeMo/Switchyard)
routing algorithm choose which provider answers each LLM call. Where the rule router matches keywords and
the meta-router asks a model, Switchyard reads the conversation itself: how an agent's tool calls are going
decides whether the next call needs the capable model or the cheap one.

The model-router runs the algorithm in-process (Switchyard's Rust core as a WebAssembly guest), so there is
no extra service to deploy. Algorithms that need a judge call a model you name, through the same providers
the selector already uses.

## A minimal selector

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: ModelSelector
metadata:
  name: adaptive-coding
spec:
  strategy: switchyard
  providers:
    - {name: claude-haiku, weight: 100}
    - {name: claude-opus, weight: 100}
  capabilityRouting:        # which provider plays which tier
    capable: claude-opus
    efficient: claude-haiku
  switchyard:               # the algorithm; targets are ModelProvider names
    type: stage_router
    picker: efficient_first
    capable_target: claude-opus
    efficient_target: claude-haiku
    confidence_threshold: 0.5
```

Complete samples: [`config/samples/switchyard-stage-router.yaml`](../config/samples/switchyard-stage-router.yaml) and
[`switchyard-llm-judge.yaml`](../config/samples/switchyard-llm-judge.yaml).

## Roles

Switchyard thinks in tiers, not provider names, so the selector says who plays each one in
`spec.capabilityRouting`. These keys are roles, not keyword tags: the keyword rule never matches them
against message text.

| Key | Meaning |
|---|---|
| `capable` | The provider an algorithm treats as its strong tier |
| `efficient` | Its cheap tier |
| `judge` | The provider that runs an LLM judge |
| `subagent.<group>` | The same, for delegated sub-agent work |

Every provider with a weight may answer; a provider with `weight: 0` is never chosen but can still be a
judge or a fallback. Each value must be one of the selector's providers.

## Algorithms

| `type` | What it does | Roles it needs |
|---|---|---|
| `stage_router` | Scores the agent's tool results (errors, repeated failures, steady production) and escalates to the capable tier only when they say so | `capable`, `efficient` |
| `composite` | An LLM judge scores the task first; the stage router carries on from there | `capable`, `efficient`, `judge` |
| `auto` | A `stage_router` with sensible defaults | `capable`, `efficient` |
| `plan_execute` | The capable model plans; the first tool that changes something hands over to the efficient one | `capable`, `efficient` |
| `llm_classifier` | A judge decides per task or per session (capability, escalation or custom mode) | Switchyard decides |
| `random`, `passthrough` | A weighted random choice, or a fixed target | none |
| `subagents` | A separate policy for delegated work | none |

The schema of each is Switchyard's own: see its
[routing algorithm documentation](https://github.com/NVIDIA-NeMo/Switchyard/tree/main/docs/routing_algorithms).

**Not served:** `advisor`, `vgr` and `noop` answer on the model's behalf, which the router cannot relay yet,
and `prefill_router` needs a PyTorch classifier checkpoint. A typed decision service
(`classifier.decision`) is not served either. A selector using any of these reports why in its
`SwitchyardValid` condition.

### Tool classes help the stage router

If your tools declare a side-effect class (`Tool.spec.sideEffect`), the router tells the stage router:
`read` tools count as investigation, `write` and `actuate` tools as production. Names you list in the
algorithm's own `tool_semantics` keep your category.

## Checking a selector

```sh
kubectl get modelselector adaptive-coding
kubectl get modelselector adaptive-coding -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

| Condition | Meaning |
|---|---|
| `ProvidersResolved` | Every provider the spec names exists in the namespace, and `capabilityRouting` and `metaRouter` name providers of this selector |
| `SwitchyardValid` | Switchyard accepts the algorithm, it is one the router serves, and the roles it needs are set (reasons `Valid`, `UnsupportedAlgorithm`, `InvalidAlgorithm`, `MissingRole`) |
| `Ready` | Both hold |

`status.activeProviders` lists the selector's providers that exist and are Ready.

**A selector that is not Ready does not stop runs.** Runs read the spec directly, and whenever Switchyard
cannot decide the rule router does. The cause is in the decision's reason and in the trace.

## When Switchyard cannot decide

The model-router never fails a request because of Switchyard. If the engine will not start, the algorithm
errors, a judge fails or times out, or something unsupported is asked of it, the rule router chooses. Those
decisions have path `switchyard-failopen` in the run's routing decisions, in `routing_events`, and in the
`agentorca_modelrouter_routing_decisions_total` metric, so a selector that quietly always falls back is
easy to find.

Set `MODEL_ROUTER_DISABLE_SWITCHYARD=true` on the model-router container to route with the rule router alone.

## What it costs

- **Time:** about 1 ms per decision at 10 messages, 8 ms at 200, 20 ms at 500, against an LLM call of seconds.
- **A judge** is an LLM call before each decision it takes part in, charged to the run (see
  [cost tracking](cost-tracking.md)).
- **Memory:** the engine adds about 110 MB at start. A pod whose selector uses `switchyard` gets a
  model-router sidecar limit of 512Mi instead of 256Mi.
- **State:** Switchyard remembers per session (for example that it escalated) in the sidecar's memory.
  A new conversation on a warm pod starts clean; a restarted pod forgets.

See also [development notes](development.md#how-the-sidecar-uses-switchyard) and the
[observability reference](observability.md#routing-telemetry).

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

## Why a model was chosen

Every routing decision carries one plain sentence that says what was chosen and why, for example:

> Escalated to sonnet-5-5: tool calls have been failing repeatedly (stage tool-exec; stage_router override)

The sentence is in the router's `routing request` log line (next to `algorithm`, `label`, `stage` and
`path`), in the trace event, in `AgentRun.status.routingDecisions[].reason`, and on the "routed" row of
the run view. That row has a "why this model" disclosure with the algorithm, label, stage, path, the
providers that were candidates and the decision ID.

| `stage_router` label | Reads as |
|---|---|
| `fall_open` | Chose the efficient model: nothing so far suggests the call needs more |
| `override` | Escalated to the capable model: tool calls have been failing repeatedly |
| `capable_hold` | Stayed on the capable model: the run was escalated earlier and has not recovered |

Rule-based and meta-router decisions, Switchyard fail-open and fallback have sentences of their own;
a fallback names the provider that failed.

## Rating routing

A person can rate how the models were chosen, on a whole run or on one routing decision:

- In the run view, the thumbs next to a "routed" row rate that decision, and the thumbs under the
  output rate the run. A thumbs down can carry a short comment.
- Through the task API: `POST /v1/tasks/{id}/feedback` with `{"rating": 1}` or `{"rating": -1}`, an
  optional `comment` (up to 2 KiB), an optional `decisionId` and an optional `turn`.

Rating the same target again replaces the earlier rating. Ratings are stored in PostgreSQL
(`routing_feedback`); without it, rating is refused with a 503 instead of being dropped.

## Benchmarking routing

A `RoutingBenchmark` measures whether routing's choices are worth their cost for a set of Agents. It replays a small sample of their calls on another model, has a judge model compare the two answers, and records the verdict. Nothing the user sees changes: the replay runs after the served answer is complete, never runs a tool, and is paid from the benchmark's own budget, not the run's.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: RoutingBenchmark
spec:
  agentSelector: {matchLabels: {team: platform}}
  shadow: {enabled: true, sampleRate: "0.05"}
  judge: {providerRef: judge}
  budget: {perDayUSD: "2.00"}
```

**Routing is judged per Agent.** Every AgentRun, AgentDeployment and workflow step names an Agent, and an Agent names the ModelSelector that routes it, so the Agent is what the evidence belongs to. A one-off run adds to the same pool as the runs and deployments of its Agent before and after it. Pick Agents with `agentRefs`, an `agentSelector`, or both; an empty selector (`{}`) means every Agent in the namespace. The result of a sample records which Agent it came from, whether it ran as a run or a deployment, and the generation of the ModelSelector that routed it, so evidence from before and after a change to the selector can be told apart.

See `config/samples/routing-benchmark.yaml`. The controller reports `Ready` (false when no Agent matches, a named Agent or a selector is missing, or a provider is missing), plus two warnings:

- `PostgresUnavailable`: the operator has no Postgres, so results cannot be stored.
- `JudgeIsArm`: the judge is also one of the models compared, which lets it favour its own answers. This is a warning, not an error.

`status` carries the Agents covered, the judged pairs, how often the judge disagreed with itself when the two answers were swapped, today's spend, and a verdict count per task category of each Agent.

**Budget.** `budget.perDayUSD` is required. Before each replay the router asks the operator for a lease on the budget, counting settled spend and leases not yet settled, so several runs cannot overspend together. A denied lease, or an operator without Postgres, means no replay. An unsettled lease expires after ten minutes. The operator also checks that the benchmark covers the Agent of the pod that asks, so a pod can spend only the budget of a benchmark that measures its own Agent.

**Limits that follow the Agent.** `shadow.maxPerRun` (default 3) caps the calls sampled from one AgentRun. A deployment keeps running, so it has no per-run cap; `shadow.maxPerHour` (default 20) caps the samples each Agent gets per hour across all its runs and deployments, so one busy Agent cannot use the day's budget. `budget.perRunUSD` applies to a deployment per UTC day. `shadow.excludeRunSelector` leaves out AgentRuns by label, for example experiments; to leave an Agent out, leave it out of the selector.

**Privacy.** Only the verdict, its scores and the costs are stored. The prompt and both answers are kept only when `storeTranscripts: true`.

**How a replay works.** After a sampled call has been answered in full, the router, in the background and one at a time:

1. asks the operator for a budget lease;
2. sends the same request, tools included, to the other model, without streaming. If that model asks for a tool the call is counted and shown to the judge, never run;
3. asks the judge twice, with the two answers in opposite order. If the two verdicts disagree about which answer won, the result is a tie marked inconsistent. A reply that is not the expected JSON is recorded as `invalid`;
4. files the verdict and the costs, which releases the lease.

Which call is sampled is decided from its decision ID, so the same call is always in or out. `shadow.arms: counterfactual` replays on the other tier: the `capable` provider when `efficient` served, and the reverse; without those roles, the cheapest other provider (or the dearest, when the served one is already the cheapest). `arms: providers` replays on each listed provider.

Both AgentRuns and AgentDeployments are sampled. The judge's credentials are mounted on the pod even when the judge is not one of the selector's providers. A deployment's pods restart when its benchmark changes, as they do when its ModelSelector changes.

The recommendations built from these results come later.

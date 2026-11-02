# Changelog

All notable changes to agent-orca are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Routing decision identity and a `Selector` seam** — every routing decision now carries a
  decision ID (UUIDv7), the rule or meta-router path that made it, and the task stage
  (`tool-exec`, `code`, `summarize`, `chat`). They appear in the `routing` trace event and in
  `status.routingDecisions` (`decisionID`, `path`, `category`, `parentDecisionID`). A fallback
  after a provider failure is recorded as its own decision pointing at the one it replaced.
  Provider selection moved behind a `Selector` interface in `internal/router`; the existing
  rule and meta-router logic is its first implementation. `internal/router/testdata/routing`
  holds golden routing fixtures (`go test ./internal/router -run TestRoutingGolden -update`).
- **Routing telemetry** — with PostgreSQL configured, every LLM call becomes a
  `routing_events` row (provider, deciding path, task stage, tokens, cost, latency, time to
  first token, fallback/error, tool-call outcomes; metadata only), and a run's outcome is
  recorded in `run_outcomes` when it is archived. Rows are kept for
  `--routing-events-retention` (default 720h). The run page can load the full decision
  history (`GET /api/runs/{ns}/{name}/routing-events`) when `status.routingDecisions`, now
  capped at the latest 100, is only a window. New router metrics: `llm_tokens_total`,
  `llm_latency_seconds`, `llm_ttft_seconds`, `llm_errors_total`, `routing_decisions_total`,
  `routing_fallbacks_total`, `telemetry_events_dropped_total`; `tool_calls_total` and
  `stream_duration_seconds`, defined but never recorded before, now are. See
  `docs/observability.md`.
- **Routing e2e harness** — `test/fakellm`, a deterministic OpenAI-compatible server for tests
  (scripted failures, latency, tool calls, judge verdicts), and a path-filtered
  `E2E Routing Tests` workflow that runs the routing telemetry specs (`ROUTING_E2E=true`,
  `E2E_LABEL` selects specs) against a Kind cluster with PostgreSQL.
- **Hindsight API-key authentication** (`hindsight.apiKeySecret` chart value, fixes the
  application-level half of #58) — the model-router sends
  `Authorization: Bearer <key>` on hindsight bank/recall/retain calls so hindsight can
  run with its `ApiKeyTenantExtension` instead of relying on ingress auth. The operator
  mirrors the key into each run namespace and injects it into the model-router
  container only (never the agent container, never the router ConfigMap). Rollout and
  rotation runbook: `docs/hindsight-auth.md`.
- **In-flight spend in `/api/costs`** (#79) — the operator reads the model-router's
  `agentorca_modelrouter_spend_usd_total` for unfinished runs, so `inFlightUSD` and the
  totals include spend that is not yet stamped on `status.spendUSD`. Runs whose router
  cannot be read are counted in `inFlightUnscraped` rather than treated as $0. Per-run
  NetworkPolicies now admit the operator's pods on `:9091` (scoped by namespace and pod
  labels, port 9091 only); `hack/verify-router-metrics.sh` checks the path. See
  `docs/observability.md`.
- **Cost dashboard rewrite** (#78) — KPI row (total, in-flight, average per run, failed-run
  spend highlighted above 20% of total, untracked-runs chip), daily spend stacked by run
  outcome using the shared phase colors with an icon + color legend and a screen-reader
  description, a By Agent / By Model / By Outcome tab bar, and a banner for runs whose
  cost could not be captured (with "View runs" and per-run links). Built from the design
  system primitives; no raw hex, emoji or hand-rolled buttons. `getCosts` now coerces
  partial or null payloads so an older operator cannot crash the page.
- **Cost API: model, day, outcome and untracked breakdowns** (`GET /api/costs`, #77) —
  `byModel` and `byDay` are now populated (they were always empty), archived runs from
  PostgreSQL are included once their CRDs are garbage-collected (live copy wins, no
  double counting), and the response adds `byOutcome` (spend by run phase, also split per
  day), `untrackedCount` / `untrackedSample` (finished runs flagged `CostCapture=Incomplete`,
  reported instead of counted as $0), `inFlightUSD`, `avgRunUSD` and `failedRunUSD`. Optional
  `days=N` limits the window. `byModel` attributes a run's whole spend to its last routing
  decision. The archive gains a `cost_capture` column (added by the startup migration).
- **`CostCapture` condition on AgentRun** (#76) — `Status.SpendUSD: ""` used to mean both
  "this run cost nothing" and "its spend could not be read". The controller now sets
  `CostCapture=True` (reason `Complete`) when the spend was read from the state store and
  `CostCapture=False` (reason `Incomplete`, with the cause) when it could not be, e.g.
  the store was unreachable or none is configured (dev profile), so cost views can show
  such runs as untracked instead of $0.
- **Per-run spend metric with call outcomes** (`agentorca_modelrouter_spend_usd_total{namespace,run,provider,model,outcome}`,
  #75) — the model-router now exports the USD it spends split by `success`,
  `provider_error`, `cancelled` and `stream_interrupted` (all four series from the first
  scrape), so cost that never reaches a success path is visible on `:9091/metrics`.
- **Native local filesystem access via the ACP bridge (`fs/*` relay)** — "pod drives
  the workstation": agent pods read and edit the user's real local workspace through
  the same ACP v1 channel editors like Zed already use, with no workspace copies,
  sync, or volume mounts (`internal/acpfs` relay, model-router built-ins
  `fs_read_text_file` / `fs_write_text_file` / `fs_list_dir`, bridge relay in
  `cmd/aoctl`; fixes #34). Security model: `Agent.spec.filesystem` tool-injection
  policy (`readOnly` | `readWrite` + `writeApproval`), workspace-root confinement
  with lexical `..` escape rejection at the bridge, and per-write ACP elicitation
  approval. Docs: `docs/filesystem-access.md`, `docs/acp-api.md`,
  `docs/aoctl-reference.md`; e2e suite gated by `FS_E2E=true`.
- **Stream model-router trace events to ACP editors mid-turn** — `toolCall`,
  `toolResult`, `guardrail`, `providerFallback` and `mcpDiscovery` events are
  forwarded as SSE `event: trace` frames by the ACP API and rendered by the bridge
  as tool-call updates and thought notes; tool-call pairs use session-scoped
  sequenced ids with pending-result pairing, and content blocks are wrapped in the
  ACP v1 `{"type":"content",…}` discriminated shape so strict clients render them.
- **Routing decisions and hindsight memory operations as trace events** — the
  model-router now emits which model a request routed to (including provider
  fallbacks) and hindsight bank/recall/retain operations (including failures) into
  the run's trace stream; the bridge renders them like the web UI's
  model-selected/notes rows, and the web UI gained the matching `TraceEvent` union
  members and archived-transcript rendering with live-view dedup against the
  CRD-backed `modelSelected`.
- **Extended-thinking streamed to the editor** — reasoning deltas now stream as
  `agent_thought_chunk` updates on the ACP run stream (previously only reached ACP
  clients as per-delta trace events the bridge dropped); replay dedup reuses the
  reasoning byte-swallow counters so thinking text is neither duplicated nor lost.
- **Write-approval ergonomics** — `aoctl acp serve --fs-remember-approval` stores
  the first approved write on the session record and skips subsequent write
  elicitations for that session (on by default — opt out with
  `--fs-remember-approval=false` or force per-write prompts with
  `--fs-require-approval`); the approval-mode matrix (approve-per-write / remember-session / unattended) is
  documented in `docs/filesystem-access.md` and `docs/aoctl-reference.md`.
- **fs writes render as before/after diffs in the editor** — the bridge snapshots
  the file being replaced (editor `fs/read_text_file` first so unsaved buffers
  win, disk as fallback) and attaches an ACP `{"type":"diff"}` ToolCallContent to
  the completed `fs_write_text_file` tool call with `kind: "edit"`; created files
  render as a full add. The wire write stays a full-file `fs/write_text_file`;
  payloads over 100 KiB, no-op writes, and editor-refused writes fall back to the
  plain tool result (#40).
- **`Agent.spec.filesystem.rememberApproval`** — agent policy for the
  approve-once vs approve-every write modes (default `true`; `false` makes every
  write prompt even when the workstation runs `--fs-remember-approval`). Agent
  policy and workstation flags narrow each other: prompts fire when either side
  asks, approvals are remembered only when both allow it, and unattended writes
  still need `--fs-require-approval=false` on the workstation. Projected into the
  ACP agent manifest (now documented in the ACP OpenAPI spec); CRDs regenerated
  (#41).
- **Pre-send context compaction is visible** — a `truncation` trace event
  (`before`/`after`/`budget`) is emitted whenever the router compacts a request
  before sending; the bridge renders it as a "context compaction" note and the
  web UI trace as a "context compacted" row (#42).
- Operator API surface for the fs relay: internal endpoints
  `POST /agentrun/{ns}/{run}/fs` and `GET /agentrun/{ns}/{run}/fs/{id}`
  (SA-token authenticated, method whitelist) and ACP-API endpoints
  `GET /runs/{id}/fs/pending` and `POST /runs/{id}/fs/pending/{id}/response`
  (tenant-ownership enforced; 501 when no relay is wired).
- `Agent.spec.filesystem` CRD field (deepcopy, `config/crd/bases`,
  `charts/agent-orca/crds`, embedded + generated OpenAPI, and the
  `agent-orca-resources` subchart rendering updated); sample manifest
  `testdata/agents/filesystem.yaml`; demo-programming-agent enables `readWrite`
  with write approval.
- ACP live-run streams are held open for a full 10-minute window (matched to the
  bridge's client timeout) instead of being sliced by the 30-second ACP request
  timeout; every other ACP route keeps the bounded timeout so auth/fs-poll/resume
  paths still fail fast under a stuck handler.
- Bridge `serve` logs its version at startup, and the version skew guard gives a
  clear "rebuild and redeploy the model-router image" error when the operator
  injects a built-in tool an older router build does not implement.
- Test hermeticity: `aoctl` test helpers scrub ambient `AOCTL_*` environment
  variables so endpoint-resolution tests pass in shells pointing
  `AOCTL_ENDPOINT` at a remote cluster.

### Changed

- Terminal `done` trace event is now emitted by the model-router whenever the
  OpenAI turn loop ends without dispatching tools (previously only the `_done`
  built-in emitted it, so framework agents never produced a terminal marker);
  `done` counts as terminal everywhere (router `TailTokens`, apiserver
  classification) so live streams close immediately instead of retrying windows
  until the deadline. Auto-clarify (paused) turns are excluded by design.
- The ACP bridge polls the run CRD instead of re-streaming when a terminal event
  was signalled but the CRD has not caught up, and each stream-reconnect attempt
  swallows the already-emitted token/reasoning prefix, so replays no longer
  duplicate output.
- `elicitation/create` calls (fs write-approval and `_clarify`) are ACP v1
  spec-compliant: required `sessionId` session scope and an
  `ElicitationSchema`-shaped `requestedSchema` envelope — forms now surface on
  strict clients (previously rejected with `-32602 Invalid params`).

### Fixed

- **Routing rules that never fired** — the router passed a zero token estimate and no
  timeout to the rule router, so the `maxRequestTokens` filter, the large-context rule and
  the near-timeout rule were dead. They now run. Runs with large conversations, a provider
  `maxRequestTokens`, or a `spec.timeout` can be routed to a different provider than before.
- **Weight-0 providers could win the budget, timeout and context rules** — providers reserved
  for the fallback chain and meta-router escalation are now skipped by those rules (and by
  vision routing, unless no weighted provider has the capability).
- **Capability keyword routing was order-dependent** — keys are matched in sorted order, and
  the role/stage keys `capable`, `efficient`, `judge`, `any`, `stage:*` and `subagent.*` are
  reserved and never matched against message text.
- **Meta-router prompts to Anthropic providers** — they were sent in OpenAI format to the
  Messages endpoint and always failed. The meta-router now uses the same provider
  forwarders as normal calls, with a 15s timeout.
- **Meta-router saw its own history twice** — the conversation was prepended a second time
  before routing, skewing keyword matching and the decision cache key. The run's budget,
  timeout and turn state now also reach the meta-router cache key as designed.
- **Duplicate and lost routing decisions** — each LLM call used to post a second, re-rolled
  decision (it could disagree with the real one) and concurrent posts overwrote each other.
  A call now posts one decision; posts use an optimistic lock, and `status.routingDecisions`
  keeps the most recent 100 (`status.routingDecisionsTotal` counts all of them).

- ACP sessions showed a truncated or empty final response when an agent ended its turn
  with `_done(output=...)` (#74): the answer rides only the terminal `done` trace
  event, which `streamACPRun` left out of `run.completed` and the `aoctl acp` bridge
  discarded. The bridge now emits it as a suffix-aware message chunk (no duplication on
  reconnect replay), `run.completed` carries it, and `Status.Output` truncation (10 KiB,
  kept for etcd) is now rune-safe and marked with `[output truncated]`.
- AgentRun spend was dropped on several terminal paths (#76): a restart reset the run to
  `Pending` without writing the partial spend (lost entirely without a state store), runs
  the router ended through `_fail`/`_done` never had their spend loaded, and any state
  store error silently became `""`. Spend is now stamped on every retry and on every
  terminal run (with a bounded retry while the store is unreachable); a failed read keeps
  the earlier value as a lower bound and flags `CostCapture=Incomplete`.
- Router spend lost on failure paths (#75): a run cancelled mid-call returned before its
  spend was recorded, a stream cut off before the provider's usage chunk recorded $0
  (now approximated from the request and the received output, or taken from usage the
  provider already sent), and non-streaming Gemini-format calls were never counted
  toward run spend or the budget cap at all. `_fail` now flushes spend before notifying
  the operator.
- `fs_list_dir` symlink escape: the bridge answers directory listings from
  disk itself, and lexical confinement let a workspace symlink (final or
  intermediate component) enumerate host directories outside the roots. Paths
  the bridge reads itself (listing, the write-diff disk fallback) are now
  symlink-resolved and re-checked against the real roots.

- Pre-send context truncation never fired: the router's token estimate was
  hardcoded to `0`, so over-window requests were forwarded verbatim, the provider
  answered `400 context length exceeded`, and the identical oversized request was
  replayed against every fallback provider. The estimate is now real and the
  budget reserves the provider's `maxOutputTokens` (capped at half the window)
  instead of a flat 1024-token reserve (#42).
- Fallback providers were sent the request fitted to the PRIMARY's window;
  each provider is now fitted to its own window from the original request, a
  request no provider can hold fails fast with 413 (502 when the primary fit
  but failed and only the fallbacks were too small), and one `truncation`
  note is emitted per turn for the request actually used (#42).
- A newest message larger than the whole context budget was dropped and
  replaced by a compaction summary, so the model never saw the user's input.
  It is now kept with its content truncated (marked `[... truncated …]`) and
  older history compacted; truncation no longer splits multi-byte UTF-8
  characters (#42).

- Write-approval diagnostics: a failed elicitation round-trip (editor without
  elicitation support, transport error, timeout) is reported as a distinct
  "write approval prompt failed" error instead of collapsing into "declined by
  the user in the editor"; a failed prompt never proceeds unapproved, and an
  accepted form approves regardless of the submitted form shape.
- `fs_write_text_file` results: Zed answers `{}` (no payload per the
  `WriteTextFileResponse` schema), which previously tripped the empty-result
  annotation into a spurious "returned no results" error even though the write
  landed — empty/null write results are now confirmed as `{"written": true}`.
- Tool-call updates rendered in strict ACP clients: content entries are wrapped in
  the discriminated `{"type":"content",…}` envelope (previously bare content
  blocks were silently dropped, e.g. by Zed), and per-session sequenced
  `toolCallId`s prevent two calls of the same tool in one run from collapsing
  onto a single cycling editor entry.
- Duplicate output on ACP stream replay: 30-second slice boundaries caused
  reconnect + full token-history replay per attempt (final output seen up to 30
  times); fixed via CRD polling + replay-prefix swallowing.
- Runs finalize immediately: the router's missing terminal `done` event kept
  editors and SSE consumers showing "running" long after the final response.
- Removed a debug leftover that echoed raw ACP-API SSE events as malformed
  session/update notifications (no `sessionId`, not a spec variant), which
  spammed client logs with deserialization warnings on every run.
- README zshrc-path typo (fixed by the ACP agent's first successful local write).
-- 0003: routing telemetry. One row per LLM call the model-router makes: which
-- provider the selector chose and why, what the call cost and how long it took,
-- and how its tool calls went. Metadata only: prompts and responses are never
-- stored here. run_outcomes is the per-run result, joined to the events by run
-- when a run is archived; it is what lets later stages ask whether a routing
-- choice was worth its cost.
CREATE TABLE routing_events (
    decision_id        TEXT PRIMARY KEY,
    parent_decision_id TEXT,
    tenant             TEXT,
    namespace          TEXT NOT NULL,
    run_name           TEXT NOT NULL,
    agent              TEXT,
    selector           TEXT,
    strategy           TEXT NOT NULL DEFAULT '',
    algorithm          TEXT NOT NULL DEFAULT '',
    -- path is what decided: rule:*, meta, meta-cache, fallback, switchyard.
    path               TEXT NOT NULL DEFAULT '',
    stage              TEXT NOT NULL DEFAULT '',
    stage_conf         DOUBLE PRECISION NOT NULL DEFAULT 0,
    category_version   INTEGER NOT NULL DEFAULT 0,
    sy_label           TEXT NOT NULL DEFAULT '',
    workflow_step      TEXT NOT NULL DEFAULT '',
    provider           TEXT NOT NULL,
    model              TEXT NOT NULL DEFAULT '',
    candidates         JSONB NOT NULL DEFAULT '[]',
    signals            JSONB,
    confidence         DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- is_shadow marks a counterfactual call made for benchmarking, not served.
    is_shadow          BOOLEAN NOT NULL DEFAULT FALSE,
    prompt_tokens      INTEGER NOT NULL DEFAULT 0,
    completion_tokens  INTEGER NOT NULL DEFAULT 0,
    cost_usd           NUMERIC(14, 8) NOT NULL DEFAULT 0,
    -- aux_cost_usd is what the selector itself spent deciding (a meta-router call).
    aux_cost_usd       NUMERIC(14, 8) NOT NULL DEFAULT 0,
    latency_ms         INTEGER NOT NULL DEFAULT 0,
    ttft_ms            INTEGER,
    finish_reason      TEXT NOT NULL DEFAULT '',
    fallback_from      TEXT NOT NULL DEFAULT '',
    error_class        TEXT NOT NULL DEFAULT '',
    tool_calls         INTEGER NOT NULL DEFAULT 0,
    tool_errors        INTEGER NOT NULL DEFAULT 0,
    tool_parse_errors  INTEGER NOT NULL DEFAULT 0,
    guardrail_blocked  BOOLEAN NOT NULL DEFAULT FALSE,
    safeguard_trip     TEXT NOT NULL DEFAULT '',
    turn_index         INTEGER NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_routing_events_selector ON routing_events(namespace, selector, created_at DESC);
CREATE INDEX idx_routing_events_run      ON routing_events(namespace, run_name, created_at);
CREATE INDEX idx_routing_events_created  ON routing_events(created_at);

CREATE TABLE run_outcomes (
    namespace                TEXT NOT NULL,
    run_name                 TEXT NOT NULL,
    tenant                   TEXT,
    selector                 TEXT,
    phase                    TEXT NOT NULL,
    -- fail_reason_class groups FailureReason into a small set (budget, safeguard,
    -- timeout, error) so outcomes can be compared across runs.
    fail_reason_class        TEXT NOT NULL DEFAULT '',
    -- turns is the number of LLM calls recorded for the run.
    turns                    INTEGER NOT NULL DEFAULT 0,
    total_cost_usd           NUMERIC(14, 8) NOT NULL DEFAULT 0,
    tool_approval_rejections INTEGER NOT NULL DEFAULT 0,
    budget_exceeded          BOOLEAN NOT NULL DEFAULT FALSE,
    restart_count            INTEGER NOT NULL DEFAULT 0,
    archived_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (namespace, run_name)
);

CREATE INDEX idx_run_outcomes_selector ON run_outcomes(namespace, selector, archived_at DESC);

-- 0005: shadow benchmarking. A shadow result is one counterfactual call: a
-- sampled call replayed on another model, then judged against the answer that
-- was served. Nothing here is ever shown to the user or acted on by a run.
-- Prompts and answers are stored only when the benchmark sets storeTranscripts.
-- Evidence belongs to an Agent: a run, a deployment or a workflow step only
-- says where a sample came from, so results build up across them.
CREATE TABLE shadow_results (
    id                 BIGSERIAL PRIMARY KEY,
    -- decision_id is the served call; one row per (decision, arm provider).
    decision_id        TEXT NOT NULL,
    tenant             TEXT,
    namespace          TEXT NOT NULL,
    -- agent is the Agent CRD whose call was sampled; the operator derives it
    -- from the source, never from the router.
    agent              TEXT NOT NULL,
    -- source_kind is AgentRun or AgentDeployment; source_name is its name.
    source_kind        TEXT NOT NULL CHECK (source_kind IN ('AgentRun', 'AgentDeployment')),
    source_name        TEXT NOT NULL,
    benchmark          TEXT NOT NULL,
    selector           TEXT NOT NULL DEFAULT '',
    -- selector_generation is the ModelSelector generation that routed the call,
    -- so evidence from before and after a change can be told apart.
    selector_generation BIGINT NOT NULL DEFAULT 0,
    -- Category of the served call, copied so results group without a join.
    stage              TEXT NOT NULL DEFAULT '',
    sy_label           TEXT NOT NULL DEFAULT '',
    workflow_step      TEXT NOT NULL DEFAULT '',
    served_provider    TEXT NOT NULL,
    shadow_provider    TEXT NOT NULL,
    -- winner is the judge's verdict on the shadow against the served answer:
    -- served, shadow, tie or invalid (the judge returned unusable output).
    winner             TEXT NOT NULL CHECK (winner IN ('served', 'shadow', 'tie', 'invalid')),
    -- inconsistent is true when the two position-swapped judgments disagreed;
    -- the verdict is then a tie.
    inconsistent       BOOLEAN NOT NULL DEFAULT FALSE,
    judge_confidence   DOUBLE PRECISION NOT NULL DEFAULT 0,
    judge_provider     TEXT NOT NULL DEFAULT '',
    dimensions         JSONB,
    judge_reason       TEXT NOT NULL DEFAULT '',
    shadow_prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    shadow_completion_tokens INTEGER NOT NULL DEFAULT 0,
    shadow_cost_usd    NUMERIC(14, 8) NOT NULL DEFAULT 0,
    judge_cost_usd     NUMERIC(14, 8) NOT NULL DEFAULT 0,
    shadow_latency_ms  INTEGER NOT NULL DEFAULT 0,
    shadow_tool_calls  INTEGER NOT NULL DEFAULT 0,
    shadow_error       TEXT NOT NULL DEFAULT '',
    -- transcript holds the prompt and both answers; null unless storeTranscripts.
    transcript         JSONB,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (decision_id, shadow_provider)
);

CREATE INDEX idx_shadow_results_benchmark ON shadow_results(namespace, benchmark, created_at DESC);
CREATE INDEX idx_shadow_results_agent     ON shadow_results(namespace, agent, created_at DESC);
CREATE INDEX idx_shadow_results_selector  ON shadow_results(namespace, selector, selector_generation, created_at DESC);

-- shadow_leases reserves budget before a shadow call is made, so concurrent
-- routers cannot together overspend the benchmark's day. A lease is released by
-- the result that settles it, or expires.
CREATE TABLE shadow_leases (
    id          BIGSERIAL PRIMARY KEY,
    namespace   TEXT NOT NULL,
    benchmark   TEXT NOT NULL,
    agent       TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    source_name TEXT NOT NULL,
    amount_usd  NUMERIC(14, 8) NOT NULL CHECK (amount_usd >= 0),
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_shadow_leases_benchmark ON shadow_leases(namespace, benchmark, expires_at);
CREATE INDEX idx_shadow_leases_agent     ON shadow_leases(namespace, benchmark, agent, created_at DESC);

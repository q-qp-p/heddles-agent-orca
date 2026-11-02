-- 0004: human feedback on routing. A thumbs up or down from a person (or from
-- a caller of the task API) on a whole run, or on one routing decision of it.
-- It is the only quality signal that comes from a human; later stages weigh it
-- against the outcome and shadow evidence. One rating per user per decision, so
-- a second click changes the first. decision_id is empty for a rating of the
-- whole run.
CREATE TABLE routing_feedback (
    id          BIGSERIAL PRIMARY KEY,
    tenant      TEXT,
    namespace   TEXT NOT NULL,
    run_name    TEXT NOT NULL,
    decision_id TEXT NOT NULL DEFAULT '',
    turn_index  INTEGER,
    rating      SMALLINT NOT NULL CHECK (rating IN (-1, 1)),
    comment     TEXT NOT NULL DEFAULT '' CHECK (octet_length(comment) <= 2048),
    user_id     TEXT NOT NULL,
    source      TEXT NOT NULL CHECK (source IN ('ui', 'api')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace, run_name, decision_id, user_id)
);

CREATE INDEX routing_feedback_run_idx ON routing_feedback (namespace, run_name);
CREATE INDEX routing_feedback_decision_idx ON routing_feedback (decision_id) WHERE decision_id <> '';

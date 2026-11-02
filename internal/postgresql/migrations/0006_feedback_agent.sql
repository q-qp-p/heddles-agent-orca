-- 0006: say which Agent a rating is about. Routing is judged per Agent, and a
-- rating names only a run, which is gone once it is cleaned up and which a
-- deployment's chat sessions and a workflow's steps each have their own of. The
-- Agent, and what its work was part of, are copied from the run when the rating
-- is stored. Ratings stored before this have an empty agent.
ALTER TABLE routing_feedback
    ADD COLUMN agent         TEXT NOT NULL DEFAULT '',
    ADD COLUMN deployment    TEXT NOT NULL DEFAULT '',
    ADD COLUMN workflow      TEXT NOT NULL DEFAULT '',
    ADD COLUMN workflow_step TEXT NOT NULL DEFAULT '';

CREATE INDEX routing_feedback_agent_idx ON routing_feedback (namespace, agent, created_at DESC) WHERE agent <> '';

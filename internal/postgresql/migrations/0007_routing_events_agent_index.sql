-- 0007: the routing evaluator reads routing_events by namespace, Agent and time
-- window (shadow samples joined to the served call, volumes, provider failures,
-- ratings). Without this it scans every event in the window and filters by Agent.
CREATE INDEX idx_routing_events_agent ON routing_events(namespace, agent, created_at DESC);

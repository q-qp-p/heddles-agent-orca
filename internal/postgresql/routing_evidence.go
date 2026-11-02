/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package postgresql

import (
	"context"
	"fmt"

	"github.com/heddles/agent-orca/internal/routingbench"
)

// stageCategory is the task category of a stage, as routing recommendations
// and the stage routes of a selector name it.
func stageCategory(stage string) string { return "stage:" + stage }

// LoadObservations reads what the routing detectors need for the Agents of a
// benchmark: judged shadow samples joined to the served call they replayed,
// how much each provider served, how often providers failed, and people's
// ratings of individual decisions. A sample whose served call has aged out of
// routing_events is left out, since its cost is no longer known.
func (s *Store) LoadObservations(ctx context.Context, q routingbench.Query) (routingbench.Observations, error) {
	var ev routingbench.Observations
	agents := pgTextArray(q.Agents)

	rows, err := s.db.QueryContext(ctx, `
		SELECT r.agent, r.stage, r.served_provider, r.shadow_provider, r.winner, r.inconsistent,
		       e.cost_usd::float8, r.shadow_cost_usd::float8, e.algorithm, e.confidence
		FROM shadow_results r
		JOIN routing_events e ON e.decision_id = r.decision_id
		WHERE r.namespace = $1 AND r.agent = ANY($2::text[])
		  AND r.created_at >= $3 AND r.created_at <= $4
		  AND r.winner <> 'invalid' AND r.shadow_error = '' AND r.stage <> ''
		ORDER BY r.created_at, r.id`, q.Namespace, agents, q.Since, q.Now)
	if err != nil {
		return ev, fmt.Errorf("loading shadow samples: %w", err)
	}
	for rows.Next() {
		var sm routingbench.Sample
		var stage string
		if err := rows.Scan(&sm.Agent, &stage, &sm.ServedProvider, &sm.ShadowProvider, &sm.Winner, &sm.Inconsistent,
			&sm.ServedCostUSD, &sm.ShadowCostUSD, &sm.Algorithm, &sm.Confidence); err != nil {
			_ = rows.Close()
			return ev, fmt.Errorf("scanning shadow sample: %w", err)
		}
		sm.Category = stageCategory(stage)
		ev.Samples = append(ev.Samples, sm)
	}
	if err := closeRows(rows); err != nil {
		return ev, fmt.Errorf("loading shadow samples: %w", err)
	}

	rows, err = s.db.QueryContext(ctx, `
		SELECT agent, stage, provider, COUNT(*)
		FROM routing_events
		WHERE NOT is_shadow AND namespace = $1 AND agent = ANY($2::text[])
		  AND created_at >= $3 AND created_at <= $4
		  AND error_class = '' AND fallback_from = '' AND stage <> ''
		GROUP BY agent, stage, provider`, q.Namespace, agents, q.Since, q.Now)
	if err != nil {
		return ev, fmt.Errorf("loading routed volumes: %w", err)
	}
	for rows.Next() {
		var v routingbench.Volume
		var stage string
		if err := rows.Scan(&v.Agent, &stage, &v.Provider, &v.Calls); err != nil {
			_ = rows.Close()
			return ev, fmt.Errorf("scanning routed volume: %w", err)
		}
		v.Category = stageCategory(stage)
		ev.Volumes = append(ev.Volumes, v)
	}
	if err := closeRows(rows); err != nil {
		return ev, fmt.Errorf("loading routed volumes: %w", err)
	}

	rows, err = s.db.QueryContext(ctx, `
		SELECT COALESCE(selector, ''), provider, COUNT(*), COUNT(*) FILTER (WHERE error_class <> '')
		FROM routing_events
		WHERE NOT is_shadow AND namespace = $1 AND agent = ANY($2::text[])
		  AND created_at >= $3 AND created_at <= $4
		GROUP BY selector, provider
		ORDER BY selector, provider`, q.Namespace, agents, q.Since, q.Now)
	if err != nil {
		return ev, fmt.Errorf("loading provider outcomes: %w", err)
	}
	for rows.Next() {
		var st routingbench.ProviderStat
		if err := rows.Scan(&st.Selector, &st.Provider, &st.Calls, &st.Errors); err != nil {
			_ = rows.Close()
			return ev, fmt.Errorf("scanning provider outcome: %w", err)
		}
		ev.ProviderStats = append(ev.ProviderStats, st)
	}
	if err := closeRows(rows); err != nil {
		return ev, fmt.Errorf("loading provider outcomes: %w", err)
	}

	rows, err = s.db.QueryContext(ctx, `
		SELECT f.agent, e.stage, COUNT(*) FILTER (WHERE f.rating = 1), COUNT(*) FILTER (WHERE f.rating = -1)
		FROM routing_feedback f
		JOIN routing_events e ON e.decision_id = f.decision_id
		WHERE f.namespace = $1 AND f.agent = ANY($2::text[]) AND f.decision_id <> ''
		  AND f.created_at >= $3 AND f.created_at <= $4 AND e.stage <> ''
		GROUP BY f.agent, e.stage`, q.Namespace, agents, q.Since, q.Now)
	if err != nil {
		return ev, fmt.Errorf("loading ratings: %w", err)
	}
	ev.Ratings = map[routingbench.AgentCategory]routingbench.Ratings{}
	for rows.Next() {
		var agent, stage string
		var r routingbench.Ratings
		if err := rows.Scan(&agent, &stage, &r.Up, &r.Down); err != nil {
			_ = rows.Close()
			return ev, fmt.Errorf("scanning ratings: %w", err)
		}
		ev.Ratings[routingbench.AgentCategory{Agent: agent, Category: stageCategory(stage)}] = r
	}
	if err := closeRows(rows); err != nil {
		return ev, fmt.Errorf("loading ratings: %w", err)
	}
	return ev, nil
}

// closeRows closes rows and returns the iteration error, if any.
func closeRows(rows interface {
	Err() error
	Close() error
}) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

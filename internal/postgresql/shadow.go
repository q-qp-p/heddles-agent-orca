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
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Judge verdicts stored in shadow_results.winner.
const (
	ShadowWinnerServed  = "served"
	ShadowWinnerShadow  = "shadow"
	ShadowWinnerTie     = "tie"
	ShadowWinnerInvalid = "invalid"
)

// ErrInvalidShadowResult is returned for a result the table would reject.
var ErrInvalidShadowResult = errors.New("invalid shadow result")

// Kinds of source a shadow result or lease can come from.
const (
	SourceKindRun        = "AgentRun"
	SourceKindDeployment = "AgentDeployment"
)

// ShadowResult is one counterfactual call and the judge's verdict on it.
type ShadowResult struct {
	DecisionID   string `json:"decisionId"`
	Tenant       string `json:"tenant,omitempty"`
	Namespace    string `json:"namespace"`
	Benchmark    string `json:"benchmark"`
	Stage        string `json:"stage,omitempty"`
	SYLabel      string `json:"syLabel,omitempty"`
	WorkflowStep string `json:"workflowStep,omitempty"`

	// The operator fills these from the call's source, not from the router: the
	// Agent the work belongs to, where it ran, and the selector that routed it.
	Agent              string `json:"agent,omitempty"`
	SourceKind         string `json:"sourceKind,omitempty"`
	SourceName         string `json:"sourceName,omitempty"`
	Selector           string `json:"selector,omitempty"`
	SelectorGeneration int64  `json:"selectorGeneration,omitempty"`

	ServedProvider string `json:"servedProvider"`
	ShadowProvider string `json:"shadowProvider"`

	Winner          string          `json:"winner"`
	Inconsistent    bool            `json:"inconsistent,omitempty"`
	JudgeConfidence float64         `json:"judgeConfidence,omitempty"`
	JudgeProvider   string          `json:"judgeProvider,omitempty"`
	Dimensions      json.RawMessage `json:"dimensions,omitempty"`
	JudgeReason     string          `json:"judgeReason,omitempty"`

	ShadowPromptTokens     int     `json:"shadowPromptTokens,omitempty"`
	ShadowCompletionTokens int     `json:"shadowCompletionTokens,omitempty"`
	ShadowCostUSD          float64 `json:"shadowCostUsd,omitempty"`
	JudgeCostUSD           float64 `json:"judgeCostUsd,omitempty"`
	ShadowLatencyMS        int     `json:"shadowLatencyMs,omitempty"`
	ShadowToolCalls        int     `json:"shadowToolCalls,omitempty"`
	ShadowError            string  `json:"shadowError,omitempty"`

	// Transcript is stored only when the benchmark sets storeTranscripts.
	Transcript json.RawMessage `json:"transcript,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// Validate reports why r cannot be stored.
func (r ShadowResult) Validate() error {
	switch {
	case r.DecisionID == "" || r.Namespace == "" || r.Benchmark == "":
		return fmt.Errorf("%w: decision, namespace and benchmark are required", ErrInvalidShadowResult)
	case r.Agent == "" || r.SourceName == "" || !validSourceKind(r.SourceKind):
		return fmt.Errorf("%w: agent and a run or deployment source are required", ErrInvalidShadowResult)
	case r.ServedProvider == "" || r.ShadowProvider == "":
		return fmt.Errorf("%w: served and shadow providers are required", ErrInvalidShadowResult)
	case r.Winner != ShadowWinnerServed && r.Winner != ShadowWinnerShadow &&
		r.Winner != ShadowWinnerTie && r.Winner != ShadowWinnerInvalid:
		return fmt.Errorf("%w: winner must be served, shadow, tie or invalid", ErrInvalidShadowResult)
	}
	return nil
}

func validSourceKind(k string) bool { return k == SourceKindRun || k == SourceKindDeployment }

const upsertShadowResultSQL = `
INSERT INTO shadow_results
	(decision_id, tenant, namespace, agent, source_kind, source_name, benchmark, selector, selector_generation,
	 stage, sy_label, workflow_step, served_provider, shadow_provider, winner, inconsistent, judge_confidence, judge_provider, dimensions,
	 judge_reason, shadow_prompt_tokens, shadow_completion_tokens, shadow_cost_usd, judge_cost_usd,
	 shadow_latency_ms, shadow_tool_calls, shadow_error, transcript, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
	$23, $24, $25, $26, $27, $28, $29)
ON CONFLICT (decision_id, shadow_provider) DO UPDATE SET
	winner = EXCLUDED.winner,
	inconsistent = EXCLUDED.inconsistent,
	judge_confidence = EXCLUDED.judge_confidence,
	judge_provider = EXCLUDED.judge_provider,
	dimensions = EXCLUDED.dimensions,
	judge_reason = EXCLUDED.judge_reason,
	shadow_prompt_tokens = EXCLUDED.shadow_prompt_tokens,
	shadow_completion_tokens = EXCLUDED.shadow_completion_tokens,
	shadow_cost_usd = EXCLUDED.shadow_cost_usd,
	judge_cost_usd = EXCLUDED.judge_cost_usd,
	shadow_latency_ms = EXCLUDED.shadow_latency_ms,
	shadow_tool_calls = EXCLUDED.shadow_tool_calls,
	shadow_error = EXCLUDED.shadow_error,
	transcript = EXCLUDED.transcript`

// UpsertShadowResult stores one result; re-posting the same (decision, shadow
// provider) refreshes it.
func (s *Store) UpsertShadowResult(ctx context.Context, r ShadowResult) error {
	if err := r.Validate(); err != nil {
		return err
	}
	created := r.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	var dims, transcript any
	if len(r.Dimensions) > 0 {
		dims = []byte(r.Dimensions)
	}
	if len(r.Transcript) > 0 {
		transcript = []byte(r.Transcript)
	}
	if _, err := s.db.ExecContext(ctx, upsertShadowResultSQL,
		r.DecisionID, r.Tenant, r.Namespace, r.Agent, r.SourceKind, r.SourceName, r.Benchmark, r.Selector,
		r.SelectorGeneration, r.Stage, r.SYLabel, r.WorkflowStep, r.ServedProvider, r.ShadowProvider, r.Winner, r.Inconsistent, r.JudgeConfidence,
		r.JudgeProvider, dims, r.JudgeReason, r.ShadowPromptTokens, r.ShadowCompletionTokens, r.ShadowCostUSD,
		r.JudgeCostUSD, r.ShadowLatencyMS, r.ShadowToolCalls, r.ShadowError, transcript, created,
	); err != nil {
		return fmt.Errorf("storing shadow result %s: %w", r.DecisionID, err)
	}
	return nil
}

// ShadowSpend is a benchmark's spend over a period, split by what was spent on.
type ShadowSpend struct {
	// SettledUSD is the cost of shadow and judge calls already recorded.
	SettledUSD float64
	// LeasedUSD is what unexpired leases hold for calls not yet recorded.
	LeasedUSD float64
}

// Total is everything spent or reserved.
func (s ShadowSpend) Total() float64 { return s.SettledUSD + s.LeasedUSD }

const shadowSpendSQL = `
SELECT
	COALESCE((SELECT SUM(shadow_cost_usd + judge_cost_usd) FROM shadow_results
		WHERE namespace = $1 AND benchmark = $2 AND created_at >= $3), 0),
	COALESCE((SELECT SUM(amount_usd) FROM shadow_leases
		WHERE namespace = $1 AND benchmark = $2 AND expires_at > $4), 0)`

// ShadowSpendSince returns what a benchmark has spent since the given time and
// what its live leases hold at now.
func (s *Store) ShadowSpendSince(ctx context.Context, namespace, benchmark string, since, now time.Time) (ShadowSpend, error) {
	var out ShadowSpend
	err := s.db.QueryRowContext(ctx, shadowSpendSQL, namespace, benchmark, since, now).Scan(&out.SettledUSD, &out.LeasedUSD)
	if err != nil {
		return ShadowSpend{}, fmt.Errorf("reading shadow spend: %w", err)
	}
	return out, nil
}

// LeaseRequest asks to reserve budget for one shadow call.
type LeaseRequest struct {
	Namespace string
	Benchmark string
	// Agent is the Agent whose call is sampled; SourceKind and SourceName say
	// whether it runs as an AgentRun or an AgentDeployment.
	Agent      string
	SourceKind string
	SourceName string
	// AmountUSD is the most the call (shadow and judge) may cost.
	AmountUSD float64
	// DayStart is the start of the UTC day the budget applies to.
	DayStart time.Time
	// PerDayUSD and PerRunUSD are the benchmark's caps; PerRunUSD 0 means none.
	// A deployment's run cap counts spend since DayStart.
	PerDayUSD float64
	PerRunUSD float64
	// MaxPerHour is the most calls sampled for the Agent in the hour before
	// Now; 0 means no cap.
	MaxPerHour int
	TTL        time.Duration
	Now        time.Time
}

// ErrLeaseDenied is returned when granting a lease would exceed a cap.
var ErrLeaseDenied = errors.New("shadow budget exhausted")

// TakeShadowLease reserves AmountUSD against the day and run caps, counting
// settled spend and live leases, atomically. It returns ErrLeaseDenied when the
// reservation does not fit. The advisory lock serialises concurrent routers for
// one benchmark, so they cannot together overspend.
func (s *Store) TakeShadowLease(ctx context.Context, req LeaseRequest) (int64, error) {
	if req.Namespace == "" || req.Benchmark == "" || req.AmountUSD < 0 || req.Agent == "" || req.SourceName == "" || !validSourceKind(req.SourceKind) {
		return 0, fmt.Errorf("%w: namespace, benchmark, agent, source and a non-negative amount are required", ErrInvalidShadowResult)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("taking shadow lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, req.Namespace+"/"+req.Benchmark); err != nil {
		return 0, fmt.Errorf("locking shadow budget: %w", err)
	}
	var day, leased, run float64
	var calls int
	if err := tx.QueryRowContext(ctx, shadowSpendSQL, req.Namespace, req.Benchmark, req.DayStart, req.Now).Scan(&day, &leased); err != nil {
		return 0, fmt.Errorf("reading shadow spend: %w", err)
	}
	if day+leased+req.AmountUSD > req.PerDayUSD {
		return 0, ErrLeaseDenied
	}
	if req.PerRunUSD > 0 {
		// A run is short; a deployment keeps going, so it gets the cap per day.
		since := time.Time{}
		if req.SourceKind == SourceKindDeployment {
			since = req.DayStart
		}
		if err := tx.QueryRowContext(ctx, shadowSourceSpendSQL, req.Namespace, req.Benchmark, req.SourceKind, req.SourceName, req.Now, since).Scan(&run); err != nil {
			return 0, fmt.Errorf("reading shadow source spend: %w", err)
		}
		if run+req.AmountUSD > req.PerRunUSD {
			return 0, ErrLeaseDenied
		}
	}
	if req.MaxPerHour > 0 {
		if err := tx.QueryRowContext(ctx, shadowAgentCallsSQL, req.Namespace, req.Benchmark, req.Agent, req.Now.Add(-time.Hour), req.Now).Scan(&calls); err != nil {
			return 0, fmt.Errorf("reading shadow agent calls: %w", err)
		}
		if calls >= req.MaxPerHour {
			return 0, ErrLeaseDenied
		}
	}
	var id int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO shadow_leases (namespace, benchmark, agent, source_kind, source_name, amount_usd, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		req.Namespace, req.Benchmark, req.Agent, req.SourceKind, req.SourceName, req.AmountUSD, req.Now.Add(req.TTL)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("recording shadow lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("taking shadow lease: %w", err)
	}
	return id, nil
}

// shadowSourceSpendSQL is what one run or deployment has spent or reserved since
// $6 (the zero time counts everything).
const shadowSourceSpendSQL = `
SELECT
	COALESCE((SELECT SUM(shadow_cost_usd + judge_cost_usd) FROM shadow_results
		WHERE namespace = $1 AND benchmark = $2 AND source_kind = $3 AND source_name = $4 AND created_at >= $6), 0) +
	COALESCE((SELECT SUM(amount_usd) FROM shadow_leases
		WHERE namespace = $1 AND benchmark = $2 AND source_kind = $3 AND source_name = $4 AND expires_at > $5), 0)`

// shadowAgentCallsSQL counts the calls sampled for an Agent since $4, settled or
// still leased.
const shadowAgentCallsSQL = `
SELECT
	(SELECT COUNT(*) FROM (SELECT DISTINCT decision_id FROM shadow_results
		WHERE namespace = $1 AND benchmark = $2 AND agent = $3 AND created_at >= $4) d) +
	(SELECT COUNT(*) FROM shadow_leases
		WHERE namespace = $1 AND benchmark = $2 AND agent = $3 AND expires_at > $5)`

// ReleaseShadowLease deletes a lease once its result is recorded, or the call
// was not made. Releasing an unknown lease is not an error.
func (s *Store) ReleaseShadowLease(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM shadow_leases WHERE id = $1`, id); err != nil {
		return fmt.Errorf("releasing shadow lease: %w", err)
	}
	return nil
}

// ShadowTally counts judged pairs for one category of one Agent.
type ShadowTally struct {
	Agent        string
	Category     string
	Served       int
	Shadow       int
	Ties         int
	Invalid      int
	Inconsistent int
}

// ShadowTallies counts a benchmark's results since the given time by Agent and
// stage.
func (s *Store) ShadowTallies(ctx context.Context, namespace, benchmark string, since time.Time) ([]ShadowTally, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT agent, stage,
	COUNT(*) FILTER (WHERE winner = 'served'),
	COUNT(*) FILTER (WHERE winner = 'shadow'),
	COUNT(*) FILTER (WHERE winner = 'tie'),
	COUNT(*) FILTER (WHERE winner = 'invalid'),
	COUNT(*) FILTER (WHERE inconsistent)
FROM shadow_results
WHERE namespace = $1 AND benchmark = $2 AND created_at >= $3
GROUP BY agent, stage ORDER BY agent, stage`, namespace, benchmark, since)
	if err != nil {
		return nil, fmt.Errorf("reading shadow tallies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ShadowTally
	for rows.Next() {
		var t ShadowTally
		if err := rows.Scan(&t.Agent, &t.Category, &t.Served, &t.Shadow, &t.Ties, &t.Invalid, &t.Inconsistent); err != nil {
			return nil, fmt.Errorf("reading shadow tallies: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading shadow tallies: %w", err)
	}
	return out, nil
}

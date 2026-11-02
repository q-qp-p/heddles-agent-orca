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
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Feedback sources.
const (
	FeedbackSourceUI  = "ui"
	FeedbackSourceAPI = "api"
)

// MaxFeedbackCommentBytes bounds a feedback comment; the table enforces it too.
const MaxFeedbackCommentBytes = 2048

// ErrInvalidFeedback is returned for feedback the table would reject.
var ErrInvalidFeedback = errors.New("invalid feedback")

// Feedback is one person's thumbs up (1) or down (-1) on a run, or on one of
// its routing decisions.
type Feedback struct {
	Tenant    string `json:"tenant,omitempty"`
	Namespace string `json:"namespace"`
	RunName   string `json:"runName"`
	// Agent is the Agent whose work was rated, so ratings build up per Agent.
	Agent string `json:"agent"`
	// Deployment, Workflow and WorkflowStep say what the run was part of: an
	// AgentDeployment's chat session, or a workflow step. Empty when neither.
	Deployment   string `json:"deployment,omitempty"`
	Workflow     string `json:"workflow,omitempty"`
	WorkflowStep string `json:"workflowStep,omitempty"`
	// DecisionID is empty for a rating of the whole run.
	DecisionID string `json:"decisionId,omitempty"`
	// TurnIndex is the turn the rating is for, when the caller knows it.
	TurnIndex *int      `json:"turn,omitempty"`
	Rating    int       `json:"rating"`
	Comment   string    `json:"comment,omitempty"`
	UserID    string    `json:"user"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Validate reports why f cannot be stored.
func (f Feedback) Validate() error {
	switch {
	case f.Namespace == "" || f.RunName == "":
		return fmt.Errorf("%w: namespace and run are required", ErrInvalidFeedback)
	case f.Agent == "":
		return fmt.Errorf("%w: agent is required", ErrInvalidFeedback)
	case f.Rating != 1 && f.Rating != -1:
		return fmt.Errorf("%w: rating must be 1 or -1", ErrInvalidFeedback)
	case len(f.Comment) > MaxFeedbackCommentBytes:
		return fmt.Errorf("%w: comment is over %d bytes", ErrInvalidFeedback, MaxFeedbackCommentBytes)
	case f.UserID == "":
		return fmt.Errorf("%w: user is required", ErrInvalidFeedback)
	case f.Source != FeedbackSourceUI && f.Source != FeedbackSourceAPI:
		return fmt.Errorf("%w: source must be %q or %q", ErrInvalidFeedback, FeedbackSourceUI, FeedbackSourceAPI)
	case f.TurnIndex != nil && *f.TurnIndex < 0:
		return fmt.Errorf("%w: turn must not be negative", ErrInvalidFeedback)
	}
	return nil
}

const upsertFeedbackSQL = `
INSERT INTO routing_feedback
	(tenant, namespace, run_name, agent, deployment, workflow, workflow_step,
	 decision_id, turn_index, rating, comment, user_id, source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (namespace, run_name, decision_id, user_id) DO UPDATE SET
	rating = EXCLUDED.rating,
	comment = EXCLUDED.comment,
	turn_index = EXCLUDED.turn_index,
	source = EXCLUDED.source,
	updated_at = now()`

// UpsertFeedback stores a rating. A user rating the same decision again (or the
// same run, when there is no decision) replaces the earlier rating.
func (s *Store) UpsertFeedback(ctx context.Context, f Feedback) error {
	if err := f.Validate(); err != nil {
		return err
	}
	var turn any
	if f.TurnIndex != nil {
		turn = *f.TurnIndex
	}
	if _, err := s.db.ExecContext(ctx, upsertFeedbackSQL,
		f.Tenant, f.Namespace, f.RunName, f.Agent, f.Deployment, f.Workflow, f.WorkflowStep,
		f.DecisionID, turn, f.Rating, f.Comment, f.UserID, f.Source); err != nil {
		return fmt.Errorf("storing feedback: %w", err)
	}
	return nil
}

// FeedbackForRun returns the ratings on a run, oldest first. A non-empty user
// limits them to that user's.
func (s *Store) FeedbackForRun(ctx context.Context, namespace, run, user string) ([]Feedback, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(tenant, ''), agent, deployment, workflow, workflow_step, decision_id, turn_index, rating,
			comment, user_id, source, created_at, updated_at
		FROM routing_feedback
		WHERE namespace = $1 AND run_name = $2 AND ($3 = '' OR user_id = $3)
		ORDER BY created_at, id`, namespace, run, user)
	if err != nil {
		return nil, fmt.Errorf("querying feedback: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Feedback{}
	for rows.Next() {
		f := Feedback{Namespace: namespace, RunName: run}
		var turn sql.NullInt64
		if err := rows.Scan(&f.Tenant, &f.Agent, &f.Deployment, &f.Workflow, &f.WorkflowStep, &f.DecisionID, &turn, &f.Rating, &f.Comment, &f.UserID, &f.Source, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning feedback: %w", err)
		}
		if turn.Valid {
			n := int(turn.Int64)
			f.TurnIndex = &n
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

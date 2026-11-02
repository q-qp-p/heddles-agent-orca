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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Approval statuses.
const (
	ApprovalPending   = "pending"
	ApprovalApproved  = "approved"
	ApprovalRejected  = "rejected"
	ApprovalExpired   = "expired"
	ApprovalWithdrawn = "withdrawn"
)

// ApprovalKindEgress is a request to allow sandbox egress (an OpenShell
// policy draft chunk).
const ApprovalKindEgress = "egress"

// ApprovalKindTool is a request to run a tool whose side-effect class needs an
// approver (GuardrailPolicy.spec.toolApproval).
const ApprovalKindTool = "tool"

// ApprovalKindWorkflow is an approval step of an AgentWorkflow waiting for a decision.
const ApprovalKindWorkflow = "workflow"

// ApprovalKindBudget is a request to raise a paused run's spend cap.
const ApprovalKindBudget = "budget"

// ApprovalKindRouting is a request to apply a RoutingRecommendation's patch to a
// ModelSelector. It belongs to no run, so its run name is empty.
const ApprovalKindRouting = "routing"

// ApprovalKindRoutingRollback is a request to undo an applied RoutingRecommendation.
const ApprovalKindRoutingRollback = "routing-rollback"

// ErrApprovalNotPending is returned when deciding an approval that has
// already been decided, expired or withdrawn.
var ErrApprovalNotPending = errors.New("approval is no longer pending")

// Approval is a request for a decision (table approvals, migration 0002).
type Approval struct {
	ID             string          `json:"id"`
	Tenant         string          `json:"tenant,omitempty"`
	Namespace      string          `json:"namespace"`
	RunName        string          `json:"runName"`
	Kind           string          `json:"kind"`
	SourceKey      string          `json:"sourceKey"`
	Summary        string          `json:"summary"`
	Details        json.RawMessage `json:"details,omitempty"`
	ApproverGroups []string        `json:"approverGroups"`
	Status         string          `json:"status"`
	RequestedAt    time.Time       `json:"requestedAt"`
	ExpiresAt      time.Time       `json:"expiresAt"`
	DecidedBy      string          `json:"decidedBy,omitempty"`
	DecidedAt      *time.Time      `json:"decidedAt,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	AppliedAt      *time.Time      `json:"appliedAt,omitempty"`
	// Created is set by RequestApproval when the request is new rather than a
	// repeat of an existing one.
	Created bool `json:"-"`
}

const approvalColumns = `id, COALESCE(tenant, ''), namespace, run_name, kind, source_key, summary, details,
	approver_groups, status, requested_at, expires_at, COALESCE(decided_by, ''), decided_at,
	COALESCE(reason, ''), applied_at`

func scanApproval(row interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var details, groups []byte
	var decidedAt, appliedAt sql.NullTime
	err := row.Scan(&a.ID, &a.Tenant, &a.Namespace, &a.RunName, &a.Kind, &a.SourceKey, &a.Summary, &details,
		&groups, &a.Status, &a.RequestedAt, &a.ExpiresAt, &a.DecidedBy, &decidedAt, &a.Reason, &appliedAt)
	if err != nil {
		return a, err
	}
	a.Details = json.RawMessage(details)
	if err := json.Unmarshal(groups, &a.ApproverGroups); err != nil {
		return a, fmt.Errorf("approval %s: approver_groups: %w", a.ID, err)
	}
	if decidedAt.Valid {
		a.DecidedAt = &decidedAt.Time
	}
	if appliedAt.Valid {
		a.AppliedAt = &appliedAt.Time
	}
	return a, nil
}

// RequestApproval records a request, or refreshes the details of the
// existing request with the same kind and source key (whatever its status),
// and returns the stored row. A decided request is not reopened.
func (s *Store) RequestApproval(ctx context.Context, a Approval) (Approval, error) {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.Status == "" {
		a.Status = ApprovalPending
	}
	if len(a.Details) == 0 {
		a.Details = json.RawMessage(`{}`)
	}
	groups, err := json.Marshal(nonNil(a.ApproverGroups))
	if err != nil {
		return a, err
	}
	var decidedAt any
	if a.DecidedAt != nil {
		decidedAt = *a.DecidedAt
	}
	row := s.db.QueryRowContext(ctx, `
INSERT INTO approvals (id, tenant, namespace, run_name, kind, source_key, summary, details,
	approver_groups, status, expires_at, decided_by, decided_at, reason)
VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''), $13, NULLIF($14, ''))
ON CONFLICT (kind, source_key) DO UPDATE SET details = EXCLUDED.details
RETURNING `+approvalColumns+`, (xmax = 0)`,
		a.ID, a.Tenant, a.Namespace, a.RunName, a.Kind, a.SourceKey, a.Summary, []byte(a.Details),
		groups, a.Status, a.ExpiresAt, a.DecidedBy, decidedAt, a.Reason)
	var created bool
	stored, err := scanApproval(rowWithExtra{row, &created})
	stored.Created = created
	return stored, err
}

// rowWithExtra scans an approval row followed by extra columns.
type rowWithExtra struct {
	row   *sql.Row
	extra *bool
}

func (r rowWithExtra) Scan(dest ...any) error {
	return r.row.Scan(append(dest, r.extra)...)
}

// ApprovalQuery filters ListApprovals. Empty fields match everything.
type ApprovalQuery struct {
	// Namespaces scopes the query to a tenant's namespaces.
	Namespaces []string
	Namespace  string
	RunName    string
	Status     string
	Limit      int
}

// ListApprovals returns approvals, newest first.
func (s *Store) ListApprovals(ctx context.Context, q ApprovalQuery) ([]Approval, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if len(q.Namespaces) > 0 {
		placeholders := make([]string, len(q.Namespaces))
		for i, ns := range q.Namespaces {
			placeholders[i] = arg(ns)
		}
		where = append(where, "namespace IN ("+strings.Join(placeholders, ", ")+")")
	}
	if q.Namespace != "" {
		where = append(where, "namespace = "+arg(q.Namespace))
	}
	if q.RunName != "" {
		where = append(where, "run_name = "+arg(q.RunName))
	}
	if q.Status != "" {
		where = append(where, "status = "+arg(q.Status))
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := "SELECT " + approvalColumns + " FROM approvals"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY requested_at DESC LIMIT " + arg(limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing approvals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApproval returns one approval, or sql.ErrNoRows.
func (s *Store) GetApproval(ctx context.Context, id string) (Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE id = $1", id))
}

// DecideApproval records a decision on a pending approval. It fails with
// ErrApprovalNotPending if the approval was decided, expired or withdrawn
// meanwhile, so two approvers cannot both decide.
func (s *Store) DecideApproval(ctx context.Context, id, status, by, reason string) (Approval, error) {
	switch status {
	case ApprovalApproved, ApprovalRejected, ApprovalExpired, ApprovalWithdrawn:
	default:
		return Approval{}, fmt.Errorf("invalid decision %q", status)
	}
	a, err := scanApproval(s.db.QueryRowContext(ctx, `
UPDATE approvals SET status = $2, decided_by = NULLIF($3, ''), decided_at = NOW(), reason = NULLIF($4, '')
WHERE id = $1 AND status = 'pending'
RETURNING `+approvalColumns, id, status, by, reason))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrApprovalNotPending
	}
	return a, err
}

// MarkApprovalApplied records that a decision reached its source.
func (s *Store) MarkApprovalApplied(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE approvals SET applied_at = NOW() WHERE id = $1 AND applied_at IS NULL`, id)
	return err
}

// WithdrawRunApprovals withdraws a run's pending approvals when it ends.
func (s *Store) WithdrawRunApprovals(ctx context.Context, namespace, runName string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE approvals SET status = 'withdrawn', decided_at = NOW(), reason = 'the run ended'
WHERE namespace = $1 AND run_name = $2 AND status = 'pending'`, namespace, runName)
	return err
}

// WithdrawBySourcePrefix withdraws the pending approvals of one kind whose source
// key starts with prefix (a workflow's approval steps, when it ends).
func (s *Store) WithdrawBySourcePrefix(ctx context.Context, kind, prefix, reason string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE approvals SET status = 'withdrawn', decided_at = NOW(), reason = $3
WHERE kind = $1 AND starts_with(source_key, $2) AND status = 'pending'`, kind, prefix, reason)
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

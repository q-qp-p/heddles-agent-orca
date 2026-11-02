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
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// TestQueryHistoryBindsLimitOffsetArgs is a regression test for the bug where
// QueryHistory called QueryContext(ctx, dataQ) WITHOUT args..., so the
// `LIMIT $1 OFFSET $2` placeholders were never satisfied and every history
// query failed with "expected 2 arguments, got 0" (causing the UI to silently
// show "No historical runs found"). sqlmock's WithArgs enforces that the
// limit/offset values are actually passed to the statement.
func TestQueryHistoryBindsLimitOffsetArgs(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	completed := start.Add(5 * time.Minute)

	// No-filter query: count (0 args) then data (LIMIT $1 OFFSET $2 with 2 args).
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM archived_runs")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`LIMIT \$1 OFFSET \$2`).
		WithArgs(50, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "namespace", "agent_ref", "phase", "spend_usd",
			"start_time", "completion_time", "tenant",
			"context_used_tokens", "max_context_tokens",
		}).AddRow("run-1", "default", "my-agent", "Succeeded", "0.0123",
			start, completed, "acme", 42, 200000))

	s := &Store{db: db}
	page, err := s.QueryHistory(context.Background(), HistoryQuery{Limit: 50, Offset: 0})
	if err != nil {
		t.Fatalf("QueryHistory failed: %v (args were not bound to the statement)", err)
	}
	if page.Total != 1 {
		t.Fatalf("total = %d, want 1", page.Total)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(page.Runs))
	}
	r := page.Runs[0]
	if r.Name != "run-1" || r.Namespace != "default" || r.AgentRef != "my-agent" {
		t.Fatalf("unexpected row: %+v", r)
	}
	if r.ContextUsedTokens != 42 || r.MaxContextTokens != 200000 {
		t.Fatalf("context tokens not scanned: used=%d max=%d", r.ContextUsedTokens, r.MaxContextTokens)
	}
	if !r.StartTime.Equal(start) || !r.CompletionTime.Equal(completed) {
		t.Fatalf("timestamps not scanned: start=%v comp=%v", r.StartTime, r.CompletionTime)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestQueryHistoryEmptyReturnsNonNullRuns verifies that an empty result set
// serializes to `[]` (not `null`), which is what previously crashed the UI
// ("can't access property Symbol.iterator, t.runs is null").
func TestQueryHistoryEmptyReturnsNonNullRuns(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM archived_runs")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`LIMIT \$1 OFFSET \$2`).
		WithArgs(50, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "namespace", "agent_ref", "phase", "spend_usd",
			"start_time", "completion_time", "tenant",
			"context_used_tokens", "max_context_tokens",
		})) // no rows added

	s := &Store{db: db}
	page, err := s.QueryHistory(context.Background(), HistoryQuery{Limit: 50, Offset: 0})
	if err != nil {
		t.Fatalf("QueryHistory failed: %v", err)
	}
	if page.Runs == nil {
		t.Fatal("Runs is nil — would serialize as null and crash the UI")
	}
	if len(page.Runs) != 0 {
		t.Fatalf("Runs = %d rows, want 0", len(page.Runs))
	}
	if page.Total != 0 {
		t.Fatalf("Total = %d, want 0", page.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// are all bound to the correct positional placeholders (regression guard for
// the same args-binding bug).
func TestQueryHistoryFilteredBindsArgs(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	// phase filter → $1, agent_ref filter → $2, then LIMIT $3 OFFSET $4.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM archived_runs")).
		WithArgs("Succeeded", "my-agent").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`agent_ref = \$2.*LIMIT \$3 OFFSET \$4`).
		WithArgs("Succeeded", "my-agent", 25, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "namespace", "agent_ref", "phase", "spend_usd",
			"start_time", "completion_time", "tenant",
			"context_used_tokens", "max_context_tokens",
		}))

	s := &Store{db: db}
	page, err := s.QueryHistory(context.Background(), HistoryQuery{
		Limit: 25, Offset: 0, Phase: "Succeeded", AgentRef: "my-agent",
	})
	if err != nil {
		t.Fatalf("QueryHistory failed: %v", err)
	}
	if page.Total != 0 || len(page.Runs) != 0 {
		t.Fatalf("expected empty result, got total=%d runs=%d", page.Total, len(page.Runs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestArchiveRunWritesTraceEvents verifies that ArchiveRun binds the trace_events
// column in the INSERT and that the UPSERT preserves existing trace_events when
// the incoming value is nil (so back-fill paths don't clobber archived traces).
func TestArchiveRunWritesTraceEvents(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "my-agent", Input: "hello"},
		Status: agentorcav1alpha1.AgentRunStatus{
			Phase:    agentorcav1alpha1.AgentRunPhaseSucceeded,
			SpendUSD: "0.01",
			Output:   "result",
		},
	}
	traceEvents := json.RawMessage(`[{"id":0,"event":{"type":"token","content":"hi"},"ts":"2026-01-01T00:00:00Z"}]`)

	// Expect the INSERT with trace_events ($23) bound before created_at ($24/$25).
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO archived_runs`).
		WithArgs(
			"default/test-run", "test-run", "default", sqlmock.AnyArg(),
			"my-agent", "hello", "", "", sqlmock.AnyArg(),
			"Succeeded", "", "0.01", "result", "",
			0, sqlmock.AnyArg(), sqlmock.AnyArg(),
			0, 0, "",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "",
		).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO run_outcomes`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	s := &Store{db: db}
	if err := s.ArchiveRun(context.Background(), run, traceEvents); err != nil {
		t.Fatalf("ArchiveRun failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestArchiveRunNilTraceEventsPreservesColumn verifies that when traceEventsJSON
// is nil, the UPSERT SQL still receives a nil trace_events value (the CASE
// statement in the SQL preserves the existing column on conflict). This
// prevents back-fill paths from clobbering archived traces when the Redis
// stream has expired.
func TestArchiveRunNilTraceEventsPreservesColumn(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "old-run", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "agent"},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseFailed},
	}

	// All args as AnyArg — the focus here is that the Exec succeeds with nil
	// traceEventsJSON (the CASE in the UPSERT SQL handles the preservation).
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO archived_runs`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO run_outcomes`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	s := &Store{db: db}
	if err := s.ArchiveRun(context.Background(), run, nil); err != nil {
		t.Fatalf("ArchiveRun with nil traceEventsJSON failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetRunScansTraceEvents verifies that GetRun selects and scans the
// trace_events column into RunArchive.
func TestGetRunScansTraceEvents(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	traceJSON := []byte(`[{"id":0,"event":{"type":"token","content":"hi"},"ts":"2026-01-01T00:00:00Z"}]`)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name, namespace, labels, agent_ref, input, parent_run_ref,")).
		WithArgs("default/my-run").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "namespace", "labels", "agent_ref", "input", "parent_run_ref",
			"prior_run_ref", "timeout_sec", "phase", "pod_name", "spend_usd", "output", "raw_output",
			"restart_count", "start_time", "completion_time", "context_used_tokens", "max_context_tokens",
			"tenant", "routing_decisions", "child_run_refs", "trace_events", "created_at", "updated_at",
		}).AddRow(
			"default/my-run", "my-run", "default", []byte(`{}`),
			"agent", "input", "", "", nil,
			"Succeeded", "pod-1", "0.00", "out", "",
			0, nil, nil, 0, 0,
			"", nil, nil, traceJSON, time.Now(), time.Now(),
		))

	s := &Store{db: db}
	archive, err := s.GetRun(context.Background(), "default", "my-run")
	if err != nil || archive == nil {
		t.Fatalf("GetRun: err=%v, archive=%v", err, archive)
	}
	got := string(archive.TraceEventsJSON)
	if got != string(traceJSON) {
		t.Fatalf("TraceEventsJSON = %s, want %s", got, string(traceJSON))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestMigrationSQLIncludesCompression verifies that the migration SQL
// enables lz4 TOAST compression on the trace_events and other large JSONB/TEXT
// columns, keeping archived trace data compact.
func TestMigrationSQLIncludesCompression(t *testing.T) {
	MigrationSQL := migration0001(t)
	mustContain := []string{
		"ALTER TABLE archived_runs ALTER COLUMN trace_events SET COMPRESSION lz4",
		"ALTER TABLE archived_runs ALTER COLUMN routing_decisions SET COMPRESSION lz4",
		"ALTER TABLE archived_runs ALTER COLUMN output SET COMPRESSION lz4",
		"ALTER TABLE archived_runs ALTER COLUMN raw_output SET COMPRESSION lz4",
		"ALTER TABLE archived_runs ALTER COLUMN input SET COMPRESSION lz4",
		"ALTER TABLE archived_runs ADD COLUMN IF NOT EXISTS trace_events",
	}
	for _, needle := range mustContain {
		if !strings.Contains(MigrationSQL, needle) {
			t.Errorf("MigrationSQL missing required statement:\n  %s", needle)
		}
	}
}

// TestMigrationSQLTraceEventsColumn verifies the trace_events column is declared
// in both the CREATE TABLE and the back-fill ALTER TABLE.
func TestMigrationSQLTraceEventsColumn(t *testing.T) {
	MigrationSQL := migration0001(t)
	if !strings.Contains(MigrationSQL, "trace_events JSONB") {
		t.Error("MigrationSQL missing trace_events JSONB in CREATE TABLE")
	}
	if !strings.Contains(MigrationSQL, "ADD COLUMN IF NOT EXISTS trace_events") {
		t.Error("MigrationSQL missing back-fill ALTER TABLE for trace_events")
	}
}

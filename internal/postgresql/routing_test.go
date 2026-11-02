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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestUpsertRoutingEvents_BindsEveryColumn(t *testing.T) {
	s, mock := costMock(t)
	ttft := 120
	ev := RoutingEvent{
		DecisionID: "d-1", ParentDecisionID: "d-0", Tenant: "acme", Namespace: "ns", RunName: "run", Agent: "coder", Selector: "balanced",
		Strategy: "hybrid", Path: "rule:vision", Stage: "code", StageConf: 0.85, CategoryVersion: 1, WorkflowStep: "implement",
		Provider: "big", Model: "anthropic/claude", Candidates: []string{"big", "cheap"}, Confidence: 0.95,
		PromptTokens: 1000, CompletionTokens: 200, CostUSD: 0.012, AuxCostUSD: 0.001, LatencyMS: 900, TTFTMS: &ttft,
		FinishReason: "stop", ToolCalls: 2, ToolErrors: 1, ToolParseErrors: 1, TurnIndex: 4,
		CreatedAt: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
	}
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO routing_events`).
		WithArgs("d-1", "d-0", "acme", "ns", "run", "coder", "balanced",
			"hybrid", "", "rule:vision", "code", 0.85, 1, "",
			"implement", "big", "anthropic/claude", []byte(`["big","cheap"]`), nil, 0.95, false,
			1000, 200, 0.012, 0.001, 900, 120,
			"stop", "", "", 2, 1,
			1, false, "", 4, ev.CreatedAt).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := s.UpsertRoutingEvents(context.Background(), []RoutingEvent{ev}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertRoutingEvents_IsIdempotentBySQL(t *testing.T) {
	// Retried batches must not duplicate rows: the statement is an upsert on
	// decision_id that refreshes the outcome columns.
	for _, want := range []string{"ON CONFLICT (decision_id) DO UPDATE", "tool_errors = EXCLUDED.tool_errors", "cost_usd = EXCLUDED.cost_usd"} {
		if !strings.Contains(upsertRoutingEventSQL, want) {
			t.Errorf("upsert SQL lacks %q", want)
		}
	}
	if strings.Contains(upsertRoutingEventSQL, "created_at = EXCLUDED") {
		t.Errorf("a retry must keep the original created_at")
	}
}

func TestUpsertRoutingEvents_RejectsIncompleteEvents(t *testing.T) {
	s, mock := costMock(t)
	mock.ExpectBegin()
	mock.ExpectRollback()
	err := s.UpsertRoutingEvents(context.Background(), []RoutingEvent{{DecisionID: "d", Namespace: "ns", RunName: "r"}})
	if err == nil {
		t.Fatal("event without a provider was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertRoutingEvents_EmptyBatchTouchesNothing(t *testing.T) {
	s, mock := costMock(t)
	if err := s.UpsertRoutingEvents(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRoutingEvents(t *testing.T) {
	s, mock := costMock(t)
	cutoff := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`DELETE FROM routing_events WHERE created_at < \$1`).WithArgs(cutoff).WillReturnResult(sqlmock.NewResult(0, 42))
	n, err := s.PruneRoutingEvents(context.Background(), cutoff)
	if err != nil || n != 42 {
		t.Fatalf("pruned %d, %v; want 42", n, err)
	}
}

func TestRoutingEventsForRun(t *testing.T) {
	s, mock := costMock(t)
	cols := []string{"decision_id", "parent", "agent", "selector", "strategy", "path", "stage", "stage_conf", "provider", "model", "confidence",
		"prompt_tokens", "completion_tokens", "cost", "aux", "latency_ms", "ttft_ms", "finish_reason", "fallback_from", "error_class",
		"tool_calls", "tool_errors", "tool_parse_errors", "turn_index", "created_at"}
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM routing_events\s+WHERE namespace = \$1 AND run_name = \$2`).WithArgs("ns", "run", maxRunRoutingEvents).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("d-1", "", "coder", "bal", "hybrid", "rule:weighted", "chat", 0.4, "cheap", "m", 0.5, 10, 5, 0.001, 0.0, 300, 80, "stop", "", "", 0, 0, 0, 1, at).
			AddRow("d-2", "d-1", "coder", "bal", "hybrid", "fallback", "chat", 0.4, "big", "m2", 0.5, 0, 0, 0.0, 0.0, 50, nil, "", "cheap", "provider_error", 0, 0, 0, 2, at))

	got, err := s.RoutingEventsForRun(context.Background(), "ns", "run", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].TTFTMS == nil || *got[0].TTFTMS != 80 || got[1].TTFTMS != nil {
		t.Fatalf("events = %+v, want two with a TTFT only on the first", got)
	}
	if got[1].ParentDecisionID != "d-1" || got[1].ErrorClass != "provider_error" || got[1].RunName != "run" {
		t.Errorf("second event = %+v", got[1])
	}
}

func TestFailReasonClass(t *testing.T) {
	failed := func(mut func(*agentorcav1alpha1.AgentRunStatus)) *agentorcav1alpha1.AgentRun {
		r := &agentorcav1alpha1.AgentRun{}
		r.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
		mut(&r.Status)
		return r
	}
	tests := []struct {
		name string
		run  *agentorcav1alpha1.AgentRun
		want string
	}{
		{"succeeded has no class", &agentorcav1alpha1.AgentRun{Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded}}, ""},
		{"budget", failed(func(s *agentorcav1alpha1.AgentRunStatus) {
			s.Budget = &agentorcav1alpha1.RunBudgetStatus{State: agentorcav1alpha1.RunBudgetExceeded}
		}), failClassBudget},
		{"safeguard", failed(func(s *agentorcav1alpha1.AgentRunStatus) { s.LoopDetected = &agentorcav1alpha1.LoopDetectedInfo{} }), failClassSafeguard},
		{"timeout", failed(func(s *agentorcav1alpha1.AgentRunStatus) { s.FailureReason = "run Timed Out after 5m" }), failClassTimeout},
		{"other", failed(func(s *agentorcav1alpha1.AgentRunStatus) { s.FailureReason = "pod crashed" }), failClassError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failReasonClass(tt.run); got != tt.want {
				t.Errorf("class = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestArchiveRunRecordsOutcome(t *testing.T) {
	s, mock := costMock(t)
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"},
		Status: agentorcav1alpha1.AgentRunStatus{
			Phase: agentorcav1alpha1.AgentRunPhaseFailed, SpendUSD: "0.25", RestartCount: 2,
			Budget: &agentorcav1alpha1.RunBudgetStatus{State: agentorcav1alpha1.RunBudgetExceeded},
		},
	}
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO archived_runs`).WillReturnResult(sqlmock.NewResult(1, 1))
	// namespace, run, tenant, phase, class, cost, budget_exceeded, restarts, archived_at
	mock.ExpectExec(`INSERT INTO run_outcomes`).
		WithArgs("ns", "r", "", "Failed", failClassBudget, 0.25, true, 2, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := s.ArchiveRun(context.Background(), run, nil); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMigration0003CreatesRoutingTables(t *testing.T) {
	all, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, m := range all {
		if m.Version == 3 {
			sql = m.SQL
		}
	}
	for _, want := range []string{"CREATE TABLE routing_events", "decision_id        TEXT PRIMARY KEY", "CREATE TABLE run_outcomes", "PRIMARY KEY (namespace, run_name)"} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration 0003 lacks %q", want)
		}
	}
	if strings.Contains(strings.ToLower(sql), "prompt text") || strings.Contains(strings.ToLower(sql), "response text") {
		t.Errorf("routing_events must hold metadata only, never prompt or response content")
	}
}

func TestRunRetention_PrunesNowAndOnEachTick(t *testing.T) {
	var mu sync.Mutex
	var cutoffs []time.Time
	prune := func(_ context.Context, cutoff time.Time) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		cutoffs = append(cutoffs, cutoff)
		if len(cutoffs) == 1 {
			return 0, errors.New("transient") // a failed prune must not stop the loop
		}
		return 3, nil
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runRetention(ctx, prune, func() time.Time { return now }, 720*time.Hour, 10*time.Millisecond)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(cutoffs)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cutoffs) < 3 {
		t.Fatalf("pruned %d times, want an immediate run plus ticks (one of them after a failure)", len(cutoffs))
	}
	if want := now.Add(-720 * time.Hour); !cutoffs[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v", cutoffs[0], want)
	}
}

func TestRunRetention_ZeroKeepsEverything(t *testing.T) {
	prune := func(context.Context, time.Time) (int64, error) {
		t.Error("pruned although retention is 0")
		return 0, nil
	}
	if err := runRetention(context.Background(), prune, time.Now, 0, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

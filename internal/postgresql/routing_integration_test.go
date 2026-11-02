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
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const (
	itNamespace  = "ns"
	itRun        = "run-1"
	itFirst      = "d-1"
	itSecond     = "d-2"
	itStrong     = "strong"
	itWeak       = "weak"
	itSelector   = "balanced"
	itTTFTMillis = 85
)

// freshRoutingDB returns a store on a migrated, empty database named by
// AGENTORCA_TEST_POSTGRES_DSN (a throwaway one: its schema is dropped), or skips.
func freshRoutingDB(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("AGENTORCA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AGENTORCA_TEST_POSTGRES_DSN not set")
	}
	store, err := NewStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(context.Background(), `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyMigration(context.Background()); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return store, db
}

// routingFixtures are a served call and the fallback that followed it.
func routingFixtures() (first, second RoutingEvent) {
	ttft := itTTFTMillis
	first = RoutingEvent{
		DecisionID: itFirst, Tenant: "acme", Namespace: itNamespace, RunName: itRun, Agent: "coder", Selector: itSelector,
		Strategy: "rule-based", Path: "rule:weighted", Stage: "chat", StageConf: 0.4, CategoryVersion: 1,
		Provider: itStrong, Model: "openai/strong", Candidates: []string{itStrong, itWeak}, Confidence: 0.5,
		PromptTokens: 190, CompletionTokens: 3, CostUSD: 0.000615, LatencyMS: 410, TTFTMS: &ttft,
		FinishReason: "stop",
	}
	second = first
	second.DecisionID, second.ParentDecisionID, second.Provider, second.Path = itSecond, itFirst, itWeak, "fallback"
	second.CostUSD, second.AuxCostUSD, second.TTFTMS, second.FallbackFrom = 0.00002, 0.00001, nil, itStrong
	return first, second
}

// TestRoutingEventsAgainstPostgres runs the routing telemetry SQL on a real
// database. sqlmock checks which statements run and with what arguments, never
// that Postgres accepts them; this does. The steps share one database in order.
func TestRoutingEventsAgainstPostgres(t *testing.T) {
	store, db := freshRoutingDB(t)
	first, second := routingFixtures()

	t.Run("a retried batch is idempotent and completes what was sent early", func(t *testing.T) {
		upsertAndRetry(t, store, first, second)
	})
	t.Run("reads the events back", func(t *testing.T) { readsBack(t, store) })
	t.Run("cost by model counts each call once and honours the namespace filter", func(t *testing.T) {
		costByModel(t, store)
	})
	t.Run("archiving records the outcome from what is stored, and again updates it", func(t *testing.T) {
		archiveRecordsOutcome(t, store, db)
	})
	t.Run("pruning deletes what is older than the cutoff", func(t *testing.T) { prunes(t, store) })
}

func upsertAndRetry(t *testing.T, store *Store, first, second RoutingEvent) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertRoutingEvents(ctx, []RoutingEvent{first, second}); err != nil {
		t.Fatalf("UpsertRoutingEvents: %v", err)
	}
	completed := first
	completed.ToolCalls, completed.ToolErrors = 2, 1
	if err := store.UpsertRoutingEvents(ctx, []RoutingEvent{completed}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func readsBack(t *testing.T, store *Store) {
	t.Helper()
	got, err := store.RoutingEventsForRun(context.Background(), itNamespace, itRun, 0)
	if err != nil {
		t.Fatalf("RoutingEventsForRun: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events after a retry, want 2: %+v", len(got), got)
	}
	byID := map[string]RoutingEvent{got[0].DecisionID: got[0], got[1].DecisionID: got[1]}
	d1, d2 := byID[itFirst], byID[itSecond]
	if d1.ToolCalls != 2 || d1.ToolErrors != 1 {
		t.Errorf("the retry did not complete the event: %+v", d1)
	}
	if d1.TTFTMS == nil || *d1.TTFTMS != itTTFTMillis || d1.CostUSD < 0.000614 || d1.CostUSD > 0.000616 || d1.PromptTokens != 190 {
		t.Errorf("%s = %+v", itFirst, d1)
	}
	if d2.ParentDecisionID != itFirst || d2.TTFTMS != nil || d2.FallbackFrom != itStrong || d2.AuxCostUSD < 0.0000099 {
		t.Errorf("%s = %+v", itSecond, d2)
	}
}

func costByModel(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	costs, err := store.CostByModel(ctx, []string{itNamespace}, since)
	if err != nil {
		t.Fatalf("CostByModel: %v", err)
	}
	if len(costs) != 2 || costs[0].Provider != itStrong || costs[0].Calls != 1 {
		t.Errorf("costs = %+v", costs)
	}
	if other, err := store.CostByModel(ctx, []string{"elsewhere"}, since); err != nil || len(other) != 0 {
		t.Errorf("another namespace's costs = %+v, %v; want none", other, err)
	}
	if all, err := store.CostByModel(ctx, nil, since); err != nil || len(all) != 2 {
		t.Errorf("all namespaces = %+v, %v", all, err)
	}
}

func archiveRecordsOutcome(t *testing.T, store *Store, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: itRun, Namespace: itNamespace, Labels: map[string]string{"agentorca.io/tenant": "acme"}},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "coder"},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded, SpendUSD: "0.0006"},
	}
	for range 2 {
		if err := store.ArchiveRun(ctx, run, nil); err != nil {
			t.Fatalf("ArchiveRun: %v", err)
		}
	}
	var phase, selector string
	var turns int
	var cost float64
	if err := db.QueryRowContext(ctx, `SELECT phase, COALESCE(selector, ''), turns, total_cost_usd::float8
		FROM run_outcomes WHERE namespace = $1 AND run_name = $2`, itNamespace, itRun).Scan(&phase, &selector, &turns, &cost); err != nil {
		t.Fatalf("reading the outcome: %v", err)
	}
	if phase != "Succeeded" || selector != itSelector || turns != 2 || cost < 0.00059 || cost > 0.00061 {
		t.Errorf("outcome = %s/%s/%d turns/%v", phase, selector, turns, cost)
	}
}

func prunes(t *testing.T, store *Store) {
	t.Helper()
	n, err := store.PruneRoutingEvents(context.Background(), time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Errorf("pruned %d, %v; want 2", n, err)
	}
}

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
	"testing"
	"time"
)

const (
	itAgent      = "coder"
	itBenchmark  = "bench"
	itDeployment = "assistant"
)

// shadowIT runs the shadow and feedback SQL on a real database, in order, on
// one database. sqlmock checks which statements run, never that Postgres
// accepts them; this does.
type shadowIT struct {
	store *Store
	ctx   context.Context
	now   time.Time
}

func TestShadowAndFeedbackAgainstPostgres(t *testing.T) {
	store, _ := freshRoutingDB(t)
	it := &shadowIT{store: store, ctx: context.Background(), now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	t.Run("a result is stored once per decision and arm, and a retry refreshes it", it.upsertRefreshes)
	t.Run("a deployment and a run of one Agent tally together, apart from another Agent", it.talliesPerAgent)
	t.Run("a result needs a known source kind", it.sourceKindChecked)
	t.Run("the day cap counts settled spend and live leases", it.dayCap)
	t.Run("releasing a lease frees its budget", it.releaseFreesBudget)
	t.Run("the hourly cap is per Agent across runs and deployments", it.hourlyCap)
	t.Run("a deployment's per-run cap counts the day, not all time", it.deploymentRunCap)
	t.Run("feedback is filed under the Agent and a second rating replaces the first", it.feedbackPerAgent)
}

func (it *shadowIT) result(decision, source, kind, winner string, cost float64) ShadowResult {
	return ShadowResult{
		DecisionID: decision, Namespace: itNamespace, Agent: itAgent, SourceKind: kind, SourceName: source,
		Benchmark: itBenchmark, Selector: itSelector, SelectorGeneration: 3, Stage: "chat",
		ServedProvider: itStrong, ShadowProvider: itWeak, Winner: winner,
		ShadowCostUSD: cost, JudgeCostUSD: cost, CreatedAt: it.now.Add(-time.Minute),
	}
}

func (it *shadowIT) lease(agent, kind, source string, amount, perDay, perRun float64, maxPerHour int) (int64, error) {
	return it.store.TakeShadowLease(it.ctx, LeaseRequest{
		Namespace: itNamespace, Benchmark: itBenchmark, Agent: agent, SourceKind: kind, SourceName: source,
		AmountUSD: amount, DayStart: it.now.Truncate(24 * time.Hour), PerDayUSD: perDay, PerRunUSD: perRun,
		MaxPerHour: maxPerHour, TTL: 10 * time.Minute, Now: it.now,
	})
}

func (it *shadowIT) upsertRefreshes(t *testing.T) {
	r := it.result("d-1", itRun, SourceKindRun, ShadowWinnerShadow, 0.01)
	if err := it.store.UpsertShadowResult(it.ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Winner, r.Inconsistent = ShadowWinnerTie, true
	if err := it.store.UpsertShadowResult(it.ctx, r); err != nil {
		t.Fatal(err)
	}
	tallies, err := it.store.ShadowTallies(it.ctx, itNamespace, itBenchmark, it.now.Add(-time.Hour))
	if err != nil || len(tallies) != 1 || tallies[0].Ties != 1 || tallies[0].Shadow != 0 || tallies[0].Inconsistent != 1 {
		t.Fatalf("tallies = %+v, %v; want one tie from the retry", tallies, err)
	}
}

func (it *shadowIT) talliesPerAgent(t *testing.T) {
	if err := it.store.UpsertShadowResult(it.ctx, it.result("d-2", itDeployment, SourceKindDeployment, ShadowWinnerServed, 0.01)); err != nil {
		t.Fatal(err)
	}
	other := it.result("d-3", itRun, SourceKindRun, ShadowWinnerShadow, 0.01)
	other.Agent = "writer"
	if err := it.store.UpsertShadowResult(it.ctx, other); err != nil {
		t.Fatal(err)
	}
	tallies, err := it.store.ShadowTallies(it.ctx, itNamespace, itBenchmark, it.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	byAgent := map[string]ShadowTally{}
	for _, tl := range tallies {
		byAgent[tl.Agent] = tl
	}
	if c := byAgent[itAgent]; c.Ties != 1 || c.Served != 1 {
		t.Errorf("coder = %+v, want its run's tie and its deployment's served win", c)
	}
	if w := byAgent["writer"]; w.Shadow != 1 || w.Served != 0 {
		t.Errorf("writer = %+v, want only its own result", w)
	}
}

func (it *shadowIT) sourceKindChecked(t *testing.T) {
	if _, err := it.store.db.ExecContext(it.ctx, `UPDATE shadow_results SET source_kind = 'Job' WHERE decision_id = 'd-1'`); err == nil {
		t.Error("the table accepted a source kind it should reject")
	}
}

func (it *shadowIT) dayCap(t *testing.T) {
	// 0.06 is settled so far (three results of 0.01 shadow + 0.01 judge).
	if _, err := it.lease(itAgent, SourceKindRun, "r", 0.5, 1, 0, 0); err != nil {
		t.Fatalf("a lease within the day cap was refused: %v", err)
	}
	if _, err := it.lease(itAgent, SourceKindRun, "r", 0.5, 1, 0, 0); !errors.Is(err, ErrLeaseDenied) {
		t.Errorf("err = %v, want ErrLeaseDenied once settled spend and the live lease reach the cap", err)
	}
}

func (it *shadowIT) releaseFreesBudget(t *testing.T) {
	id, err := it.lease("writer", SourceKindRun, "r2", 0.3, 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := it.store.ReleaseShadowLease(it.ctx, id); err != nil {
		t.Fatal(err)
	}
	spend, err := it.store.ShadowSpendSince(it.ctx, itNamespace, itBenchmark, it.now.Truncate(24*time.Hour), it.now)
	if err != nil || spend.LeasedUSD != 0.5 {
		t.Errorf("spend = %+v, %v; want only the first 0.5 lease held", spend, err)
	}
}

func (it *shadowIT) hourlyCap(t *testing.T) {
	// coder already has two sampled calls in the hour (d-1, d-2) and one live lease.
	if _, err := it.lease(itAgent, SourceKindDeployment, itDeployment, 0.001, 100, 0, 3); !errors.Is(err, ErrLeaseDenied) {
		t.Errorf("err = %v, want ErrLeaseDenied at 3 calls (2 settled, 1 leased)", err)
	}
	if _, err := it.lease("writer", SourceKindDeployment, itDeployment, 0.001, 100, 0, 3); err != nil {
		t.Errorf("another Agent was refused: %v", err)
	}
}

func (it *shadowIT) deploymentRunCap(t *testing.T) {
	old := it.result("d-old", "old-deploy", SourceKindDeployment, ShadowWinnerTie, 0.20)
	old.CreatedAt = it.now.Add(-48 * time.Hour)
	if err := it.store.UpsertShadowResult(it.ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := it.lease(itAgent, SourceKindDeployment, "old-deploy", 0.1, 100, 0.2, 0); err != nil {
		t.Errorf("a deployment's spend from two days ago counted against today: %v", err)
	}
	old.DecisionID, old.SourceKind, old.SourceName = "d-old-run", SourceKindRun, "old-run"
	if err := it.store.UpsertShadowResult(it.ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := it.lease(itAgent, SourceKindRun, "old-run", 0.1, 100, 0.2, 0); !errors.Is(err, ErrLeaseDenied) {
		t.Errorf("err = %v, want a run's earlier spend to count in full", err)
	}
}

func (it *shadowIT) feedbackPerAgent(t *testing.T) {
	f := Feedback{
		Namespace: itNamespace, RunName: itRun, Agent: itAgent, Workflow: "release", WorkflowStep: "review",
		DecisionID: "d-1", Rating: -1, UserID: "ada", Source: FeedbackSourceUI,
	}
	if err := it.store.UpsertFeedback(it.ctx, f); err != nil {
		t.Fatal(err)
	}
	f.Rating, f.Comment = 1, "fine after all"
	if err := it.store.UpsertFeedback(it.ctx, f); err != nil {
		t.Fatal(err)
	}
	got, err := it.store.FeedbackForRun(it.ctx, itNamespace, itRun, "ada")
	if err != nil || len(got) != 1 {
		t.Fatalf("feedback = %+v, %v; want one rating", got, err)
	}
	if g := got[0]; g.Rating != 1 || g.Agent != itAgent || g.Workflow != "release" || g.WorkflowStep != "review" || g.Comment != "fine after all" {
		t.Errorf("feedback = %+v", g)
	}
}

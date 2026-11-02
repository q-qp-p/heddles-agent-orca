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
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/routingbench"
)

// itCategory is the category of the stage the fixtures use.
const itCategory = "stage:chat"

// TestLoadObservationsAgainstPostgres runs the evidence queries on a real
// database: sqlmock checks which statements run, never that Postgres accepts
// them. It builds a small history for two Agents and checks that each query
// keeps what it should and drops the rest.
func TestLoadObservationsAgainstPostgres(t *testing.T) {
	store, _ := freshRoutingDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedObservationHistory(t, store, now)

	got, err := store.LoadObservations(ctx, routingbench.Query{Namespace: itNamespace, Agents: []string{itAgent}, Since: now.Add(-time.Hour), Now: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("samples are the judged replays of the Agent's own served calls", func(t *testing.T) { checkSamples(t, got) })
	t.Run("volumes count served, successful calls with a category", func(t *testing.T) { checkVolumes(t, got) })
	t.Run("provider stats count attempts and failures per selector", func(t *testing.T) { checkProviderStats(t, got) })
	t.Run("ratings are the decision-level ones, per category", func(t *testing.T) { checkRatings(t, got) })
	t.Run("no Agents loads nothing", func(t *testing.T) {
		empty, err := store.LoadObservations(ctx, routingbench.Query{Namespace: itNamespace, Since: now.Add(-time.Hour), Now: now})
		if err != nil || len(empty.Samples) != 0 || len(empty.Volumes) != 0 || len(empty.ProviderStats) != 0 {
			t.Errorf("observations = %+v, %v", empty, err)
		}
	})
}

// seedObservationHistory stores the served calls, replays and ratings the
// checks below describe.
func seedObservationHistory(t *testing.T, store *Store, now time.Time) {
	t.Helper()
	ctx := context.Background()
	at := now.Add(-10 * time.Minute)

	event := func(id, agent, provider string, cost float64, mutate func(*RoutingEvent)) RoutingEvent {
		e := RoutingEvent{
			DecisionID: id, Namespace: itNamespace, RunName: "run-" + id, Agent: agent, Selector: itSelector,
			Strategy: "switchyard", Algorithm: "composite", Path: "switchyard", Stage: "chat",
			Provider: provider, Confidence: 0.4, CostUSD: cost, CreatedAt: at,
		}
		if mutate != nil {
			mutate(&e)
		}
		return e
	}
	events := []RoutingEvent{
		event("ev-1", itAgent, itStrong, 0.010, nil),
		event("ev-2", itAgent, itWeak, 0.001, func(e *RoutingEvent) { e.Confidence = 0.8 }),
		event("ev-3", itAgent, itWeak, 0, func(e *RoutingEvent) { e.ErrorClass = "rate_limit" }),
		event("ev-4", itAgent, itWeak, 0.001, func(e *RoutingEvent) { e.IsShadow = true }),
		event("ev-5", "writer", itStrong, 0.010, nil),
		event("ev-6", itAgent, itStrong, 0.010, func(e *RoutingEvent) { e.Stage = "" }), // no category
		event("ev-7", itAgent, itStrong, 0.010, func(e *RoutingEvent) { e.CreatedAt = now.Add(-48 * time.Hour) }),
	}
	if err := store.UpsertRoutingEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	shadow := func(decision, agent, served, shadowProv, winner string, mutate func(*ShadowResult)) ShadowResult {
		r := ShadowResult{
			DecisionID: decision, Namespace: itNamespace, Agent: agent, SourceKind: SourceKindRun, SourceName: "run-" + decision,
			Benchmark: itBenchmark, Selector: itSelector, Stage: "chat", ServedProvider: served, ShadowProvider: shadowProv,
			Winner: winner, ShadowCostUSD: 0.002, CreatedAt: at,
		}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}
	for _, r := range []ShadowResult{
		shadow("ev-1", itAgent, itStrong, itWeak, ShadowWinnerServed, nil),
		shadow("ev-2", itAgent, itWeak, itStrong, ShadowWinnerShadow, func(r *ShadowResult) { r.Inconsistent = true }),
		shadow("ev-3", itAgent, itWeak, itStrong, ShadowWinnerInvalid, nil),                                          // unusable verdict
		shadow("ev-4", itAgent, itWeak, itStrong, ShadowWinnerTie, func(r *ShadowResult) { r.ShadowError = "boom" }), // the replay failed
		shadow("ev-5", "writer", itStrong, itWeak, ShadowWinnerTie, nil),                                             // another Agent
		shadow("orphan", itAgent, itStrong, itWeak, ShadowWinnerTie, nil),                                            // served call aged out of routing_events
	} {
		if err := store.UpsertShadowResult(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	for _, f := range []Feedback{
		{Namespace: itNamespace, RunName: "run-ev-1", Agent: itAgent, DecisionID: "ev-1", Rating: -1, UserID: "ada", Source: FeedbackSourceUI},
		{Namespace: itNamespace, RunName: "run-ev-2", Agent: itAgent, DecisionID: "ev-2", Rating: 1, UserID: "ada", Source: FeedbackSourceUI},
		{Namespace: itNamespace, RunName: "run-ev-2", Agent: itAgent, Rating: -1, UserID: "ada", Source: FeedbackSourceUI}, // a rating of the whole run has no category
		{Namespace: itNamespace, RunName: "run-ev-5", Agent: "writer", DecisionID: "ev-5", Rating: 1, UserID: "ada", Source: FeedbackSourceUI},
	} {
		if err := store.UpsertFeedback(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
}

func checkSamples(t *testing.T, got routingbench.Observations) {
	t.Helper()
	if len(got.Samples) != 2 {
		t.Fatalf("samples = %+v, want ev-1 and ev-2 only", got.Samples)
	}
	first, second := got.Samples[0], got.Samples[1]
	if first.Category != itCategory || first.ServedProvider != itStrong || first.Winner != "served" ||
		first.ServedCostUSD != 0.010 || first.ShadowCostUSD != 0.002 || first.Algorithm != "composite" || first.Confidence != 0.4 {
		t.Errorf("first = %+v", first)
	}
	if second.Winner != "shadow" || !second.Inconsistent || second.Confidence != 0.8 || second.ServedCostUSD != 0.001 {
		t.Errorf("second = %+v", second)
	}
}

func checkVolumes(t *testing.T, got routingbench.Observations) {
	t.Helper()
	counts := map[string]int{}
	for _, v := range got.Volumes {
		counts[v.Agent+"/"+v.Category+"/"+v.Provider] += v.Calls
	}
	// ev-1 strong, ev-2 weak. Not counted: ev-3 (error), ev-4 (shadow), ev-5
	// (another Agent), ev-6 (no stage), ev-7 (before the window).
	if len(counts) != 2 || counts[itAgent+"/"+itCategory+"/"+itStrong] != 1 || counts[itAgent+"/"+itCategory+"/"+itWeak] != 1 {
		t.Errorf("volumes = %v", counts)
	}
}

func checkProviderStats(t *testing.T, got routingbench.Observations) {
	t.Helper()
	stats := map[string]routingbench.ProviderStat{}
	for _, s := range got.ProviderStats {
		stats[s.Selector+"/"+s.Provider] = s
	}
	// weak: ev-2 and ev-3 (a failure); ev-4 is a shadow call and is not counted.
	if w := stats[itSelector+"/"+itWeak]; w.Calls != 2 || w.Errors != 1 {
		t.Errorf("weak = %+v, want 2 calls and 1 error", w)
	}
	// strong: ev-1 and ev-6 (the old ev-7 is outside the window).
	if s := stats[itSelector+"/"+itStrong]; s.Calls != 2 || s.Errors != 0 {
		t.Errorf("strong = %+v, want 2 calls and no errors", s)
	}
}

func checkRatings(t *testing.T, got routingbench.Observations) {
	t.Helper()
	r := got.Ratings[routingbench.AgentCategory{Agent: itAgent, Category: itCategory}]
	if len(got.Ratings) != 1 || r.Up != 1 || r.Down != 1 {
		t.Errorf("ratings = %+v, want one thumbs up and one down on chat, the run-level rating ignored", got.Ratings)
	}
}

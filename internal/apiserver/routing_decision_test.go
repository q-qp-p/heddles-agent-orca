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

package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const routingTestNS = "default"

func routingTestServer(t *testing.T, run *agentorcav1alpha1.AgentRun) *Server {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).
		WithObjects(run).WithStatusSubresource(&agentorcav1alpha1.AgentRun{}).Build()
	return &Server{crdClient: cl}
}

func postRoutingDecision(s *Server, name, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/agentrun/"+routingTestNS+"/"+name+"/route", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.recordRoutingDecision(rec, req, routingTestNS, name)
	return rec.Code
}

func getRun(t *testing.T, s *Server, name string) agentorcav1alpha1.AgentRun {
	t.Helper()
	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(context.Background(), client.ObjectKey{Namespace: routingTestNS, Name: name}, &run); err != nil {
		t.Fatalf("get run: %v", err)
	}
	return run
}

func decisionBody(id string) string {
	return fmt.Sprintf(`{"provider":"p","model":"m","strategy":"hybrid","reason":"r","confidence":"0.50",`+
		`"decisionID":%q,"path":"rule:weighted","category":"chat","parentDecisionID":"parent-1"}`, id)
}

func TestRecordRoutingDecision_StoresDecisionFields(t *testing.T) {
	s := routingTestServer(t, &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: routingTestNS}})

	if code := postRoutingDecision(s, "r1", decisionBody("d-1")); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	run := getRun(t, s, "r1")
	if len(run.Status.RoutingDecisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(run.Status.RoutingDecisions))
	}
	got := run.Status.RoutingDecisions[0]
	if got.DecisionID != "d-1" || got.Path != "rule:weighted" || got.Category != "chat" || got.ParentDecisionID != "parent-1" {
		t.Errorf("decision fields not stored: %+v", got)
	}
	if got.Timestamp == nil {
		t.Errorf("timestamp not set")
	}
	if run.Status.RoutingDecisionsTotal != 1 {
		t.Errorf("total = %d, want 1", run.Status.RoutingDecisionsTotal)
	}
}

func TestRecordRoutingDecision_CapsListButCountsAll(t *testing.T) {
	// Seed past the cap, as a long run would have: the controller's own
	// "configured provider" entries are not in the total, which must still
	// never undercount what is held.
	seed := make([]agentorcav1alpha1.RoutingDecision, maxRoutingDecisions)
	for i := range seed {
		seed[i] = agentorcav1alpha1.RoutingDecision{Provider: "p", DecisionID: fmt.Sprintf("seed-%d", i)}
	}
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: routingTestNS}}
	run.Status.RoutingDecisions = seed
	s := routingTestServer(t, run)

	for i := range 3 {
		if code := postRoutingDecision(s, "r2", decisionBody(fmt.Sprintf("new-%d", i))); code != http.StatusNoContent {
			t.Fatalf("status = %d", code)
		}
	}
	got := getRun(t, s, "r2")
	if len(got.Status.RoutingDecisions) != maxRoutingDecisions {
		t.Fatalf("held = %d, want cap %d", len(got.Status.RoutingDecisions), maxRoutingDecisions)
	}
	if first := got.Status.RoutingDecisions[0].DecisionID; first != "seed-3" {
		t.Errorf("oldest kept = %q, want seed-3 (three oldest dropped)", first)
	}
	if last := got.Status.RoutingDecisions[maxRoutingDecisions-1].DecisionID; last != "new-2" {
		t.Errorf("newest = %q, want new-2", last)
	}
	if got.Status.RoutingDecisionsTotal != maxRoutingDecisions+3 {
		t.Errorf("total = %d, want %d", got.Status.RoutingDecisionsTotal, maxRoutingDecisions+3)
	}
}

func TestRecordRoutingDecision_ConcurrentPostsLoseNothing(t *testing.T) {
	s := routingTestServer(t, &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r3", Namespace: routingTestNS}})

	const writers = 8
	var wg sync.WaitGroup
	codes := make([]int, writers)
	for i := range writers {
		wg.Go(func() {
			codes[i] = postRoutingDecision(s, "r3", decisionBody(fmt.Sprintf("c-%d", i)))
		})
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusNoContent {
			t.Errorf("writer %d: status = %d, want 204", i, code)
		}
	}
	run := getRun(t, s, "r3")
	seen := map[string]bool{}
	for _, d := range run.Status.RoutingDecisions {
		seen[d.DecisionID] = true
	}
	if len(seen) != writers {
		t.Errorf("recorded %d distinct decisions, want %d: a concurrent post was lost", len(seen), writers)
	}
	if run.Status.RoutingDecisionsTotal != writers {
		t.Errorf("total = %d, want %d", run.Status.RoutingDecisionsTotal, writers)
	}
}

func TestRecordRoutingDecision_Errors(t *testing.T) {
	s := routingTestServer(t, &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r4", Namespace: routingTestNS}})

	if code := postRoutingDecision(s, "r4", `{"model":"m"}`); code != http.StatusBadRequest {
		t.Errorf("missing provider: status = %d, want 400", code)
	}
	if code := postRoutingDecision(s, "r4", `not json`); code != http.StatusBadRequest {
		t.Errorf("bad json: status = %d, want 400", code)
	}
	if code := postRoutingDecision(s, "missing", decisionBody("d")); code != http.StatusNotFound {
		t.Errorf("unknown run: status = %d, want 404", code)
	}
}

func TestRoutingDecisionsSince(t *testing.T) {
	mk := func(total int, ids ...string) *agentorcav1alpha1.AgentRun {
		run := &agentorcav1alpha1.AgentRun{}
		run.Status.RoutingDecisionsTotal = total
		for _, id := range ids {
			run.Status.RoutingDecisions = append(run.Status.RoutingDecisions, agentorcav1alpha1.RoutingDecision{DecisionID: id})
		}
		return run
	}
	ids := func(ds []agentorcav1alpha1.RoutingDecision) string {
		var out strings.Builder
		for _, d := range ds {
			out.WriteString(d.DecisionID)
		}
		return out.String()
	}
	tests := []struct {
		name     string
		run      *agentorcav1alpha1.AgentRun
		emitted  int
		wantIDs  string
		wantNext int
	}{
		{"legacy status without a total emits everything once", mk(0, "a", "b"), 0, "ab", 2},
		{"legacy status resumes by position", mk(0, "a", "b", "c"), 2, "c", 3},
		{"nothing new", mk(3, "a", "b", "c"), 3, "", 3},
		{"total ahead of the list: rolled-off decisions are skipped, not replayed", mk(10, "h", "i", "j"), 8, "ij", 10},
		{"a full capped list still yields new decisions", mk(103, "x", "y", "z"), 102, "z", 103},
		{"a reader further behind than the window starts at the window", mk(50, "p", "q"), 0, "pq", 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, next := routingDecisionsSince(tt.run, tt.emitted)
			if ids(got) != tt.wantIDs || next != tt.wantNext {
				t.Errorf("got %q next %d, want %q next %d", ids(got), next, tt.wantIDs, tt.wantNext)
			}
		})
	}
}

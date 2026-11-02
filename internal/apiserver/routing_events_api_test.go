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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/router"
)

type fakeRoutingStore struct {
	got []postgresql.RoutingEvent
	err error
}

func (f *fakeRoutingStore) UpsertRoutingEvents(_ context.Context, ev []postgresql.RoutingEvent) error {
	f.got = append(f.got, ev...)
	return f.err
}

const (
	// routingTestRun is the run the routing event tests post for.
	routingTestRun     = "run-1"
	routingCallerOK    = "system:serviceaccount:" + defaultNamespace + ":agent-sa"
	routingCallerOther = "system:serviceaccount:other:agent-sa"
)

func routingEventsServer(t *testing.T, store RoutingEventStore) *Server {
	t.Helper()
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Name: routingTestRun, Namespace: defaultNamespace, Labels: map[string]string{"agentorca.io/tenant": "acme"},
	}}
	s := &Server{crdClient: fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(run).Build()}
	return s.WithRoutingStore(store)
}

func postRoutingEvents(s *Server, caller, name, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/agentrun/"+defaultNamespace+"/"+name+"/routing-events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.recordRoutingEvents(rec, req, caller, defaultNamespace, name)
	return rec.Code
}

func TestRecordRoutingEvents_StoresWithServerSideAttribution(t *testing.T) {
	store := &fakeRoutingStore{}
	s := routingEventsServer(t, store)

	// The body tries to claim another namespace, run and tenant: none of it counts.
	body := `{"events":[{"decisionID":"d-1","provider":"p","namespace":"victim","runName":"other","tenant":"evil","costUSD":0.5}]}`
	if code := postRoutingEvents(s, routingCallerOK, routingTestRun, body); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	if len(store.got) != 1 {
		t.Fatalf("stored %d events, want 1", len(store.got))
	}
	e := store.got[0]
	if e.Namespace != defaultNamespace || e.RunName != routingTestRun || e.Tenant != "acme" {
		t.Errorf("attribution = %q/%q/%q, want default/run-1/acme from the URL and the run", e.Namespace, e.RunName, e.Tenant)
	}
	if e.CostUSD != 0.5 || e.CreatedAt.IsZero() {
		t.Errorf("event = %+v, want cost kept and created_at defaulted", e)
	}
}

func TestRecordRoutingEvents_RejectsOtherNamespaces(t *testing.T) {
	store := &fakeRoutingStore{}
	s := routingEventsServer(t, store)
	if code := postRoutingEvents(s, routingCallerOther, routingTestRun, `{"events":[{"decisionID":"d","provider":"p"}]}`); code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a router from another namespace", code)
	}
	if len(store.got) != 0 {
		t.Errorf("stored %d events from a forbidden caller", len(store.got))
	}
}

func TestRecordRoutingEvents_NoStoreAcknowledgesAndDrops(t *testing.T) {
	s := routingEventsServer(t, nil)
	if code := postRoutingEvents(s, routingCallerOK, routingTestRun, `{"events":[{"decisionID":"d","provider":"p"}]}`); code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 without a store", code)
	}
}

func TestRecordRoutingEvents_Validation(t *testing.T) {
	s := routingEventsServer(t, &fakeRoutingStore{})
	many := `{"events":[` + strings.Repeat(`{"decisionID":"d","provider":"p"},`, maxRoutingEventsPerBatch) + `{"decisionID":"d","provider":"p"}]}`
	tests := []struct {
		name string
		run  string
		body string
		want int
	}{
		{"not json", routingTestRun, `nope`, http.StatusBadRequest},
		{"missing decision id", routingTestRun, `{"events":[{"provider":"p"}]}`, http.StatusBadRequest},
		{"missing provider", routingTestRun, `{"events":[{"decisionID":"d"}]}`, http.StatusBadRequest},
		{"too many events", routingTestRun, many, http.StatusRequestEntityTooLarge},
		{"unknown run", "ghost", `{"events":[{"decisionID":"d","provider":"p"}]}`, http.StatusNotFound},
		{"empty batch", routingTestRun, `{"events":[]}`, http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := postRoutingEvents(s, routingCallerOK, tt.run, tt.body); code != tt.want {
				t.Errorf("status = %d, want %d", code, tt.want)
			}
		})
	}
}

func TestRecordRoutingEvents_StoreFailureIs500(t *testing.T) {
	s := routingEventsServer(t, &fakeRoutingStore{err: errors.New("db down")})
	if code := postRoutingEvents(s, routingCallerOK, routingTestRun, `{"events":[{"decisionID":"d","provider":"p"}]}`); code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 so the router retries", code)
	}
}

// The router and the operator define the event separately (the router image
// must not link the database package). This pins them to one wire format.
func TestRoutingEventWireFormatMatchesRouter(t *testing.T) {
	ttft := 80
	sent := router.RoutingEvent{
		DecisionID: "d-1", ParentDecisionID: "d-0", Agent: "coder", Selector: "bal", Strategy: "hybrid", Path: "rule:vision",
		Stage: "code", StageConf: 0.85, CategoryVersion: 1, WorkflowStep: "step", Provider: "p", Model: "m",
		Candidates: []string{"p", "q"}, Confidence: 0.9, PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.01, AuxCostUSD: 0.002,
		LatencyMS: 300, TTFTMS: &ttft, FinishReason: "stop", FallbackFrom: "x", ErrorClass: "provider_error",
		ToolCalls: 3, ToolErrors: 2, ToolParseErrors: 1, GuardrailBlocked: true, SafeguardTrip: "repeat", TurnIndex: 7,
		Signals: json.RawMessage(`{"k":1}`),
	}
	raw, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var got postgresql.RoutingEvent
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	back, _ := json.Marshal(got)
	// Re-marshalling the operator's view must reproduce the router's bytes: no
	// field is renamed, dropped or retyped across the boundary.
	var a, b map[string]any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(back, &b)
	delete(b, "createdAt")
	delete(a, "createdAt")
	if len(a) != len(b) {
		t.Fatalf("router sent %d fields, operator kept %d:\nrouter:   %s\noperator: %s", len(a), len(b), raw, back)
	}
	for k, v := range a {
		if bj, _ := json.Marshal(b[k]); string(bj) != jsonOfValue(v) {
			t.Errorf("field %q: router %s, operator %s", k, jsonOfValue(v), bj)
		}
	}
}

func jsonOfValue(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

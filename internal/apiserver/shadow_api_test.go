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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/router"
)

type fakeShadowStore struct {
	results  []postgresql.ShadowResult
	leased   float64
	cap      float64
	released []int64
	req      postgresql.LeaseRequest
}

func (f *fakeShadowStore) UpsertShadowResult(_ context.Context, r postgresql.ShadowResult) error {
	f.results = append(f.results, r)
	return nil
}

func (f *fakeShadowStore) TakeShadowLease(_ context.Context, req postgresql.LeaseRequest) (int64, error) {
	f.req = req
	if f.leased+req.AmountUSD > f.cap {
		return 0, postgresql.ErrLeaseDenied
	}
	f.leased += req.AmountUSD
	return 9, nil
}

func (f *fakeShadowStore) ReleaseShadowLease(_ context.Context, id int64) error {
	f.released = append(f.released, id)
	return nil
}

const (
	shadowRun        = "run"
	shadowDeployment = "assistant"
	shadowTenant     = "acme"
	shadowAgent      = "coder"
	shadowStray      = "stray"
)

const shadowCaller = "system:serviceaccount:" + routingTestNS + ":runner"

// shadowServer builds a cluster with an Agent the benchmark covers, run on as
// an AgentRun and as an AgentDeployment, and an Agent it does not cover.
func shadowServer(t *testing.T, storeTranscripts bool, store ShadowStore) *Server {
	t.Helper()
	tenant := map[string]string{"agentorca.io/tenant": shadowTenant}
	objs := []client.Object{
		&agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: shadowAgent, Namespace: routingTestNS, Labels: map[string]string{"team": "a"}},
			Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "balanced"},
		},
		&agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "outsider", Namespace: routingTestNS, Labels: map[string]string{"team": "b"}},
			Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "balanced"},
		},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "balanced", Namespace: routingTestNS, Generation: 4}},
		&agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: shadowRun, Namespace: routingTestNS, Labels: tenant},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: shadowAgent},
		},
		&agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: shadowDeployment, Namespace: routingTestNS, Labels: tenant},
			Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: shadowAgent},
		},
		&agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: shadowStray, Namespace: routingTestNS},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "outsider"},
		},
		&agentorcav1alpha1.RoutingBenchmark{
			ObjectMeta: metav1.ObjectMeta{Name: "bench", Namespace: routingTestNS},
			Spec: agentorcav1alpha1.RoutingBenchmarkSpec{
				AgentSelector:    &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
				StoreTranscripts: storeTranscripts,
				Shadow:           &agentorcav1alpha1.ShadowSpec{MaxPerHour: 7},
				Budget:           agentorcav1alpha1.BenchmarkBudget{PerDayUSD: "1.00", PerRunUSD: "0.25"},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(objs...).Build()
	s := &Server{crdClient: cl}
	if store != nil {
		s.WithShadowStore(store)
	}
	return s
}

func shadowPost(h func(http.ResponseWriter, *http.Request, string, string, string), caller, body string) *httptest.ResponseRecorder {
	return shadowPostTo(h, caller, shadowRun, body)
}

func shadowPostTo(h func(http.ResponseWriter, *http.Request, string, string, string), caller, source, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req, caller, routingTestNS, source)
	return w
}

const resultBody = `{"leaseId":9,"result":{"decisionId":"d-1","benchmark":"bench","servedProvider":"strong",
"shadowProvider":"weak","winner":"tie","namespace":"evil","agent":"outsider","sourceName":"other","tenant":"evil",
"selector":"evil","transcript":{"a":1}}}`

func TestRecordShadowResult_TakesIdentityFromTheClusterNotTheBody(t *testing.T) {
	store := &fakeShadowStore{}
	s := shadowServer(t, false, store)
	w := shadowPost(s.recordShadowResult, shadowCaller, resultBody)
	if w.Code != http.StatusNoContent || len(store.results) != 1 {
		t.Fatalf("code = %d, results = %d", w.Code, len(store.results))
	}
	got := store.results[0]
	if got.Namespace != routingTestNS || got.Agent != shadowAgent || got.SourceKind != postgresql.SourceKindRun ||
		got.SourceName != shadowRun || got.Tenant != shadowTenant || got.Selector != "balanced" || got.SelectorGeneration != 4 {
		t.Errorf("identity = %+v", got)
	}
	if got.Transcript != nil {
		t.Errorf("a transcript was stored although the benchmark does not store them: %s", got.Transcript)
	}
	if len(store.released) != 1 || store.released[0] != 9 {
		t.Errorf("released leases = %v, want [9]", store.released)
	}
}

func TestRecordShadowResult_KeepsTranscriptWhenBenchmarkStoresThem(t *testing.T) {
	store := &fakeShadowStore{}
	s := shadowServer(t, true, store)
	shadowPost(s.recordShadowResult, shadowCaller, resultBody)
	if len(store.results) != 1 || string(store.results[0].Transcript) != `{"a":1}` {
		t.Errorf("transcript = %v", store.results)
	}
}

func TestRecordShadowResult_Rejections(t *testing.T) {
	s := shadowServer(t, false, &fakeShadowStore{})
	if w := shadowPost(s.recordShadowResult, "system:serviceaccount:other:x", resultBody); w.Code != http.StatusForbidden {
		t.Errorf("caller from another namespace = %d, want 403", w.Code)
	}
	if w := shadowPost(s.recordShadowResult, shadowCaller, `{"result":{"winner":"tie"}}`); w.Code != http.StatusBadRequest {
		t.Errorf("incomplete result = %d, want 400", w.Code)
	}
	missing := strings.Replace(resultBody, `"benchmark":"bench"`, `"benchmark":"nope"`, 1)
	if w := shadowPost(s.recordShadowResult, shadowCaller, missing); w.Code != http.StatusNotFound {
		t.Errorf("unknown benchmark = %d, want 404", w.Code)
	}
}

// A deployment's sidecar names itself after the deployment. Its results are
// filed under the same Agent as the runs of that Agent, so evidence from every
// way the Agent's work is done builds up in one place.
func TestRecordShadowResult_FilesADeploymentsResultsUnderItsAgent(t *testing.T) {
	store := &fakeShadowStore{}
	s := shadowServer(t, false, store)
	w := shadowPostTo(s.recordShadowResult, shadowCaller, shadowDeployment, resultBody)
	if w.Code != http.StatusNoContent || len(store.results) != 1 {
		t.Fatalf("code = %d %s, results = %d", w.Code, w.Body, len(store.results))
	}
	got := store.results[0]
	if got.Agent != shadowAgent || got.SourceKind != postgresql.SourceKindDeployment || got.SourceName != shadowDeployment || got.Tenant != shadowTenant {
		t.Errorf("deployment result = %+v", got)
	}
}

func TestRecordShadowResult_UnknownSourceIs404(t *testing.T) {
	s := shadowServer(t, false, &fakeShadowStore{})
	if w := shadowPostTo(s.recordShadowResult, shadowCaller, "ghost", resultBody); w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// A pod can spend only the budget of a benchmark that covers its own Agent.
func TestShadow_BenchmarkMustCoverTheCallersAgent(t *testing.T) {
	store := &fakeShadowStore{cap: 10}
	s := shadowServer(t, false, store)
	if w := shadowPostTo(s.takeShadowLease, shadowCaller, shadowStray, `{"benchmark":"bench","amountUsd":0.1}`); w.Code != http.StatusForbidden {
		t.Errorf("lease for an uncovered Agent = %d, want 403", w.Code)
	}
	if w := shadowPostTo(s.recordShadowResult, shadowCaller, shadowStray, resultBody); w.Code != http.StatusForbidden || len(store.results) != 0 {
		t.Errorf("result for an uncovered Agent = %d with %d stored, want 403 and none", w.Code, len(store.results))
	}
}

// The handlers are reached through the route table, behind the token review. The
// other tests call them directly, which is how the routes once went unregistered
// and every lease request from a router was a 404.
func TestShadowRoutesAreRegisteredBehindTheTokenReview(t *testing.T) {
	store := &fakeShadowStore{cap: 10}
	s := shadowServer(t, false, store)
	s.k8s = k8sfake.NewSimpleClientset()
	s.k8s.(*k8sfake.Clientset).PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true, User: authv1.UserInfo{Username: shadowCaller},
		}}, nil
	})
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		s.handleAgentRun(rec, req)
		return rec
	}
	base := "/agentrun/" + routingTestNS + "/" + shadowRun
	if rec := call(http.MethodPost, base+"/shadow-lease", `{"benchmark":"bench","amountUsd":0.1}`); rec.Code != http.StatusOK {
		t.Errorf("shadow-lease = %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := call(http.MethodPost, base+"/shadow-results", resultBody); rec.Code != http.StatusNoContent {
		t.Errorf("shadow-results = %d %s, want 204", rec.Code, rec.Body)
	}
	if rec := call(http.MethodGet, base+"/shadow-lease", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET shadow-lease = %d, want 404 (POST only)", rec.Code)
	}
}

func TestRecordShadowResult_WithoutStoreIsAcknowledged(t *testing.T) {
	s := shadowServer(t, false, nil)
	if w := shadowPost(s.recordShadowResult, shadowCaller, resultBody); w.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", w.Code)
	}
}

func TestTakeShadowLease_UsesTheBenchmarksCapsAndDeniesWith429(t *testing.T) {
	store := &fakeShadowStore{cap: 0.15}
	s := shadowServer(t, false, store)
	body := `{"benchmark":"bench","amountUsd":0.10}`
	if w := shadowPost(s.takeShadowLease, shadowCaller, body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"leaseId":9`) {
		t.Fatalf("first lease = %d %s", w.Code, w.Body)
	}
	if store.req.PerDayUSD != 1.0 || store.req.PerRunUSD != 0.25 || store.req.MaxPerHour != 7 ||
		store.req.Agent != shadowAgent || store.req.SourceKind != postgresql.SourceKindRun || store.req.SourceName != shadowRun {
		t.Errorf("limits and identity passed to the store = %+v", store.req)
	}
	if w := shadowPost(s.takeShadowLease, shadowCaller, body); w.Code != http.StatusTooManyRequests {
		t.Errorf("second lease = %d, want 429", w.Code)
	}
}

func TestTakeShadowLease_WithoutStoreIsDenied(t *testing.T) {
	s := shadowServer(t, false, nil)
	w := shadowPost(s.takeShadowLease, shadowCaller, `{"benchmark":"bench","amountUsd":0.1}`)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("code = %d, want 429 so the router makes no shadow call", w.Code)
	}
}

func TestTakeShadowLease_Rejections(t *testing.T) {
	s := shadowServer(t, false, &fakeShadowStore{cap: 1})
	if w := shadowPost(s.takeShadowLease, "system:serviceaccount:other:x", `{"benchmark":"bench"}`); w.Code != http.StatusForbidden {
		t.Errorf("other namespace = %d, want 403", w.Code)
	}
	if w := shadowPost(s.takeShadowLease, shadowCaller, `{"amountUsd":-1}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad request = %d, want 400", w.Code)
	}
	if w := shadowPost(s.takeShadowLease, shadowCaller, `{"benchmark":"nope","amountUsd":0.1}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown benchmark = %d, want 404", w.Code)
	}
}

// The router and the operator define the result separately (the router image
// must not link the database package). This pins them to one wire format.
func TestShadowResultWireFormatMatchesRouter(t *testing.T) {
	sent := router.ShadowResultEnvelope{LeaseID: 4, Result: router.ShadowResult{
		DecisionID: "d", Benchmark: "b", Stage: "chat", SYLabel: "override", Agent: "coder", WorkflowStep: "step",
		ServedProvider: "a", ShadowProvider: "b", Winner: "tie", Inconsistent: true, JudgeConfidence: 0.5,
		JudgeProvider: "j", Dimensions: json.RawMessage(`{"correctness":"A"}`), JudgeReason: "r",
		ShadowPromptTokens: 1, ShadowCompletionTokens: 2, ShadowCostUSD: 0.1, JudgeCostUSD: 0.2,
		ShadowLatencyMS: 3, ShadowToolCalls: 4, ShadowError: "e", Transcript: json.RawMessage(`{"t":1}`),
	}}
	raw, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var got shadowResultRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the operator does not accept what the router sends: %v\n%s", err, raw)
	}
	back, _ := json.Marshal(got)
	var a, b map[string]map[string]any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(back, &b)
	delete(a["result"], "createdAt")
	delete(b["result"], "createdAt")
	// The operator fills these from the URL and the run, never from the body.
	for _, k := range []string{"namespace", "sourceKind", "sourceName", "selector", "selectorGeneration", "tenant"} {
		delete(b["result"], k)
	}
	// agent is also sent by the router but the operator overwrites it.
	if len(a["result"]) != len(b["result"]) {
		t.Fatalf("router sent %d fields, operator kept %d:\nrouter:   %s\noperator: %s", len(a["result"]), len(b["result"]), raw, back)
	}
	for k, v := range a["result"] {
		if jsonOfValue(b["result"][k]) != jsonOfValue(v) {
			t.Errorf("field %q: router %s, operator %s", k, jsonOfValue(v), jsonOfValue(b["result"][k]))
		}
	}
}

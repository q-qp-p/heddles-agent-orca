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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const (
	insightsNS    = "team-a"
	insightsPatch = `{"spec":{"capabilityRouting":{"stage:chat":"weak"}}}`
)

func insightsRec(name, ns, phase string, mutate func(*agentorcav1alpha1.RoutingRecommendation)) *agentorcav1alpha1.RoutingRecommendation {
	r := &agentorcav1alpha1.RoutingRecommendation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentorcav1alpha1.RoutingRecommendationSpec{
			SelectorRef: "adaptive", BenchmarkRef: "bench", Detector: agentorcav1alpha1.DetectorOverspend, Agent: shadowAgent,
			Category: "stage:chat", Summary: "weak answers as well", Patch: insightsPatch, BaseGeneration: 3,
			Action: agentorcav1alpha1.RecommendationActionNone,
		},
		Status: agentorcav1alpha1.RoutingRecommendationStatus{Phase: phase},
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

func insightsServer(t *testing.T, mode string, objs ...client.Object) *UIServer {
	t.Helper()
	base := []client.Object{
		&agentorcav1alpha1.ModelSelector{
			ObjectMeta: metav1.ObjectMeta{Name: "adaptive", Namespace: insightsNS},
			Status:     agentorcav1alpha1.ModelSelectorStatus{BenchmarkRef: "bench"},
		},
		&agentorcav1alpha1.RoutingBenchmark{
			ObjectMeta: metav1.ObjectMeta{Name: "bench", Namespace: insightsNS},
			Spec: agentorcav1alpha1.RoutingBenchmarkSpec{
				Mode: mode, ApproverGroups: []string{"admins"}, Budget: agentorcav1alpha1.BenchmarkBudget{PerDayUSD: "1"},
				Shadow: &agentorcav1alpha1.ShadowSpec{SampleRate: "0.1"}, Judge: &agentorcav1alpha1.JudgeSpec{ProviderRef: "judge"},
			},
			Status: agentorcav1alpha1.RoutingBenchmarkStatus{
				MatchedAgents: []string{shadowAgent}, SampledCalls: 12, JudgedPairs: 10, ShadowSpendTodayUSD: "0.040000",
				Categories: []agentorcav1alpha1.CategorySummary{{Agent: shadowAgent, Category: "stage:chat", Samples: 10, ServedWins: 3, ShadowWins: 1, Ties: 6}},
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"},
					{Type: "JudgeIsArm", Status: metav1.ConditionTrue, Reason: "JudgeIsArm", Message: "judge also compared"},
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(append(base, objs...)...).
		WithStatusSubresource(&agentorcav1alpha1.RoutingRecommendation{}).Build()
	return &UIServer{crdClient: cl}
}

func insightsGet(h http.HandlerFunc, target string, tenant *TenantIdentity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if tenant != nil {
		req = req.WithContext(tenantCtx(tenant))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decodeRecs(t *testing.T, rec *httptest.ResponseRecorder) []recommendationView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Recommendations []recommendationView `json:"recommendations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Recommendations
}

var insightsTenant = &TenantIdentity{TenantName: shadowTenant, Namespaces: []string{insightsNS}, UserID: "ada"}

func TestRecommendations_AProposalCanBeAskedAboutOnlyWhereApplyingIsAllowed(t *testing.T) {
	for name, tc := range map[string]struct {
		mode    string
		mutate  func(*agentorcav1alpha1.RoutingRecommendation)
		can     bool
		blocked string
	}{
		"proposeApply": {agentorcav1alpha1.BenchmarkModeProposeApply, nil, true, ""},
		"reportOnly":   {agentorcav1alpha1.BenchmarkModeReportOnly, nil, false, "proposeApply"},
		"already asked for": {agentorcav1alpha1.BenchmarkModeProposeApply, func(r *agentorcav1alpha1.RoutingRecommendation) {
			r.Spec.Action = agentorcav1alpha1.RecommendationActionRequestApply
		}, false, ""},
		"no patch (report only)": {agentorcav1alpha1.BenchmarkModeProposeApply, func(r *agentorcav1alpha1.RoutingRecommendation) { r.Spec.Patch, r.Spec.Reason = "", "no lever" }, false, "report only: no lever"},
	} {
		s := insightsServer(t, tc.mode, insightsRec("rec", insightsNS, agentorcav1alpha1.RecommendationProposed, tc.mutate))
		got := decodeRecs(t, insightsGet(s.handleListRecommendations, "/api/routing/recommendations", insightsTenant))
		if len(got) != 1 || got[0].CanRequestApply != tc.can || got[0].CanRollback || !strings.Contains(got[0].BlockedReason, tc.blocked) {
			t.Errorf("%s: %+v, want canRequestApply %v and a blocked reason containing %q", name, got, tc.can, tc.blocked)
		}
	}
}

func TestRecommendations_AnAppliedChangeCanBeRolledBackUnlessOneIsAlreadyAsked(t *testing.T) {
	applied := func(r *agentorcav1alpha1.RoutingRecommendation) { r.Status.AppliedRollbackPatch = `{"spec":{}}` }
	asked := func(r *agentorcav1alpha1.RoutingRecommendation) {
		applied(r)
		r.Status.Conditions = []metav1.Condition{{Type: "RollbackRequested", Status: metav1.ConditionTrue, Reason: "WaitingForApproval"}}
	}
	for name, tc := range map[string]struct {
		mutate func(*agentorcav1alpha1.RoutingRecommendation)
		can    bool
	}{"applied": {applied, true}, "already asked": {asked, false}, "no rollback patch": {nil, false}} {
		s := insightsServer(t, agentorcav1alpha1.BenchmarkModeProposeApply, insightsRec("rec", insightsNS, agentorcav1alpha1.RecommendationApplied, tc.mutate))
		got := decodeRecs(t, insightsGet(s.handleListRecommendations, "/api/routing/recommendations", insightsTenant))
		if len(got) != 1 || got[0].CanRollback != tc.can || got[0].CanRequestApply {
			t.Errorf("%s: %+v, want canRollback %v", name, got, tc.can)
		}
	}
}

func TestRecommendations_ListIsScopedToTheCallersNamespacesFiltersAndOrders(t *testing.T) {
	old := insightsRec("old-open", insightsNS, agentorcav1alpha1.RecommendationProposed, func(r *agentorcav1alpha1.RoutingRecommendation) {
		r.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour))
	})
	s := insightsServer(t, agentorcav1alpha1.BenchmarkModeProposeApply,
		insightsRec("new-open", insightsNS, agentorcav1alpha1.RecommendationProposed, func(r *agentorcav1alpha1.RoutingRecommendation) {
			r.CreationTimestamp = metav1.Now()
		}),
		old,
		insightsRec("decided", insightsNS, agentorcav1alpha1.RecommendationRejected, nil),
		insightsRec("elsewhere", "team-b", agentorcav1alpha1.RecommendationProposed, nil),
	)
	got := decodeRecs(t, insightsGet(s.handleListRecommendations, "/api/routing/recommendations", insightsTenant))
	names := make([]string, 0, len(got))
	for _, v := range got {
		names = append(names, v.Name)
	}
	if strings.Join(names, ",") != "new-open,old-open,decided" {
		t.Errorf("names = %v, want the caller's three, open ones first and the newest before the oldest", names)
	}
	only := decodeRecs(t, insightsGet(s.handleListRecommendations, "/api/routing/recommendations?phase=Rejected", insightsTenant))
	if len(only) != 1 || only[0].Name != "decided" {
		t.Errorf("phase filter = %+v", only)
	}
}

func TestRecommendations_KubectlCommandIsSafeToPasteIntoAShell(t *testing.T) {
	s := insightsServer(t, agentorcav1alpha1.BenchmarkModeReportOnly,
		insightsRec("rec", insightsNS, agentorcav1alpha1.RecommendationProposed, func(r *agentorcav1alpha1.RoutingRecommendation) {
			r.Spec.Patch = `{"spec":{"providers":[{"name":"it's","weight":1}]}}`
		}))
	got := decodeRecs(t, insightsGet(s.handleListRecommendations, "/api/routing/recommendations", insightsTenant))
	want := `kubectl patch modelselector adaptive -n team-a --type merge -p '{"spec":{"providers":[{"name":"it'\''s","weight":1}]}}'`
	if len(got) != 1 || got[0].KubectlPatch != want {
		t.Errorf("command = %q\nwant      %q", got[0].KubectlPatch, want)
	}
}

func TestBenchmarks_ListShowsStatusAndWarnings(t *testing.T) {
	s := insightsServer(t, agentorcav1alpha1.BenchmarkModeProposeApply)
	rec := insightsGet(s.handleListBenchmarks, "/api/routing/benchmarks", insightsTenant)
	var body struct {
		Benchmarks []benchmarkView `json:"benchmarks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Benchmarks) != 1 {
		t.Fatalf("%d %s (%v)", rec.Code, rec.Body, err)
	}
	b := body.Benchmarks[0]
	if !b.Ready || b.Mode != "proposeApply" || b.JudgedPairs != 10 || b.PerDayUSD != "1" || b.Judge != "judge" || b.SampleRate != "0.1" ||
		len(b.Agents) != 1 || len(b.Categories) != 1 || b.Categories[0].Ties != 6 || len(b.Warnings) != 1 || !strings.Contains(b.Warnings[0], "JudgeIsArm") {
		t.Errorf("benchmark = %+v", b)
	}
	other := &TenantIdentity{TenantName: "globex", Namespaces: []string{"team-z"}, UserID: "bob"}
	rec = insightsGet(s.handleListBenchmarks, "/api/routing/benchmarks", other)
	if strings.Contains(rec.Body.String(), `"bench"`) {
		t.Errorf("another tenant saw the benchmark: %s", rec.Body)
	}
}

// --- asking for an action ---------------------------------------------------

func actionPost(s *UIServer, path, body string, tenant *TenantIdentity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if tenant != nil {
		req = req.WithContext(tenantCtx(tenant))
	}
	rec := httptest.NewRecorder()
	s.handleRecommendationAction(rec, req)
	return rec
}

func TestRecommendationAction_SetsTheActionAndWhoAskedWithoutChangingAnythingElse(t *testing.T) {
	s := insightsServer(t, agentorcav1alpha1.BenchmarkModeProposeApply, insightsRec("rec", insightsNS, agentorcav1alpha1.RecommendationProposed, nil))
	rec := actionPost(s, "/api/routing/recommendations/team-a/rec/action", `{"action":"requestApply"}`, insightsTenant)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got agentorcav1alpha1.RoutingRecommendation
	if err := s.crdClient.Get(context.Background(), client.ObjectKey{Namespace: insightsNS, Name: "rec"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Action != agentorcav1alpha1.RecommendationActionRequestApply || got.Annotations[annotationActionRequestedBy] != "ada" || got.Spec.Patch != insightsPatch {
		t.Errorf("recommendation = %+v / %v", got.Spec, got.Annotations)
	}
}

func TestRecommendationAction_Rejections(t *testing.T) {
	s := insightsServer(t, agentorcav1alpha1.BenchmarkModeProposeApply,
		insightsRec("proposed", insightsNS, agentorcav1alpha1.RecommendationProposed, nil),
		insightsRec("applied", insightsNS, agentorcav1alpha1.RecommendationApplied, nil),
		insightsRec("foreign", "team-b", agentorcav1alpha1.RecommendationProposed, nil))
	for name, tc := range map[string]struct {
		path, body string
		tenant     *TenantIdentity
		want       int
	}{
		"not signed in":            {"/api/routing/recommendations/team-a/proposed/action", `{"action":"requestApply"}`, nil, http.StatusForbidden},
		"no user id":               {"/api/routing/recommendations/team-a/proposed/action", `{"action":"requestApply"}`, &TenantIdentity{Namespaces: []string{insightsNS}}, http.StatusForbidden},
		"another namespace":        {"/api/routing/recommendations/team-b/foreign/action", `{"action":"requestApply"}`, insightsTenant, http.StatusForbidden},
		"unknown action":           {"/api/routing/recommendations/team-a/proposed/action", `{"action":"applyNow"}`, insightsTenant, http.StatusBadRequest},
		"not json":                 {"/api/routing/recommendations/team-a/proposed/action", `please`, insightsTenant, http.StatusBadRequest},
		"missing":                  {"/api/routing/recommendations/team-a/ghost/action", `{"action":"requestApply"}`, insightsTenant, http.StatusNotFound},
		"apply an applied one":     {"/api/routing/recommendations/team-a/applied/action", `{"action":"requestApply"}`, insightsTenant, http.StatusConflict},
		"roll back a proposed one": {"/api/routing/recommendations/team-a/proposed/action", `{"action":"rollback"}`, insightsTenant, http.StatusConflict},
		"wrong path":               {"/api/routing/recommendations/team-a/proposed/other", `{"action":"requestApply"}`, insightsTenant, http.StatusNotFound},
	} {
		if rec := actionPost(s, tc.path, tc.body, tc.tenant); rec.Code != tc.want {
			t.Errorf("%s: status = %d (%s), want %d", name, rec.Code, strings.TrimSpace(rec.Body.String()), tc.want)
		}
	}
	var got agentorcav1alpha1.RoutingRecommendation
	if err := s.crdClient.Get(context.Background(), client.ObjectKey{Namespace: insightsNS, Name: "proposed"}, &got); err != nil || got.Spec.Action != agentorcav1alpha1.RecommendationActionNone {
		t.Errorf("a refused request changed the recommendation: %+v (%v)", got.Spec, err)
	}
}

func TestRecommendationAction_OnlyPostIsAccepted(t *testing.T) {
	s := insightsServer(t, agentorcav1alpha1.BenchmarkModeProposeApply)
	req := httptest.NewRequest(http.MethodGet, "/api/routing/recommendations/team-a/rec/action", nil)
	rec := httptest.NewRecorder()
	s.handleRecommendationAction(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", rec.Code)
	}
}

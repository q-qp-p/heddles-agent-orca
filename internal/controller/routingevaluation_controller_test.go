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

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/routingbench"
)

const (
	evalBench  = "bench"
	evalOther  = "other"
	evalStrong = "strong"
	evalWeak   = "weak"
	evalChat   = "stage:chat"
	evalSel    = "adaptive"
)

type fakeSource struct {
	obs   routingbench.Observations
	err   error
	calls int
	last  routingbench.Query
}

func (f *fakeSource) LoadObservations(_ context.Context, q routingbench.Query) (routingbench.Observations, error) {
	f.calls++
	f.last = q
	return f.obs, f.err
}

// overspendObservations: strong serves 300 chat calls, yet over 100 judged
// pairs weak answers as well (one win each way).
func overspendObservations() routingbench.Observations {
	var obs routingbench.Observations
	for i := range 100 {
		w := routingbench.WinnerTie
		switch i {
		case 0:
			w = routingbench.WinnerServed
		case 1:
			w = routingbench.WinnerShadow
		}
		obs.Samples = append(obs.Samples, routingbench.Sample{
			Agent: benchAgent, Category: evalChat, ServedProvider: evalStrong, ShadowProvider: evalWeak, Winner: w,
			ServedCostUSD: 0.010, ShadowCostUSD: 0.001,
		})
	}
	obs.Volumes = []routingbench.Volume{{Agent: benchAgent, Category: evalChat, Provider: evalStrong, Calls: 300}}
	return obs
}

type evalWorld struct {
	r   *RoutingEvaluationReconciler
	src *fakeSource
	c   client.Client
	now time.Time
}

func newEvalWorld(t *testing.T, mutate func(objs *[]client.Object)) *evalWorld {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pricing := func(name, in, out string) *agentorcav1alpha1.ModelProvider {
		return &agentorcav1alpha1.ModelProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Spec: agentorcav1alpha1.ModelProviderSpec{Constraints: agentorcav1alpha1.ModelConstraints{
				CostPerMillionInputTokens: in, CostPerMillionOutputTokens: out,
			}},
		}
	}
	objs := []client.Object{
		&agentorcav1alpha1.ModelSelector{
			ObjectMeta: metav1.ObjectMeta{Name: evalSel, Namespace: "ns", Generation: 3},
			Spec: agentorcav1alpha1.ModelSelectorSpec{
				Strategy:          "rule-based",
				Providers:         []agentorcav1alpha1.ProviderWeight{{Name: evalStrong, Weight: 50}, {Name: evalWeak, Weight: 50}},
				CapabilityRouting: map[string]string{"capable": evalStrong, "efficient": evalWeak},
			},
		},
		&agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: benchAgent, Namespace: "ns"},
			Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: evalSel},
		},
		pricing(evalStrong, "3", "15"), pricing(evalWeak, "0.3", "1.5"),
		&agentorcav1alpha1.RoutingBenchmark{
			ObjectMeta: metav1.ObjectMeta{Name: evalBench, Namespace: "ns"},
			Spec: agentorcav1alpha1.RoutingBenchmarkSpec{
				AgentRefs: []string{benchAgent},
				Budget:    agentorcav1alpha1.BenchmarkBudget{PerDayUSD: "1"},
			},
		},
	}
	if mutate != nil {
		mutate(&objs)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&agentorcav1alpha1.RoutingRecommendation{}, &agentorcav1alpha1.ModelSelector{}).Build()
	w := &evalWorld{src: &fakeSource{obs: overspendObservations()}, c: c, now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	w.r = &RoutingEvaluationReconciler{Client: c, Scheme: scheme, Source: w.src, now: func() time.Time { return w.now }}
	return w
}

func (w *evalWorld) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: evalSel}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (w *evalWorld) recs(t *testing.T) []agentorcav1alpha1.RoutingRecommendation {
	t.Helper()
	var list agentorcav1alpha1.RoutingRecommendationList
	if err := w.c.List(context.Background(), &list, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func (w *evalWorld) selector(t *testing.T) agentorcav1alpha1.ModelSelector {
	t.Helper()
	var sel agentorcav1alpha1.ModelSelector
	if err := w.c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: evalSel}, &sel); err != nil {
		t.Fatal(err)
	}
	return sel
}

func TestEvaluation_WritesARecommendationForAFinding(t *testing.T) {
	w := newEvalWorld(t, nil)
	res := w.reconcile(t)
	if res.RequeueAfter != 6*time.Hour {
		t.Errorf("requeue = %v, want the 6h default", res.RequeueAfter)
	}
	recs := w.recs(t)
	if len(recs) != 1 {
		t.Fatalf("recommendations = %d, want 1", len(recs))
	}
	rec := recs[0]
	if !strings.HasPrefix(rec.Name, "adaptive-overspend-") || rec.Labels[labelRecSelector] != evalSel || rec.Labels[labelRecDetector] != "overspend" {
		t.Errorf("name %q labels %v", rec.Name, rec.Labels)
	}
	sp := rec.Spec
	if sp.SelectorRef != evalSel || sp.BenchmarkRef != evalBench || sp.Agent != benchAgent || sp.Category != evalChat ||
		sp.BaseGeneration != 3 || sp.Action != agentorcav1alpha1.RecommendationActionNone {
		t.Errorf("spec = %+v", sp)
	}
	if sp.Patch != `{"spec":{"capabilityRouting":{"stage:chat":"weak"}}}` || sp.RollbackPatch != `{"spec":{"capabilityRouting":{"stage:chat":null}}}` {
		t.Errorf("patch %q rollback %q", sp.Patch, sp.RollbackPatch)
	}
	if rec.Status.Phase != agentorcav1alpha1.RecommendationProposed || rec.Status.Evidence == nil ||
		!strings.Contains(string(rec.Status.Evidence.Raw), `"samples":100`) || rec.Status.LastEvaluated == nil {
		t.Errorf("status = %+v", rec.Status)
	}
	if len(rec.OwnerReferences) != 1 || rec.OwnerReferences[0].Name != evalSel {
		t.Errorf("owner references = %+v, want the selector so deleting it removes its recommendations", rec.OwnerReferences)
	}
	if sel := w.selector(t); sel.Status.OpenRecommendations != 1 || sel.Status.BenchmarkRef != evalBench {
		t.Errorf("selector status = %+v", sel.Status)
	}
	// The window is the benchmark default (a week), up to now, for every Agent on the selector.
	if got := w.src.last; got.Namespace != "ns" || len(got.Agents) != 1 || got.Agents[0] != benchAgent || got.Now.Sub(got.Since) != 168*time.Hour {
		t.Errorf("query = %+v", got)
	}
}

func TestEvaluation_IsIdempotentAndRefreshesEvidence(t *testing.T) {
	w := newEvalWorld(t, nil)
	w.reconcile(t)
	first := w.recs(t)[0]
	w.now = w.now.Add(6 * time.Hour)
	w.reconcile(t)
	recs := w.recs(t)
	if len(recs) != 1 || recs[0].Name != first.Name {
		t.Fatalf("recommendations = %+v, want the same one", recs)
	}
	if !recs[0].Status.LastEvaluated.After(first.Status.LastEvaluated.Time) {
		t.Error("the evidence was not refreshed")
	}
}

func TestEvaluation_ExpiresAProposalWhoseEvidenceGoesAndRevivesItIfItReturns(t *testing.T) {
	w := newEvalWorld(t, nil)
	w.reconcile(t)
	name := w.recs(t)[0].Name

	w.src.obs = routingbench.Observations{}
	w.reconcile(t)
	if got := w.recs(t)[0]; got.Name != name || got.Status.Phase != agentorcav1alpha1.RecommendationExpired {
		t.Fatalf("after the evidence went: %+v", got.Status)
	}
	if sel := w.selector(t); sel.Status.OpenRecommendations != 0 {
		t.Errorf("open recommendations = %d, want 0", sel.Status.OpenRecommendations)
	}

	w.src.obs = overspendObservations()
	w.reconcile(t)
	if got := w.recs(t); len(got) != 1 || got[0].Status.Phase != agentorcav1alpha1.RecommendationProposed {
		t.Errorf("after the evidence returned: %+v", got)
	}
}

func TestEvaluation_LeavesWhatAPersonActedOnAlone(t *testing.T) {
	for _, phase := range []string{
		agentorcav1alpha1.RecommendationRejected, agentorcav1alpha1.RecommendationApplied,
		agentorcav1alpha1.RecommendationPendingApproval, agentorcav1alpha1.RecommendationRolledBack,
	} {
		w := newEvalWorld(t, nil)
		w.reconcile(t)
		rec := w.recs(t)[0]
		rec.Status.Phase, rec.Status.Message = phase, "by a person"
		if err := w.c.Status().Update(context.Background(), &rec); err != nil {
			t.Fatal(err)
		}
		w.src.obs = routingbench.Observations{} // the evidence has gone, which expires only proposals
		w.reconcile(t)
		if got := w.recs(t)[0]; got.Status.Phase != phase || got.Status.Message != "by a person" {
			t.Errorf("%s: after another evaluation: %+v", phase, got.Status)
		}
	}
}

func TestEvaluation_SupersedesAnOlderVersionOfTheSameFinding(t *testing.T) {
	w := newEvalWorld(t, nil)
	w.reconcile(t)
	current := w.recs(t)[0]
	// An older proposal for the same finding, with another patch.
	old := current.DeepCopy()
	old.ObjectMeta = metav1.ObjectMeta{
		Name: "adaptive-overspend-0000000000", Namespace: "ns", Labels: current.Labels,
		Annotations: map[string]string{annotationRecKey: current.Annotations[annotationRecKey]},
	}
	old.Spec.Patch = `{"spec":{"capabilityRouting":{"stage:chat":evalOther}}}`
	old.Status = agentorcav1alpha1.RoutingRecommendationStatus{}
	if err := w.c.Create(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	old.Status.Phase = agentorcav1alpha1.RecommendationProposed
	if err := w.c.Status().Update(context.Background(), old); err != nil {
		t.Fatal(err)
	}

	w.reconcile(t)
	var got agentorcav1alpha1.RoutingRecommendation
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(old), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != agentorcav1alpha1.RecommendationSuperseded || !strings.Contains(got.Status.Message, current.Name) {
		t.Errorf("older proposal = %+v, want Superseded by %s", got.Status, current.Name)
	}
}

func TestEvaluation_DoesNothingWithoutASourceOrWhenSwitchedOff(t *testing.T) {
	for name, set := range map[string]func(*evalWorld){
		"no postgres": func(w *evalWorld) { w.r.Source = nil },
		"flag off":    func(w *evalWorld) { w.r.Disabled = true },
		"selector opt-out": func(w *evalWorld) {
			sel := w.selector(t)
			sel.Annotations = map[string]string{annotationRoutingRecommendations: routingRecommendationsDisabled}
			if err := w.c.Update(context.Background(), &sel); err != nil {
				t.Fatal(err)
			}
		},
	} {
		w := newEvalWorld(t, nil)
		set(w)
		w.reconcile(t)
		if len(w.recs(t)) != 0 || w.src.calls != 0 {
			t.Errorf("%s: %d recommendations, %d loads; want none", name, len(w.recs(t)), w.src.calls)
		}
	}
}

func TestEvaluation_SelectorWithNoAgentsLoadsNothing(t *testing.T) {
	w := newEvalWorld(t, func(objs *[]client.Object) {
		for i, o := range *objs {
			if _, ok := o.(*agentorcav1alpha1.Agent); ok {
				*objs = append((*objs)[:i], (*objs)[i+1:]...)
				break
			}
		}
	})
	res := w.reconcile(t)
	if w.src.calls != 0 || len(w.recs(t)) != 0 || res.RequeueAfter == 0 {
		t.Errorf("loads %d recs %d requeue %v", w.src.calls, len(w.recs(t)), res.RequeueAfter)
	}
}

func TestEvaluation_SourceErrorIsReturnedAndWritesNothing(t *testing.T) {
	w := newEvalWorld(t, nil)
	w.src.err = errors.New("postgres down")
	if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: evalSel}}); err == nil ||
		!strings.Contains(err.Error(), "postgres down") {
		t.Errorf("err = %v", err)
	}
	if len(w.recs(t)) != 0 {
		t.Error("recommendations were written although the evidence could not be loaded")
	}
}

// A selector another Agent also uses cannot be changed on one Agent's evidence.
func TestEvaluation_ASharedSelectorGetsAReportOnlyRecommendationUntilEveryAgentHasEvidence(t *testing.T) {
	w := newEvalWorld(t, func(objs *[]client.Object) {
		*objs = append(*objs, &agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: "ns"},
			Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: evalSel},
		})
	})
	w.reconcile(t)
	recs := w.recs(t)
	if len(recs) != 1 {
		t.Fatalf("recommendations = %+v", recs)
	}
	if recs[0].Spec.Patch != "" || !strings.Contains(recs[0].Spec.Reason, "writer") {
		t.Errorf("spec = %+v, want no patch and a reason that names writer", recs[0].Spec)
	}
	if len(w.src.last.Agents) != 2 {
		t.Errorf("evidence was loaded for %v, want every Agent on the selector", w.src.last.Agents)
	}
}

func TestEvaluation_PassiveFindingsNeedNoBenchmark(t *testing.T) {
	w := newEvalWorld(t, func(objs *[]client.Object) {
		kept := (*objs)[:0]
		for _, o := range *objs {
			switch v := o.(type) {
			case *agentorcav1alpha1.RoutingBenchmark:
				continue
			case *agentorcav1alpha1.ModelSelector:
				// No role points at the failing provider, so it can be removed.
				v.Spec.CapabilityRouting = map[string]string{"capable": evalStrong}
			}
			kept = append(kept, o)
		}
		*objs = kept
	})
	w.src.obs = routingbench.Observations{ProviderStats: []routingbench.ProviderStat{
		{Selector: evalSel, Provider: evalWeak, Calls: 50, Errors: 45},
	}}
	w.reconcile(t)
	recs := w.recs(t)
	if len(recs) != 1 || recs[0].Spec.Detector != "deadProvider" || recs[0].Spec.BenchmarkRef != "" || recs[0].Spec.Patch == "" {
		t.Fatalf("recommendations = %+v, want one deadProvider with a patch and no benchmark", recs)
	}
	if sel := w.selector(t); sel.Status.BenchmarkRef != "" || sel.Status.OpenRecommendations != 1 {
		t.Errorf("selector status = %+v", sel.Status)
	}
}

func TestEvaluation_ThresholdsFromTheBenchmarkReachTheDetectors(t *testing.T) {
	limit := "0.05"
	o := optionsOf(&agentorcav1alpha1.RoutingBenchmark{Spec: agentorcav1alpha1.RoutingBenchmarkSpec{
		Evaluation: &agentorcav1alpha1.EvaluationSpec{MinSamples: 12, ConfidenceLevel: "0.9"},
		Thresholds: &agentorcav1alpha1.BenchmarkThresholds{NonInferiorityMargin: "0.1", MaxUSDPerNetWin: limit, DeadProviderErrorRate: "0.3"},
	}})
	if o.MinSamples != 12 || o.Confidence != 0.9 || o.NonInferiorityMargin != 0.1 || o.DeadProviderErrorRate != 0.3 ||
		o.MaxUSDPerNetWin == nil || *o.MaxUSDPerNetWin != 0.05 {
		t.Errorf("options = %+v", o)
	}
	if zero := optionsOf(nil); zero.MaxUSDPerNetWin != nil || zero.MinSamples != 0 {
		t.Errorf("no benchmark = %+v, want defaults and no price limit", zero)
	}
}

func TestEvaluation_ClusterReadsThePriceAndTheSwitchyardBlock(t *testing.T) {
	w := newEvalWorld(t, nil)
	sel := w.selector(t)
	sel.Spec.Strategy = strategySwitchyard
	sel.Spec.Switchyard = &runtime.RawExtension{Raw: []byte(`{"type":"composite","classifier":{"base_threshold":0.5}}`)}
	c, err := w.r.cluster(context.Background(), &sel, []*agentorcav1alpha1.Agent{{ObjectMeta: metav1.ObjectMeta{Name: benchAgent}}})
	if err != nil {
		t.Fatal(err)
	}
	// strong: $3 + $15 per million = 0.000018 a token.
	if c.ProviderCost[evalStrong] != 0.000018 || c.AgentSelector[benchAgent] != evalSel {
		t.Errorf("cluster = %+v", c)
	}
	if got := c.Selectors[evalSel]; got.Strategy != "switchyard" || got.Switchyard["type"] != "composite" {
		t.Errorf("selector = %+v", got)
	}
}

func TestEvaluation_ADeadProviderThatARoleStillUsesIsReportedNotRemoved(t *testing.T) {
	w := newEvalWorld(t, nil) // capabilityRouting names weak as the efficient model
	w.src.obs = routingbench.Observations{ProviderStats: []routingbench.ProviderStat{
		{Selector: evalSel, Provider: evalWeak, Calls: 50, Errors: 45},
	}}
	w.reconcile(t)
	recs := w.recs(t)
	if len(recs) != 1 || recs[0].Spec.Patch != "" || !strings.Contains(recs[0].Spec.Reason, "still sends traffic") {
		t.Fatalf("recommendations = %+v, want a report-only deadProvider that explains why", recs)
	}
}

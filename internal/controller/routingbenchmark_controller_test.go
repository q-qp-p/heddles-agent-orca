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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/router"
)

const benchJudge = "judge"

const benchAgent = "coder"

func agentFixture(name string, labels map[string]string) *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "adaptive"},
	}
}

// benchScope is the scope of a benchmark that covers one Agent routing through
// the selector.
func benchScope(sel *agentorcav1alpha1.ModelSelector) benchmarkScope {
	scope := benchmarkScope{Agents: []string{benchAgent}, Providers: map[string]bool{}}
	for _, n := range providerNames(sel) {
		scope.Providers[n] = true
	}
	return scope
}

func benchmarkFixture() (*agentorcav1alpha1.RoutingBenchmark, *agentorcav1alpha1.ModelSelector) {
	sel := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "adaptive", Namespace: "ns"},
		Spec:       agentorcav1alpha1.ModelSelectorSpec{Providers: []agentorcav1alpha1.ProviderWeight{{Name: "strong"}, {Name: "weak"}}},
	}
	b := &agentorcav1alpha1.RoutingBenchmark{
		ObjectMeta: metav1.ObjectMeta{Name: "bench", Namespace: "ns", Generation: 1},
		Spec: agentorcav1alpha1.RoutingBenchmarkSpec{
			AgentRefs: []string{benchAgent},
			Judge:     &agentorcav1alpha1.JudgeSpec{ProviderRef: benchJudge},
			Budget:    agentorcav1alpha1.BenchmarkBudget{PerDayUSD: "1.00"},
		},
	}
	return b, sel
}

func benchCondition(conds []metav1.Condition, t string) *metav1.Condition {
	return meta.FindStatusCondition(conds, t)
}

func TestAssessBenchmark_ReadyWhenEverythingResolves(t *testing.T) {
	b, sel := benchmarkFixture()
	conds := assessBenchmark(b, benchScope(sel), nil, true)
	if c := benchCondition(conds, "Ready"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v", c)
	}
	if hasCondition(conds, agentorcav1alpha1.BenchmarkConditionJudgeIsArm) || hasCondition(conds, agentorcav1alpha1.BenchmarkConditionPostgresUnavailable) {
		t.Errorf("unexpected warning conditions: %+v", conds)
	}
}

func TestAssessBenchmark_NotReady(t *testing.T) {
	for name, tc := range map[string]struct {
		scope    func(benchmarkScope) benchmarkScope
		missing  map[string]bool
		postgres bool
		reason   string
	}{
		"no agent matches": {
			scope:    func(s benchmarkScope) benchmarkScope { s.Agents = nil; return s },
			postgres: true, reason: reasonNoAgentsMatched,
		},
		"named agent missing": {
			scope:    func(s benchmarkScope) benchmarkScope { s.MissingAgents = []string{"ghost"}; return s },
			postgres: true, reason: reasonAgentNotFound,
		},
		"selector missing": {
			scope:    func(s benchmarkScope) benchmarkScope { s.MissingSelectors = []string{"coder -> adaptive"}; return s },
			postgres: true, reason: reasonSelectorNotFound,
		},
		"judge missing": {missing: map[string]bool{benchJudge: true}, postgres: true, reason: reasonBenchmarkProviderGap},
		"no postgres":   {postgres: false, reason: reasonPostgresUnavailable},
	} {
		b, sel := benchmarkFixture()
		scope := benchScope(sel)
		if tc.scope != nil {
			scope = tc.scope(scope)
		}
		conds := assessBenchmark(b, scope, tc.missing, tc.postgres)
		c := benchCondition(conds, "Ready")
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tc.reason {
			t.Errorf("%s: Ready = %+v, want False/%s", name, c, tc.reason)
		}
	}
	b, sel := benchmarkFixture()
	if c := benchCondition(assessBenchmark(b, benchScope(sel), nil, false), agentorcav1alpha1.BenchmarkConditionPostgresUnavailable); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("PostgresUnavailable = %+v", c)
	}
}

func TestAssessBenchmark_JudgeIsArmWarnsButStaysReady(t *testing.T) {
	b, sel := benchmarkFixture()
	b.Spec.Judge.ProviderRef = "strong"
	conds := assessBenchmark(b, benchScope(sel), nil, true)
	if c := benchCondition(conds, agentorcav1alpha1.BenchmarkConditionJudgeIsArm); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("JudgeIsArm = %+v", c)
	}
	if c := benchCondition(conds, "Ready"); c.Status != metav1.ConditionTrue {
		t.Errorf("a warning must not make the benchmark not Ready: %+v", c)
	}
	b.Spec.Judge.ProviderRef = benchJudge
	b.Spec.Shadow = &agentorcav1alpha1.ShadowSpec{Arms: agentorcav1alpha1.ShadowArmsProviders, Providers: []string{benchJudge}}
	if !judgeIsArm(b, benchScope(sel)) {
		t.Error("a judge listed as a shadow provider is an arm")
	}
}

type fakeResults struct {
	tallies []postgresql.ShadowTally
	spend   postgresql.ShadowSpend
}

func (f fakeResults) ShadowSpendSince(context.Context, string, string, time.Time, time.Time) (postgresql.ShadowSpend, error) {
	return f.spend, nil
}

func (f fakeResults) ShadowTallies(context.Context, string, string, time.Time) ([]postgresql.ShadowTally, error) {
	return f.tallies, nil
}

func reconcileBenchmark(t *testing.T, results BenchmarkResults, extra ...client.Object) (*agentorcav1alpha1.RoutingBenchmark, ctrl.Result) {
	t.Helper()
	b, sel := benchmarkFixture()
	judge := &agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: benchJudge, Namespace: "ns"}}
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := append([]client.Object{b, sel, judge, agentFixture(benchAgent, nil)}, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&agentorcav1alpha1.RoutingBenchmark{}).Build()
	r := &RoutingBenchmarkReconciler{Client: c, Scheme: scheme, Results: results,
		now: func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "bench"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got agentorcav1alpha1.RoutingBenchmark
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "bench"}, &got); err != nil {
		t.Fatal(err)
	}
	return &got, res
}

func TestReconcileBenchmark_PublishesResultsAndRequeues(t *testing.T) {
	got, res := reconcileBenchmark(t, fakeResults{
		tallies: []postgresql.ShadowTally{
			{Agent: benchAgent, Category: "chat", Served: 3, Shadow: 1, Ties: 2, Invalid: 1, Inconsistent: 3},
		},
		spend: postgresql.ShadowSpend{SettledUSD: 0.4, LeasedUSD: 0.7},
	})
	st := got.Status
	if st.JudgedPairs != 6 || st.SampledCalls != 7 || st.InconsistentJudgments != 3 || st.JudgeInconsistentRate != "0.500" {
		t.Errorf("counts = %+v", st)
	}
	if len(st.Categories) != 1 || st.Categories[0].Agent != benchAgent || st.Categories[0].Category != "stage:chat" || st.Categories[0].ServedWins != 3 {
		t.Errorf("categories = %+v", st.Categories)
	}
	if len(st.MatchedAgents) != 1 || st.MatchedAgents[0] != benchAgent {
		t.Errorf("matched agents = %v", st.MatchedAgents)
	}
	if st.ShadowSpendTodayUSD != "0.400000" || !st.BudgetExhausted {
		t.Errorf("spend = %s exhausted = %v; leased budget counts toward exhaustion", st.ShadowSpendTodayUSD, st.BudgetExhausted)
	}
	if res.RequeueAfter != 6*time.Hour {
		t.Errorf("requeue = %v, want the 6h default interval", res.RequeueAfter)
	}
}

// Evidence is kept per Agent, so one Agent's samples never stand in for another's.
func TestReconcileBenchmark_KeepsEachAgentsCategoriesApart(t *testing.T) {
	got, _ := reconcileBenchmark(t, fakeResults{tallies: []postgresql.ShadowTally{
		{Agent: benchAgent, Category: "chat", Served: 1},
		{Agent: "writer", Category: "chat", Shadow: 5},
		{Agent: benchAgent, Category: "tool-exec", Ties: 3},
	}})
	byKey := map[string]agentorcav1alpha1.CategorySummary{}
	for _, c := range got.Status.Categories {
		byKey[c.Agent+"/"+c.Category] = c
	}
	if len(byKey) != 3 || byKey["writer/stage:chat"].ShadowWins != 5 || byKey[benchAgent+"/stage:chat"].ShadowWins != 0 {
		t.Errorf("categories = %+v", got.Status.Categories)
	}
	// The best-sampled category comes first.
	if got.Status.Categories[0].Agent != "writer" {
		t.Errorf("first category = %+v, want the best-sampled", got.Status.Categories[0])
	}
}

func TestReconcileBenchmark_MatchesAgentsBySelectorAndName(t *testing.T) {
	b, sel := benchmarkFixture()
	b.Spec.AgentRefs = []string{"named"}
	b.Spec.AgentSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(b, sel,
		agentFixture("named", nil), agentFixture("labelled", map[string]string{"team": "a"}), agentFixture("other", map[string]string{"team": "b"}),
	).Build()
	r := &RoutingBenchmarkReconciler{Client: c, Scheme: scheme}
	scope, err := r.scopeOf(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(scope.Agents, ",") != "labelled,named" || len(scope.MissingAgents) != 0 {
		t.Errorf("scope = %+v, want labelled and named, not other", scope)
	}
	b.Spec.AgentRefs = []string{"named", "ghost"}
	if scope, _ = r.scopeOf(context.Background(), b); strings.Join(scope.MissingAgents, ",") != "ghost" {
		t.Errorf("missing agents = %v, want ghost", scope.MissingAgents)
	}
}

func TestReconcileBenchmark_WithoutPostgresReportsIt(t *testing.T) {
	got, _ := reconcileBenchmark(t, nil)
	if c := benchCondition(got.Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonPostgresUnavailable {
		t.Errorf("Ready = %+v", c)
	}
	if got.Status.LastEvaluated != nil {
		t.Error("evaluated results without a store")
	}
}

// --- delivery to the router ---------------------------------------------------

// runSource is an AgentRun labelled team=a.
var runSource = shadowSource{RunLabels: map[string]string{"team": "a"}}

func shadowFixture(t *testing.T, mutate func(*agentorcav1alpha1.RoutingBenchmark)) (shadowRouting, error) {
	t.Helper()
	return shadowFixtureFor(t, runSource, mutate)
}

func shadowFixtureFor(t *testing.T, src shadowSource, mutate func(*agentorcav1alpha1.RoutingBenchmark)) (shadowRouting, error) {
	t.Helper()
	b, sel := benchmarkFixture()
	b.Spec.Shadow = &agentorcav1alpha1.ShadowSpec{Enabled: true, SampleRate: "0.25", MaxPerRun: 2}
	if mutate != nil {
		mutate(b)
	}
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := agentFixture(benchAgent, map[string]string{"app": "x"})
	objs := []client.Object{b, sel, agent, &agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: benchJudge, Namespace: "ns"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	routing := selectorRouting{Providers: []router.ProviderConfig{{Name: "strong"}, {Name: "weak"}}}
	return resolveShadow(context.Background(), c, agent, src, routing)
}

func TestResolveShadow_BuildsConfigAndAuxJudge(t *testing.T) {
	got, err := shadowFixture(t, nil)
	if err != nil || got.Config == nil {
		t.Fatalf("shadow = %+v, %v", got, err)
	}
	c := got.Config
	if c.Benchmark != "bench" || c.SampleRate != 0.25 || c.MaxPerRun != 2 || c.Judge != benchJudge || c.Arms != "counterfactual" {
		t.Errorf("config = %+v", c)
	}
	if !c.PositionSwap || !c.IncludeToolTurns || c.JudgeMaxTokens != 512 {
		t.Errorf("defaults not applied: %+v", c)
	}
	if len(got.Aux) != 1 || got.Aux[0].Name != benchJudge || got.Aux[0].Weight != 0 || !strings.HasSuffix(got.Aux[0].APIKeyFile, "/judge/api-key") {
		t.Errorf("aux = %+v", got.Aux)
	}
	if names := got.secretNames(); len(names) != 1 || names[0] != benchJudge {
		t.Errorf("secret names = %v", names)
	}
}

func TestResolveShadow_NotBenchmarked(t *testing.T) {
	for name, mutate := range map[string]func(*agentorcav1alpha1.RoutingBenchmark){
		"disabled":    func(b *agentorcav1alpha1.RoutingBenchmark) { b.Spec.Shadow.Enabled = false },
		"other agent": func(b *agentorcav1alpha1.RoutingBenchmark) { b.Spec.AgentRefs = []string{"unrelated"} },
		"selector does not match": func(b *agentorcav1alpha1.RoutingBenchmark) {
			b.Spec.AgentRefs = nil
			b.Spec.AgentSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "y"}}
		},
		"no agents named": func(b *agentorcav1alpha1.RoutingBenchmark) { b.Spec.AgentRefs = nil },
		"run label excluded": func(b *agentorcav1alpha1.RoutingBenchmark) {
			b.Spec.Shadow.ExcludeRunSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}
		},
		"judge not found": func(b *agentorcav1alpha1.RoutingBenchmark) { b.Spec.Judge.ProviderRef = "ghost" },
		"no shadow block": func(b *agentorcav1alpha1.RoutingBenchmark) { b.Spec.Shadow = nil },
	} {
		got, err := shadowFixture(t, mutate)
		if err != nil || got.Config != nil || len(got.Aux) != 0 {
			t.Errorf("%s: shadow = %+v, %v; want none", name, got, err)
		}
	}
}

// The benchmark follows the Agent, however its work is done.
func TestResolveShadow_CoversAnAgentByLabelOrByAnEmptySelector(t *testing.T) {
	for name, sel := range map[string]*metav1.LabelSelector{
		"label":    {MatchLabels: map[string]string{"app": "x"}},
		"every":    {},
		"set-expr": {MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"x", "z"}}}},
	} {
		got, err := shadowFixture(t, func(b *agentorcav1alpha1.RoutingBenchmark) {
			b.Spec.AgentRefs, b.Spec.AgentSelector = nil, sel
		})
		if err != nil || got.Config == nil {
			t.Errorf("%s: shadow = %+v, %v; want the Agent covered", name, got, err)
		}
	}
}

func TestResolveShadow_DeploymentHasNoRunCapAndIgnoresRunLabels(t *testing.T) {
	got, err := shadowFixtureFor(t, shadowSource{Deployment: true}, func(b *agentorcav1alpha1.RoutingBenchmark) {
		b.Spec.Shadow.ExcludeRunSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}
	})
	if err != nil || got.Config == nil {
		t.Fatalf("shadow = %+v, %v; a deployment is not an AgentRun and run labels do not exclude it", got, err)
	}
	if got.Config.MaxPerRun != 0 {
		t.Errorf("MaxPerRun = %d, want 0 (no local cap; the operator limits samples per hour)", got.Config.MaxPerRun)
	}
	run, _ := shadowFixture(t, nil)
	if run.Config.MaxPerRun != 2 {
		t.Errorf("a run keeps its cap, got %d", run.Config.MaxPerRun)
	}
}

func TestResolveShadow_JudgeThatIsAnArmNeedsNoExtraMount(t *testing.T) {
	got, err := shadowFixture(t, func(b *agentorcav1alpha1.RoutingBenchmark) { b.Spec.Judge.ProviderRef = "strong" })
	if err != nil || got.Config == nil || len(got.Aux) != 0 {
		t.Errorf("shadow = %+v, %v; a judge the selector already routes to is already mounted", got, err)
	}
}

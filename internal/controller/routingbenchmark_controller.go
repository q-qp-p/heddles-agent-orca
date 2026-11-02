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
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// Reasons of the RoutingBenchmark conditions.
const (
	reasonBenchmarkReady       = "Ready"
	reasonSelectorNotFound     = "SelectorNotFound"
	reasonNoAgentsMatched      = "NoAgentsMatched"
	reasonAgentNotFound        = "AgentNotFound"
	reasonBenchmarkProviderGap = "ProviderNotFound"
	reasonPostgresUnavailable  = "PostgresUnavailable"
	reasonJudgeIsArm           = "JudgeIsArm"
)

// defaultBenchmarkInterval is how often a benchmark is re-evaluated when
// spec.evaluation.interval is unset.
const defaultBenchmarkInterval = 6 * time.Hour

// defaultBenchmarkWindow is how far back results count when
// spec.evaluation.window is unset.
const defaultBenchmarkWindow = 168 * time.Hour

// BenchmarkResults reads a benchmark's results. *postgresql.Store implements it.
type BenchmarkResults interface {
	ShadowSpendSince(ctx context.Context, namespace, benchmark string, since, now time.Time) (postgresql.ShadowSpend, error)
	ShadowTallies(ctx context.Context, namespace, benchmark string, since time.Time) ([]postgresql.ShadowTally, error)
}

// RoutingBenchmarkReconciler validates a RoutingBenchmark and publishes what
// its shadow sampling has found. It never changes a ModelSelector: sampling
// itself runs in the model-router sidecar of each run and deployment.
type RoutingBenchmarkReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Results reads shadow results. nil means the operator has no Postgres.
	Results BenchmarkResults

	// now is the clock; nil means time.Now.
	now func() time.Time
}

// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=routingbenchmarks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=routingbenchmarks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=routingbenchmarks/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelselectors,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelproviders,verbs=get;list;watch

// Reconcile checks one RoutingBenchmark and refreshes its status.
func (r *RoutingBenchmarkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b agentorcav1alpha1.RoutingBenchmark
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}

	scope, err := r.scopeOf(ctx, &b)
	if err != nil {
		return ctrl.Result{}, err
	}
	missing := map[string]bool{}
	for _, name := range benchmarkProviders(&b) {
		var mp agentorcav1alpha1.ModelProvider
		switch err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, &mp); {
		case err == nil:
		case client.IgnoreNotFound(err) != nil:
			return ctrl.Result{}, fmt.Errorf("getting ModelProvider %q: %w", name, err)
		default:
			missing[name] = true
		}
	}

	conds := assessBenchmark(&b, scope, missing, r.Results != nil)
	patch := client.MergeFrom(b.DeepCopy())
	b.Status.ObservedGeneration = b.Generation
	b.Status.MatchedAgents = scope.Agents
	if len(b.Status.MatchedAgents) > maxMatchedAgents {
		b.Status.MatchedAgents = b.Status.MatchedAgents[:maxMatchedAgents]
	}
	for _, c := range conds {
		c.ObservedGeneration = b.Generation
		c.LastTransitionTime = metav1.NewTime(now)
		setCondition(&b.Status.Conditions, c)
	}
	if !hasCondition(conds, agentorcav1alpha1.BenchmarkConditionJudgeIsArm) {
		meta.RemoveStatusCondition(&b.Status.Conditions, agentorcav1alpha1.BenchmarkConditionJudgeIsArm)
	}
	if !hasCondition(conds, agentorcav1alpha1.BenchmarkConditionPostgresUnavailable) {
		meta.RemoveStatusCondition(&b.Status.Conditions, agentorcav1alpha1.BenchmarkConditionPostgresUnavailable)
	}

	if r.Results != nil {
		if err := r.refreshResults(ctx, &b, now); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.Status().Patch(ctx, &b, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching RoutingBenchmark status: %w", err)
	}
	return ctrl.Result{RequeueAfter: benchmarkInterval(&b)}, nil
}

// Limits that keep the status within its schema.
const (
	maxMatchedAgents = 100
	maxCategories    = 200
)

// benchmarkScope is what a benchmark covers now: the Agents it matches and the
// ModelSelectors those Agents route through.
type benchmarkScope struct {
	// Agents are the matched Agents, sorted by name.
	Agents []string
	// MissingAgents are names in agentRefs that no Agent has.
	MissingAgents []string
	// MissingSelectors lists "agent -> selector" for matched Agents whose
	// ModelSelector does not exist.
	MissingSelectors []string
	// Providers are the ModelProviders any matched Agent can route to.
	Providers map[string]bool
}

// scopeOf finds the Agents a benchmark covers and their selectors.
func (r *RoutingBenchmarkReconciler) scopeOf(ctx context.Context, b *agentorcav1alpha1.RoutingBenchmark) (benchmarkScope, error) {
	var agents agentorcav1alpha1.AgentList
	if err := r.List(ctx, &agents, client.InNamespace(b.Namespace)); err != nil {
		return benchmarkScope{}, fmt.Errorf("listing Agents: %w", err)
	}
	scope := benchmarkScope{Providers: map[string]bool{}}
	found := map[string]bool{}
	selectors := map[string]*agentorcav1alpha1.ModelSelector{}
	for i := range agents.Items {
		a := &agents.Items[i]
		found[a.Name] = true
		if !b.Covers(a) {
			continue
		}
		scope.Agents = append(scope.Agents, a.Name)
		ref := a.Spec.ModelSelectorRef
		sel, seen := selectors[ref]
		if !seen {
			var s agentorcav1alpha1.ModelSelector
			switch err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ref}, &s); {
			case err == nil:
				sel = &s
			case client.IgnoreNotFound(err) != nil:
				return benchmarkScope{}, fmt.Errorf("getting ModelSelector %q: %w", ref, err)
			}
			selectors[ref] = sel
		}
		if sel == nil {
			scope.MissingSelectors = append(scope.MissingSelectors, a.Name+" -> "+ref)
			continue
		}
		for _, n := range providerNames(sel) {
			scope.Providers[n] = true
		}
	}
	sort.Strings(scope.Agents)
	sort.Strings(scope.MissingSelectors)
	for _, name := range b.Spec.AgentRefs {
		if !found[name] {
			scope.MissingAgents = append(scope.MissingAgents, name)
		}
	}
	sort.Strings(scope.MissingAgents)
	return scope, nil
}

// refreshResults fills the counts, spend and per-category summaries of b.Status.
func (r *RoutingBenchmarkReconciler) refreshResults(ctx context.Context, b *agentorcav1alpha1.RoutingBenchmark, now time.Time) error {
	since := now.Add(-benchmarkWindow(b))
	tallies, err := r.Results.ShadowTallies(ctx, b.Namespace, b.Name, since)
	if err != nil {
		return err
	}
	st := &b.Status
	st.SampledCalls, st.JudgedPairs, st.InconsistentJudgments = 0, 0, 0
	st.Categories = st.Categories[:0]
	for _, t := range tallies {
		judged := t.Served + t.Shadow + t.Ties
		st.SampledCalls += int32(judged + t.Invalid)
		st.JudgedPairs += int32(judged)
		st.InconsistentJudgments += int32(t.Inconsistent)
		st.Categories = append(st.Categories, agentorcav1alpha1.CategorySummary{
			Agent: t.Agent, Category: "stage:" + t.Category, Samples: int32(judged),
			ServedWins: int32(t.Served), ShadowWins: int32(t.Shadow), Ties: int32(t.Ties),
		})
	}
	// The best-sampled categories first, so a cap drops the thinnest evidence.
	sort.SliceStable(st.Categories, func(i, j int) bool { return st.Categories[i].Samples > st.Categories[j].Samples })
	if len(st.Categories) > maxCategories {
		st.Categories = st.Categories[:maxCategories]
	}
	st.JudgeInconsistentRate = ""
	if st.JudgedPairs > 0 {
		st.JudgeInconsistentRate = strconv.FormatFloat(float64(st.InconsistentJudgments)/float64(st.JudgedPairs), 'f', 3, 64)
	}

	dayStart := now.UTC().Truncate(24 * time.Hour)
	spend, err := r.Results.ShadowSpendSince(ctx, b.Namespace, b.Name, dayStart, now)
	if err != nil {
		return err
	}
	st.ShadowSpendTodayUSD = strconv.FormatFloat(spend.SettledUSD, 'f', 6, 64)
	perDay, _ := strconv.ParseFloat(b.Spec.Budget.PerDayUSD, 64)
	st.BudgetExhausted = perDay > 0 && spend.Total() >= perDay
	t := metav1.NewTime(now)
	st.LastEvaluated = &t
	return nil
}

// benchmarkProviders lists the ModelProviders a benchmark names besides its
// Agents' selectors: the judge and the shadow arms.
func benchmarkProviders(b *agentorcav1alpha1.RoutingBenchmark) []string {
	var names []string
	if b.Spec.Judge != nil {
		names = append(names, b.Spec.Judge.ProviderRef)
	}
	if b.Spec.Shadow != nil {
		for _, p := range b.Spec.Shadow.Providers {
			if !slices.Contains(names, p) {
				names = append(names, p)
			}
		}
	}
	return names
}

func benchmarkInterval(b *agentorcav1alpha1.RoutingBenchmark) time.Duration {
	if e := b.Spec.Evaluation; e != nil && e.Interval != nil && e.Interval.Duration > 0 {
		return e.Interval.Duration
	}
	return defaultBenchmarkInterval
}

func benchmarkWindow(b *agentorcav1alpha1.RoutingBenchmark) time.Duration {
	if e := b.Spec.Evaluation; e != nil && e.Window != nil && e.Window.Duration > 0 {
		return e.Window.Duration
	}
	return defaultBenchmarkWindow
}

func hasCondition(conds []metav1.Condition, t string) bool {
	return slices.ContainsFunc(conds, func(c metav1.Condition) bool { return c.Type == t })
}

// assessBenchmark turns what the benchmark names, and whether it exists, into
// conditions. scope is what it covers; missing holds the providers the
// benchmark names that do not exist. JudgeIsArm and PostgresUnavailable are
// returned only when they hold.
func assessBenchmark(b *agentorcav1alpha1.RoutingBenchmark, scope benchmarkScope, missing map[string]bool, postgres bool) []metav1.Condition {
	ready := metav1.Condition{
		Type: agentorcav1alpha1.BenchmarkConditionReady, Status: metav1.ConditionTrue,
		Reason: reasonBenchmarkReady, Message: "the benchmark is valid",
	}
	var out []metav1.Condition
	fail := func(reason, msg string) {
		if ready.Status == metav1.ConditionFalse {
			return // the first problem is the one reported
		}
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, reason, clipMessage(msg)
	}

	switch {
	case len(scope.MissingAgents) > 0:
		fail(reasonAgentNotFound, fmt.Sprintf("Agent not found in namespace %s: %s", b.Namespace, strings.Join(scope.MissingAgents, ", ")))
	case len(scope.Agents) == 0:
		fail(reasonNoAgentsMatched, fmt.Sprintf("no Agent in namespace %s matches the benchmark", b.Namespace))
	case len(scope.MissingSelectors) > 0:
		fail(reasonSelectorNotFound, fmt.Sprintf("ModelSelector not found for: %s", strings.Join(scope.MissingSelectors, ", ")))
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		fail(reasonBenchmarkProviderGap, fmt.Sprintf("ModelProvider not found in namespace %s: %s", b.Namespace, strings.Join(names, ", ")))
	}
	if !postgres {
		fail(reasonPostgresUnavailable, "the operator runs without Postgres, so shadow results cannot be stored or evaluated")
		out = append(out, metav1.Condition{
			Type: agentorcav1alpha1.BenchmarkConditionPostgresUnavailable, Status: metav1.ConditionTrue,
			Reason: reasonPostgresUnavailable, Message: "start the operator with a Postgres connection",
		})
	}

	if judgeIsArm(b, scope) {
		out = append(out, metav1.Condition{
			Type: agentorcav1alpha1.BenchmarkConditionJudgeIsArm, Status: metav1.ConditionTrue, Reason: reasonJudgeIsArm,
			Message: fmt.Sprintf("judge %q is also one of the models compared, so it may favour its own answers; pick another model", b.Spec.Judge.ProviderRef),
		})
	}
	return append([]metav1.Condition{ready}, out...)
}

// judgeIsArm reports whether the judge is one of the models being compared: a
// provider any covered Agent routes to, or one of the listed shadow providers.
func judgeIsArm(b *agentorcav1alpha1.RoutingBenchmark, scope benchmarkScope) bool {
	if b.Spec.Judge == nil {
		return false
	}
	j := b.Spec.Judge.ProviderRef
	if b.Spec.Shadow != nil && slices.Contains(b.Spec.Shadow.Providers, j) {
		return true
	}
	return scope.Providers[j]
}

// benchmarksForProvider maps a ModelProvider change to the benchmarks that
// name it as a judge or shadow arm.
func (r *RoutingBenchmarkReconciler) benchmarksForProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.benchmarksWhere(ctx, obj.GetNamespace(), func(b *agentorcav1alpha1.RoutingBenchmark) bool {
		return slices.Contains(benchmarkProviders(b), obj.GetName())
	})
}

// benchmarksForAgent maps an Agent change to the benchmarks that cover it.
func (r *RoutingBenchmarkReconciler) benchmarksForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	agent, ok := obj.(*agentorcav1alpha1.Agent)
	if !ok {
		return nil
	}
	return r.benchmarksWhere(ctx, agent.Namespace, func(b *agentorcav1alpha1.RoutingBenchmark) bool {
		return b.Covers(agent) || slices.Contains(b.Spec.AgentRefs, agent.Name)
	})
}

// benchmarksForSelector maps a ModelSelector change to the benchmarks that
// cover an Agent routing through it.
func (r *RoutingBenchmarkReconciler) benchmarksForSelector(ctx context.Context, obj client.Object) []reconcile.Request {
	var agents agentorcav1alpha1.AgentList
	if err := r.List(ctx, &agents, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var users []*agentorcav1alpha1.Agent
	for i := range agents.Items {
		if agents.Items[i].Spec.ModelSelectorRef == obj.GetName() {
			users = append(users, &agents.Items[i])
		}
	}
	return r.benchmarksWhere(ctx, obj.GetNamespace(), func(b *agentorcav1alpha1.RoutingBenchmark) bool {
		return slices.ContainsFunc(users, b.Covers)
	})
}

func (r *RoutingBenchmarkReconciler) benchmarksWhere(ctx context.Context, namespace string, want func(*agentorcav1alpha1.RoutingBenchmark) bool) []reconcile.Request {
	var list agentorcav1alpha1.RoutingBenchmarkList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if want(&list.Items[i]) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *RoutingBenchmarkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// An Agent's labels decide whether a benchmark covers it, and labels do not
	// change its generation.
	agentChanged := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.RoutingBenchmark{}).
		Watches(&agentorcav1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.benchmarksForAgent), builder.WithPredicates(agentChanged)).
		Watches(&agentorcav1alpha1.ModelSelector{}, handler.EnqueueRequestsFromMapFunc(r.benchmarksForSelector)).
		Watches(&agentorcav1alpha1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.benchmarksForProvider)).
		Named("routingbenchmark").
		Complete(r)
}

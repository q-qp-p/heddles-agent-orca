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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/routingbench"
)

// Labels and annotations of a RoutingRecommendation.
const (
	labelRecSelector = "agentorca.io/selector"
	labelRecDetector = "agentorca.io/detector"
	// annotationRecKey groups the recommendations that are versions of one
	// finding: same selector, detector, Agent and category.
	annotationRecKey = "agentorca.io/dedupe-key"

	// annotationRoutingRecommendations set to "disabled" on a ModelSelector turns
	// its recommendations off.
	annotationRoutingRecommendations = "agentorca.io/routing-recommendations"
	routingRecommendationsDisabled   = "disabled"
)

// strategyRuleBasedDefault is what a selector with no strategy set routes by.
const strategyRuleBasedDefault = "rule-based"

// RoutingEvaluationReconciler looks at how each ModelSelector has been routing
// and writes what it finds as RoutingRecommendations. It never changes a
// selector: a person reads a recommendation and applies it, or asks the
// operator to.
//
// Every selector is evaluated from passive data (providers that fail, ratings).
// A RoutingBenchmark covering its Agents adds the replayed samples that let it
// say whether a cheaper or dearer model would have done.
type RoutingEvaluationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Source loads the evidence. nil means the operator has no Postgres.
	Source routingbench.Source
	// Disabled turns every evaluation off (--routing-recommendations=false).
	Disabled bool

	// Recorder emits events; optional.
	Recorder record.EventRecorder

	// now is the clock; nil means time.Now.
	now func() time.Time
}

// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=routingrecommendations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=routingrecommendations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=routingrecommendations/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelselectors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=routingbenchmarks,verbs=get;list;watch

// Reconcile evaluates one ModelSelector.
func (r *RoutingEvaluationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sel agentorcav1alpha1.ModelSelector
	if err := r.Get(ctx, req.NamespacedName, &sel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.Disabled || r.Source == nil || sel.Annotations[annotationRoutingRecommendations] == routingRecommendationsDisabled {
		return ctrl.Result{}, nil
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}

	var agents agentorcav1alpha1.AgentList
	if err := r.List(ctx, &agents, client.InNamespace(sel.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing Agents: %w", err)
	}
	var users []*agentorcav1alpha1.Agent
	for i := range agents.Items {
		if agents.Items[i].Spec.ModelSelectorRef == sel.Name {
			users = append(users, &agents.Items[i])
		}
	}
	bench, err := r.benchmarkFor(ctx, sel.Namespace, users)
	if err != nil {
		return ctrl.Result{}, err
	}
	interval := benchmarkIntervalOrDefault(bench)
	if len(users) == 0 {
		return ctrl.Result{RequeueAfter: interval}, r.publish(ctx, &sel, bench, nil)
	}

	cluster, err := r.cluster(ctx, &sel, users)
	if err != nil {
		return ctrl.Result{}, err
	}
	query := routingbench.Query{
		Namespace: sel.Namespace, Agents: agentNames(users),
		Since: now.Add(-benchmarkWindowOrDefault(bench)), Now: now,
	}
	obs, err := r.Source.LoadObservations(ctx, query)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("loading routing evidence for %s: %w", sel.Name, err)
	}
	findings := routingbench.Analyze(obs, cluster, query, optionsOf(bench))
	if err := r.record(ctx, &sel, bench, findings, now); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.checkApplied(ctx, &sel, bench, cluster, now); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// benchmarkFor returns the first benchmark, by name, that covers any of the
// Agents, or nil.
func (r *RoutingEvaluationReconciler) benchmarkFor(ctx context.Context, namespace string, users []*agentorcav1alpha1.Agent) (*agentorcav1alpha1.RoutingBenchmark, error) {
	var list agentorcav1alpha1.RoutingBenchmarkList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing RoutingBenchmarks: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for i := range list.Items {
		if slices.ContainsFunc(users, list.Items[i].Covers) {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// cluster describes the selector, its Agents and its providers' prices to the
// detectors.
func (r *RoutingEvaluationReconciler) cluster(ctx context.Context, sel *agentorcav1alpha1.ModelSelector, users []*agentorcav1alpha1.Agent) (routingbench.Cluster, error) {
	rs := routingbench.Selector{Name: sel.Name, Strategy: sel.Spec.Strategy, FallbackChain: slices.Clone(sel.Spec.FallbackChain)}
	if rs.Strategy == "" {
		rs.Strategy = strategyRuleBasedDefault
	}
	for _, p := range sel.Spec.Providers {
		rs.Providers = append(rs.Providers, routingbench.ProviderWeight{Name: p.Name, Weight: p.Weight, RoutingHint: p.RoutingHint})
	}
	if len(sel.Spec.CapabilityRouting) > 0 {
		rs.CapabilityRouting = maps.Clone(sel.Spec.CapabilityRouting)
	}
	if sel.Spec.Switchyard != nil && len(sel.Spec.Switchyard.Raw) > 0 {
		if err := json.Unmarshal(sel.Spec.Switchyard.Raw, &rs.Switchyard); err != nil {
			slog.Warn("ModelSelector has a switchyard block that is not a JSON object; not evaluating its algorithm",
				"selector", sel.Name, "err", err)
			rs.Switchyard = nil
		}
	}
	c := routingbench.Cluster{
		Selectors:     map[string]routingbench.Selector{sel.Name: rs},
		AgentSelector: make(map[string]string, len(users)),
		ProviderCost:  map[string]float64{},
	}
	for _, a := range users {
		c.AgentSelector[a.Name] = sel.Name
	}
	for _, name := range providerNames(sel) {
		var mp agentorcav1alpha1.ModelProvider
		switch err := r.Get(ctx, client.ObjectKey{Namespace: sel.Namespace, Name: name}, &mp); {
		case err == nil:
			c.ProviderCost[name] = (parseFloat(mp.Spec.Constraints.CostPerMillionInputTokens) + parseFloat(mp.Spec.Constraints.CostPerMillionOutputTokens)) / 1e6
		case !apierrors.IsNotFound(err):
			return c, fmt.Errorf("getting ModelProvider %q: %w", name, err)
		}
	}
	return c, nil
}

// optionsOf reads the evidence bar from a benchmark; without one the defaults
// apply and only the passive detectors can fire.
func optionsOf(b *agentorcav1alpha1.RoutingBenchmark) routingbench.Options {
	var o routingbench.Options
	if b == nil {
		return o
	}
	if e := b.Spec.Evaluation; e != nil {
		o.MinSamples = int(e.MinSamples)
		if e.ConfidenceLevel != "" {
			o.Confidence = parseFloat(e.ConfidenceLevel)
		}
	}
	if t := b.Spec.Thresholds; t != nil {
		o.NonInferiorityMargin = parseFloat(t.NonInferiorityMargin)
		o.MinNetWinDelta = parseFloat(t.MinNetWinDelta)
		o.DeadProviderErrorRate = parseFloat(t.DeadProviderErrorRate)
		if t.MaxUSDPerNetWin != "" {
			v := parseFloat(t.MaxUSDPerNetWin)
			o.MaxUSDPerNetWin = &v
		}
	}
	return o
}

func benchmarkIntervalOrDefault(b *agentorcav1alpha1.RoutingBenchmark) time.Duration {
	if b == nil {
		return defaultBenchmarkInterval
	}
	return benchmarkInterval(b)
}

func benchmarkWindowOrDefault(b *agentorcav1alpha1.RoutingBenchmark) time.Duration {
	if b == nil {
		return defaultBenchmarkWindow
	}
	return benchmarkWindow(b)
}

func agentNames(as []*agentorcav1alpha1.Agent) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name)
	}
	sort.Strings(out)
	return out
}

// record turns findings into RoutingRecommendations and updates the selector's
// status.
func (r *RoutingEvaluationReconciler) record(ctx context.Context, sel *agentorcav1alpha1.ModelSelector, bench *agentorcav1alpha1.RoutingBenchmark, findings []routingbench.Finding, now time.Time) error {
	var existing agentorcav1alpha1.RoutingRecommendationList
	if err := r.List(ctx, &existing, client.InNamespace(sel.Namespace), client.MatchingLabels{labelRecSelector: sel.Name}); err != nil {
		return fmt.Errorf("listing RoutingRecommendations: %w", err)
	}
	byName := map[string]*agentorcav1alpha1.RoutingRecommendation{}
	for i := range existing.Items {
		byName[existing.Items[i].Name] = &existing.Items[i]
	}

	seen := map[string]bool{}
	keys := map[string]string{} // dedupe key -> current recommendation name
	for _, f := range findings {
		name := recommendationName(sel.Name, f)
		seen[name] = true
		keys[recommendationKey(sel.Name, f)] = name
		rec := byName[name]
		if rec == nil {
			created, err := r.create(ctx, sel, bench, f, name, now)
			if err != nil {
				return err
			}
			byName[name] = created
			continue
		}
		if err := r.refresh(ctx, rec, f, now); err != nil {
			return err
		}
	}

	// A finding that has a newer version supersedes its older proposals; one
	// that no longer appears at all expires. Only proposals are touched here:
	// anything a person has acted on keeps its own life.
	for _, rec := range byName {
		if rec.Status.Phase != agentorcav1alpha1.RecommendationProposed || seen[rec.Name] {
			continue
		}
		phase, msg := agentorcav1alpha1.RecommendationExpired, "the evidence no longer supports it"
		if newer, ok := keys[rec.Annotations[annotationRecKey]]; ok && newer != rec.Name {
			phase, msg = agentorcav1alpha1.RecommendationSuperseded, "replaced by "+newer
		}
		if err := r.setPhase(ctx, rec, phase, msg); err != nil {
			return err
		}
	}
	return r.publish(ctx, sel, bench, byName)
}

// checkApplied judges the changes that sent work to the cheaper model, one window
// after each was applied: did the answers get worse? Those are the changes that
// can quietly do harm, so they are the ones checked. A check that finds too
// little to say is tried again, once, a window later. A regression is recorded
// and never acted on here; the recommendation's controller asks for a rollback
// where the benchmark allows it.
func (r *RoutingEvaluationReconciler) checkApplied(ctx context.Context, sel *agentorcav1alpha1.ModelSelector, bench *agentorcav1alpha1.RoutingBenchmark, cluster routingbench.Cluster, now time.Time) error {
	var list agentorcav1alpha1.RoutingRecommendationList
	if err := r.List(ctx, &list, client.InNamespace(sel.Namespace), client.MatchingLabels{labelRecSelector: sel.Name}); err != nil {
		return fmt.Errorf("listing RoutingRecommendations: %w", err)
	}
	window, opts := benchmarkWindowOrDefault(bench), optionsOf(bench)
	for i := range list.Items {
		rec := &list.Items[i]
		if rec.Status.Phase != agentorcav1alpha1.RecommendationApplied || rec.Status.PostApply != nil ||
			rec.Spec.Detector != agentorcav1alpha1.DetectorOverspend || rec.Status.AppliedAt == nil || rec.Spec.Agent == "" {
			continue
		}
		since := rec.Status.AppliedAt.Time
		age := now.Sub(since)
		if age < window {
			continue
		}
		obs, err := r.Source.LoadObservations(ctx, routingbench.Query{
			Namespace: sel.Namespace, Agents: []string{rec.Spec.Agent}, Since: since, Now: now,
		})
		if err != nil {
			return fmt.Errorf("loading evidence for the check of %s: %w", rec.Name, err)
		}
		verdict, w := routingbench.AnalyzeDowngrade(obs, cluster, rec.Spec.Agent, rec.Spec.Category, opts)
		if verdict == routingbench.VerdictInconclusive && age < 2*window {
			continue
		}
		evaluated := metav1.NewTime(now)
		base := rec.DeepCopy()
		rec.Status.PostApply = &agentorcav1alpha1.PostApplyStatus{
			Verdict: verdict, EvaluatedAt: &evaluated, Message: postApplyMessage(verdict, w, opts),
		}
		if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("recording the check of %s: %w", rec.Name, err)
		}
		if verdict == routingbench.VerdictRegressed && r.Recorder != nil {
			r.Recorder.Eventf(rec, corev1.EventTypeWarning, "Regressed", "%s", rec.Status.PostApply.Message)
			r.Recorder.Eventf(sel, corev1.EventTypeWarning, "RoutingRegressed", "after applying %s: %s", rec.Name, rec.Status.PostApply.Message)
		}
	}
	return nil
}

func postApplyMessage(verdict string, w routingbench.Worth, opts routingbench.Options) string {
	switch verdict {
	case routingbench.VerdictRegressed:
		return fmt.Sprintf("since the change, across %d judged pairs the capable model's answers win by %.0f%% (at least %.0f%%), more than the %.0f%% allowed",
			w.N, w.NetWin*100, w.NetWinCI.Lo*100, optsMargin(opts)*100)
	case routingbench.VerdictConfirmed:
		return fmt.Sprintf("since the change, across %d judged pairs the capable model's answers win by %.0f%% (at most %.0f%%): not worse than the %.0f%% allowed",
			w.N, w.NetWin*100, w.NetWinCI.Hi*100, optsMargin(opts)*100)
	}
	return fmt.Sprintf("too few judged pairs since the change to say (%d)", w.N)
}

func optsMargin(o routingbench.Options) float64 {
	if o.NonInferiorityMargin > 0 {
		return o.NonInferiorityMargin
	}
	return 0.05
}

// recommendationKey groups the versions of one finding.
func recommendationKey(selector string, f routingbench.Finding) string {
	return shortHash(selector, f.Detector, f.Agent, f.Category)
}

// recommendationName is stable for one finding with one proposed change: the
// same finding next evaluation updates it, a different patch is a new object.
func recommendationName(selector string, f routingbench.Finding) string {
	change := f.Reason
	if f.Patch != nil {
		change = string(f.Patch.Forward)
	}
	prefix := strings.ToLower(selector)
	if len(prefix) > 100 {
		prefix = prefix[:100]
	}
	return fmt.Sprintf("%s-%s-%s", prefix, strings.ToLower(f.Detector), shortHash(selector, f.Detector, f.Agent, f.Category, change))
}

func shortHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])[:10]
}

func (r *RoutingEvaluationReconciler) create(ctx context.Context, sel *agentorcav1alpha1.ModelSelector, bench *agentorcav1alpha1.RoutingBenchmark, f routingbench.Finding, name string, now time.Time) (*agentorcav1alpha1.RoutingRecommendation, error) {
	rec := &agentorcav1alpha1.RoutingRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: sel.Namespace,
			Labels:      map[string]string{labelRecSelector: sel.Name, labelRecDetector: f.Detector},
			Annotations: map[string]string{annotationRecKey: recommendationKey(sel.Name, f)},
		},
		Spec: agentorcav1alpha1.RoutingRecommendationSpec{
			SelectorRef: sel.Name, Detector: f.Detector, Agent: f.Agent, Category: f.Category,
			Summary: f.Summary, Reason: f.Reason, Caveat: f.Caveat, BaseGeneration: sel.Generation,
			Action: agentorcav1alpha1.RecommendationActionNone,
		},
	}
	if bench != nil && f.Detector != routingbenchDeadProvider {
		rec.Spec.BenchmarkRef = bench.Name
	}
	if f.Patch != nil {
		rec.Spec.Patch, rec.Spec.RollbackPatch = string(f.Patch.Forward), string(f.Patch.Rollback)
	}
	if err := ctrl.SetControllerReference(sel, rec, r.Scheme); err != nil {
		return nil, fmt.Errorf("owning RoutingRecommendation: %w", err)
	}
	if err := r.Create(ctx, rec); err != nil {
		if apierrors.IsAlreadyExists(err) { // another evaluation got there first
			return rec, nil
		}
		return nil, fmt.Errorf("creating RoutingRecommendation %s: %w", name, err)
	}
	if err := r.setEvidence(ctx, rec, f, now, agentorcav1alpha1.RecommendationProposed, "found by the "+f.Detector+" check"); err != nil {
		return nil, err
	}
	return rec, nil
}

// routingbenchDeadProvider is the detector that needs no benchmark.
const routingbenchDeadProvider = agentorcav1alpha1.DetectorDeadProvider

// refresh updates the evidence of a recommendation that is still current. One
// that a person has acted on, or that was dismissed, is left alone; an expired
// one whose finding came back becomes a proposal again.
func (r *RoutingEvaluationReconciler) refresh(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, f routingbench.Finding, now time.Time) error {
	switch rec.Status.Phase {
	case agentorcav1alpha1.RecommendationProposed:
		return r.setEvidence(ctx, rec, f, now, "", "")
	case agentorcav1alpha1.RecommendationExpired:
		return r.setEvidence(ctx, rec, f, now, agentorcav1alpha1.RecommendationProposed, "the evidence supports it again")
	}
	return nil
}

// setEvidence stores the finding's evidence on the recommendation and, when
// phase is not empty, moves it to that phase. The copy the patch is computed
// against is taken first, so the phase is part of the patch.
func (r *RoutingEvaluationReconciler) setEvidence(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, f routingbench.Finding, now time.Time, phase, message string) error {
	raw, err := json.Marshal(f.Evidence)
	if err != nil {
		return fmt.Errorf("encoding evidence: %w", err)
	}
	base := rec.DeepCopy()
	if phase != "" {
		rec.Status.Phase, rec.Status.Message = phase, message
	}
	rec.Status.Evidence = &runtime.RawExtension{Raw: raw}
	rec.Status.Corroboration = f.Corroboration
	t := metav1.NewTime(now)
	rec.Status.LastEvaluated = &t
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
	}
	return nil
}

func (r *RoutingEvaluationReconciler) setPhase(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, phase, msg string) error {
	base := rec.DeepCopy()
	rec.Status.Phase, rec.Status.Message = phase, msg
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
	}
	return nil
}

// publish records on the selector which benchmark covers it and how many
// recommendations are open.
func (r *RoutingEvaluationReconciler) publish(ctx context.Context, sel *agentorcav1alpha1.ModelSelector, bench *agentorcav1alpha1.RoutingBenchmark, recs map[string]*agentorcav1alpha1.RoutingRecommendation) error {
	open := int32(0)
	for _, rec := range recs {
		if rec.Status.Phase == agentorcav1alpha1.RecommendationProposed || rec.Status.Phase == agentorcav1alpha1.RecommendationPendingApproval {
			open++
		}
	}
	ref := ""
	if bench != nil {
		ref = bench.Name
	}
	if sel.Status.OpenRecommendations == open && sel.Status.BenchmarkRef == ref {
		return nil
	}
	base := sel.DeepCopy()
	sel.Status.OpenRecommendations, sel.Status.BenchmarkRef = open, ref
	if err := r.Status().Patch(ctx, sel, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating ModelSelector status: %w", err)
	}
	return nil
}

// selectorsOfAgent maps an Agent change to the selector it routes through.
func (r *RoutingEvaluationReconciler) selectorsOfAgent(_ context.Context, obj client.Object) []reconcile.Request {
	agent, ok := obj.(*agentorcav1alpha1.Agent)
	if !ok || agent.Spec.ModelSelectorRef == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: agent.Namespace, Name: agent.Spec.ModelSelectorRef}}}
}

// selectorsOfBenchmark maps a benchmark change to the selectors of the Agents it
// covers.
func (r *RoutingEvaluationReconciler) selectorsOfBenchmark(ctx context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*agentorcav1alpha1.RoutingBenchmark)
	if !ok {
		return nil
	}
	var agents agentorcav1alpha1.AgentList
	if err := r.List(ctx, &agents, client.InNamespace(b.Namespace)); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var reqs []reconcile.Request
	for i := range agents.Items {
		a := &agents.Items[i]
		if b.Covers(a) && !seen[a.Spec.ModelSelectorRef] {
			seen[a.Spec.ModelSelectorRef] = true
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.ModelSelectorRef}})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *RoutingEvaluationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// The selector's status is written here, so only a change to its spec or to
	// the opt-out annotation starts an evaluation; its own status does not.
	selectorChanged := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{})
	agentChanged := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("routingevaluation") //nolint:staticcheck
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.ModelSelector{}, builder.WithPredicates(selectorChanged)).
		Watches(&agentorcav1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.selectorsOfAgent), builder.WithPredicates(agentChanged)).
		Watches(&agentorcav1alpha1.RoutingBenchmark{}, handler.EnqueueRequestsFromMapFunc(r.selectorsOfBenchmark), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("routingevaluation").
		Complete(r)
}

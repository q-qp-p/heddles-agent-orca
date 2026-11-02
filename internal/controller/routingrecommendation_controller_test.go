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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

const (
	recName      = "adaptive-overspend-1234567890"
	routePatch   = `{"spec":{"capabilityRouting":{"stage:chat":"weak"}}}`
	routeInverse = `{"spec":{"capabilityRouting":{"stage:chat":null}}}`
)

type recWorld struct {
	r     *RoutingRecommendationReconciler
	c     client.Client
	appr  *memApprovals
	notes *recordingNotifier
	rec   *record.FakeRecorder
	now   time.Time
}

// newRecWorld has a selector at generation 3 covered by a benchmark in the given
// mode, and a proposed recommendation for it that has not been acted on.
func newRecWorld(t *testing.T, mode string, mutateRec func(*agentorcav1alpha1.RoutingRecommendation)) *recWorld {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sel := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: evalSel, Namespace: "ns", Generation: 3},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Strategy:          "rule-based",
			Providers:         []agentorcav1alpha1.ProviderWeight{{Name: evalStrong, Weight: 50}, {Name: evalWeak, Weight: 50}},
			CapabilityRouting: map[string]string{"capable": evalStrong},
		},
		Status: agentorcav1alpha1.ModelSelectorStatus{BenchmarkRef: evalBench},
	}
	bench := &agentorcav1alpha1.RoutingBenchmark{
		ObjectMeta: metav1.ObjectMeta{Name: evalBench, Namespace: "ns"},
		Spec: agentorcav1alpha1.RoutingBenchmarkSpec{
			Mode: mode, AgentRefs: []string{benchAgent}, ApproverGroups: []string{"admins"},
			Budget: agentorcav1alpha1.BenchmarkBudget{PerDayUSD: "1"},
		},
	}
	rec := &agentorcav1alpha1.RoutingRecommendation{
		ObjectMeta: metav1.ObjectMeta{Name: recName, Namespace: "ns", UID: "uid-1"},
		Spec: agentorcav1alpha1.RoutingRecommendationSpec{
			SelectorRef: evalSel, BenchmarkRef: evalBench, Detector: agentorcav1alpha1.DetectorOverspend,
			Agent: benchAgent, Category: evalChat, Summary: "weak answers as well", BaseGeneration: 3,
			Patch: routePatch, RollbackPatch: routeInverse, Action: agentorcav1alpha1.RecommendationActionNone,
		},
		Status: agentorcav1alpha1.RoutingRecommendationStatus{Phase: agentorcav1alpha1.RecommendationProposed},
	}
	if mutateRec != nil {
		mutateRec(rec)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sel, bench, rec).
		WithStatusSubresource(&agentorcav1alpha1.RoutingRecommendation{}, &agentorcav1alpha1.ModelSelector{}).Build()
	w := &recWorld{c: c, appr: &memApprovals{}, notes: &recordingNotifier{}, rec: record.NewFakeRecorder(20), now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	w.r = &RoutingRecommendationReconciler{
		Client: c, Scheme: scheme, Recorder: w.rec, Approvals: w.appr, ApprovalNotifier: w.notes,
		now: func() time.Time { return w.now },
	}
	return w
}

func (w *recWorld) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: recName}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (w *recWorld) get(t *testing.T) agentorcav1alpha1.RoutingRecommendation {
	t.Helper()
	var rec agentorcav1alpha1.RoutingRecommendation
	if err := w.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: recName}, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func (w *recWorld) selector(t *testing.T) agentorcav1alpha1.ModelSelector {
	t.Helper()
	var sel agentorcav1alpha1.ModelSelector
	if err := w.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: evalSel}, &sel); err != nil {
		t.Fatal(err)
	}
	return sel
}

// act changes what the recommendation's spec asks for.
func (w *recWorld) act(t *testing.T, action string) {
	t.Helper()
	rec := w.get(t)
	rec.Spec.Action = action
	if err := w.c.Update(context.Background(), &rec); err != nil {
		t.Fatal(err)
	}
}

func (w *recWorld) decide(t *testing.T, status string) {
	t.Helper()
	if _, err := w.appr.DecideApproval(context.Background(), w.appr.rows[len(w.appr.rows)-1].ID, status, "ada", "because"); err != nil {
		t.Fatal(err)
	}
}

func (w *recWorld) setSelectorGeneration(t *testing.T, gen int64) {
	t.Helper()
	sel := w.selector(t)
	sel.Generation = gen
	if err := w.c.Update(context.Background(), &sel); err != nil {
		t.Fatal(err)
	}
}

// --- asking for approval ----------------------------------------------------

func TestRecommendation_ProposeApplyAsksForApprovalOnItsOwn(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, nil)
	res := w.reconcile(t)
	if res.RequeueAfter != routingApprovalPoll {
		t.Errorf("requeue = %v, want the approval poll", res.RequeueAfter)
	}
	rec := w.get(t)
	if rec.Status.Phase != agentorcav1alpha1.RecommendationPendingApproval || rec.Status.ApprovalID == "" {
		t.Fatalf("status = %+v", rec.Status)
	}
	if len(w.appr.rows) != 1 {
		t.Fatalf("approvals = %d, want 1", len(w.appr.rows))
	}
	a := w.appr.rows[0]
	if a.Kind != postgresql.ApprovalKindRouting || a.RunName != "" || a.Namespace != "ns" || a.SourceKey != "uid-1/apply" ||
		len(a.ApproverGroups) != 1 || a.ApproverGroups[0] != "admins" || !a.ExpiresAt.Equal(w.now.Add(defaultRoutingApprovalTimeout)) {
		t.Errorf("approval = %+v", a)
	}
	var d routingApprovalDetails
	if err := json.Unmarshal(a.Details, &d); err != nil {
		t.Fatal(err)
	}
	if d.Recommendation != recName || d.Selector != evalSel || string(d.Patch) != routePatch || d.BaseGeneration != 3 || !strings.Contains(d.Description, "weak answers as well") {
		t.Errorf("details = %+v", d)
	}
	if len(w.notes.sent) != 1 {
		t.Errorf("approvers were told %d times, want once", len(w.notes.sent))
	}
	// Asking again changes nothing: still one request, and nobody is told twice.
	w.reconcile(t)
	if len(w.appr.rows) != 1 || len(w.notes.sent) != 1 {
		t.Errorf("approvals %d, notifications %d after a second pass", len(w.appr.rows), len(w.notes.sent))
	}
}

func TestRecommendation_ReportOnlyBenchmarkNeverAsksUnlessAPersonDoes(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeReportOnly, nil)
	w.reconcile(t)
	if rec := w.get(t); rec.Status.Phase != agentorcav1alpha1.RecommendationProposed || len(rec.Status.Conditions) != 0 || len(w.appr.rows) != 0 {
		t.Errorf("a recommendation nobody asked about was touched: %+v, approvals %d", rec.Status, len(w.appr.rows))
	}
	w.act(t, agentorcav1alpha1.RecommendationActionRequestApply)
	w.reconcile(t)
	rec := w.get(t)
	c := meta.FindStatusCondition(rec.Status.Conditions, conditionApplyBlocked)
	if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "proposeApply") {
		t.Fatalf("conditions = %+v, want ApplyBlocked that says what is needed", rec.Status.Conditions)
	}
	if rec.Status.Phase != agentorcav1alpha1.RecommendationProposed || len(w.appr.rows) != 0 {
		t.Errorf("phase %q, approvals %d: the operator must not apply on a report-only benchmark", rec.Status.Phase, len(w.appr.rows))
	}
}

func TestRecommendation_ASelectorWithNoBenchmarkCannotBeApplied(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, nil)
	sel := w.selector(t)
	sel.Status.BenchmarkRef = ""
	if err := w.c.Status().Update(context.Background(), &sel); err != nil {
		t.Fatal(err)
	}
	w.act(t, agentorcav1alpha1.RecommendationActionRequestApply)
	w.reconcile(t)
	if c := meta.FindStatusCondition(w.get(t).Status.Conditions, conditionApplyBlocked); c == nil || len(w.appr.rows) != 0 {
		t.Errorf("condition %+v, approvals %d", c, len(w.appr.rows))
	}
}

func TestRecommendation_AReportOnlyFindingHasNothingToApply(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, func(r *agentorcav1alpha1.RoutingRecommendation) {
		r.Spec.Patch, r.Spec.RollbackPatch, r.Spec.Reason = "", "", "a switchyard selector ignores stage routes"
		r.Spec.Action = agentorcav1alpha1.RecommendationActionRequestApply
	})
	w.reconcile(t)
	c := meta.FindStatusCondition(w.get(t).Status.Conditions, conditionApplyBlocked)
	if c == nil || !strings.Contains(c.Message, "switchyard selector ignores stage routes") || len(w.appr.rows) != 0 {
		t.Errorf("condition %+v, approvals %d", c, len(w.appr.rows))
	}
}

func TestRecommendation_WithoutPostgresNothingCanBeApplied(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, func(r *agentorcav1alpha1.RoutingRecommendation) {
		r.Spec.Action = agentorcav1alpha1.RecommendationActionRequestApply
	})
	w.r.Approvals = nil
	w.reconcile(t)
	if c := meta.FindStatusCondition(w.get(t).Status.Conditions, conditionApplyBlocked); c == nil || !strings.Contains(c.Message, "PostgreSQL") {
		t.Errorf("condition = %+v", c)
	}
}

func TestRecommendation_IsStaleIfTheSelectorChangedBeforeApproval(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, nil)
	w.setSelectorGeneration(t, 4)
	w.reconcile(t)
	if rec := w.get(t); rec.Status.Phase != agentorcav1alpha1.RecommendationStale || len(w.appr.rows) != 0 {
		t.Errorf("phase %q, approvals %d; a finding about generation 3 must not ask to change generation 4", rec.Status.Phase, len(w.appr.rows))
	}
}

// --- applying -----------------------------------------------------------------

func TestRecommendation_ApprovedPatchIsAppliedAndRemembered(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, nil)
	w.reconcile(t)
	w.decide(t, postgresql.ApprovalApproved)
	w.reconcile(t)

	sel := w.selector(t)
	if sel.Spec.CapabilityRouting["stage:chat"] != evalWeak || sel.Spec.CapabilityRouting["capable"] != evalStrong {
		t.Errorf("capabilityRouting = %v, want the new route added and the old one kept", sel.Spec.CapabilityRouting)
	}
	if sel.Annotations[annotationLastRecommendation] != recName {
		t.Errorf("annotations = %v, want the recommendation named", sel.Annotations)
	}
	rec := w.get(t)
	st := rec.Status
	if st.Phase != agentorcav1alpha1.RecommendationApplied || st.AppliedAt == nil || st.AppliedRollbackPatch != routeInverse {
		t.Fatalf("status = %+v", st)
	}
	if st.PostApply != nil {
		t.Errorf("post-apply = %+v: an overspend change is checked later, not marked now", st.PostApply)
	}
	if w.appr.rows[0].AppliedAt == nil {
		t.Error("the decision was not marked as having reached its source")
	}
	if len(w.rec.Events) < 2 {
		t.Errorf("events = %d, want one on the selector and one on the recommendation", len(w.rec.Events))
	}
}

func TestRecommendation_OnlyOverspendChangesAreCheckedAfterwards(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, func(r *agentorcav1alpha1.RoutingRecommendation) {
		r.Spec.Detector = agentorcav1alpha1.DetectorUnderpowered
	})
	w.reconcile(t)
	w.decide(t, postgresql.ApprovalApproved)
	w.reconcile(t)
	if pa := w.get(t).Status.PostApply; pa == nil || pa.Verdict != agentorcav1alpha1.PostApplyNotAssessed {
		t.Errorf("post-apply = %+v, want NotAssessed", pa)
	}
}

func TestRecommendation_ApprovedButOutdatedIsNotApplied(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, nil)
	w.reconcile(t)
	w.setSelectorGeneration(t, 4) // somebody edited the selector while the request waited
	w.decide(t, postgresql.ApprovalApproved)
	w.reconcile(t)
	rec := w.get(t)
	if rec.Status.Phase != agentorcav1alpha1.RecommendationStale || !strings.Contains(rec.Status.Message, "not applied") {
		t.Errorf("status = %+v", rec.Status)
	}
	if _, ok := w.selector(t).Spec.CapabilityRouting["stage:chat"]; ok {
		t.Error("an approved but outdated change was applied anyway")
	}
	if w.appr.rows[0].AppliedAt == nil {
		t.Error("the decision was not marked as having reached its source")
	}
}

func TestRecommendation_RejectedAndExpiredApprovalsApplyNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		decide func(*testing.T, *recWorld)
		phase  string
	}{
		"rejected":               {func(t *testing.T, w *recWorld) { w.decide(t, postgresql.ApprovalRejected) }, agentorcav1alpha1.RecommendationRejected},
		"withdrawn":              {func(t *testing.T, w *recWorld) { w.decide(t, postgresql.ApprovalWithdrawn) }, agentorcav1alpha1.RecommendationExpired},
		"nobody decided in time": {func(_ *testing.T, w *recWorld) { w.now = w.now.Add(defaultRoutingApprovalTimeout + time.Hour) }, agentorcav1alpha1.RecommendationExpired},
	} {
		w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, nil)
		w.reconcile(t)
		tc.decide(t, w)
		w.reconcile(t)
		if rec := w.get(t); rec.Status.Phase != tc.phase {
			t.Errorf("%s: phase = %q, want %q (%s)", name, rec.Status.Phase, tc.phase, rec.Status.Message)
		}
		if _, ok := w.selector(t).Spec.CapabilityRouting["stage:chat"]; ok {
			t.Errorf("%s: the selector was changed", name)
		}
	}
}

func TestRecommendation_RejectionNamesWhoAndWhy(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, nil)
	w.reconcile(t)
	w.decide(t, postgresql.ApprovalRejected)
	w.reconcile(t)
	if msg := w.get(t).Status.Message; !strings.Contains(msg, "ada") || !strings.Contains(msg, "because") {
		t.Errorf("message = %q", msg)
	}
}

// If the selector was patched but the status write failed, the next pass must
// recognise its own work and not call the moved generation a conflict.
func TestRecommendation_RecognisesAChangeItAlreadyMade(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, nil)
	w.reconcile(t)
	w.decide(t, postgresql.ApprovalApproved)
	sel := w.selector(t)
	sel.Generation = 4
	sel.Annotations = map[string]string{annotationLastRecommendation: recName}
	sel.Spec.CapabilityRouting["stage:chat"] = evalWeak
	if err := w.c.Update(context.Background(), &sel); err != nil {
		t.Fatal(err)
	}
	w.reconcile(t)
	rec := w.get(t)
	if rec.Status.Phase != agentorcav1alpha1.RecommendationApplied || rec.Status.AppliedGeneration != 4 || !strings.Contains(rec.Status.Message, "already been made") {
		t.Errorf("status = %+v", rec.Status)
	}
}

// A patch that reaches outside spec is refused whatever was approved.
func TestRecommendation_RefusesAPatchThatTouchesMoreThanSpec(t *testing.T) {
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, func(r *agentorcav1alpha1.RoutingRecommendation) {
		r.Spec.Patch = `{"metadata":{"labels":{"x":"y"}},"spec":{"capabilityRouting":{"stage:chat":"weak"}}}`
	})
	w.reconcile(t)
	w.decide(t, postgresql.ApprovalApproved)
	res := w.reconcile(t)
	rec := w.get(t)
	c := meta.FindStatusCondition(rec.Status.Conditions, conditionApplyFailed)
	if c == nil || !strings.Contains(c.Message, "only spec may be changed") || rec.Status.Phase != agentorcav1alpha1.RecommendationPendingApproval {
		t.Fatalf("condition %+v phase %q", c, rec.Status.Phase)
	}
	if res.RequeueAfter != applyRetry {
		t.Errorf("requeue = %v, want a retry later", res.RequeueAfter)
	}
	if sel := w.selector(t); sel.Labels["x"] != "" || len(sel.Spec.CapabilityRouting) != 1 {
		t.Errorf("the selector was changed: %+v", sel.ObjectMeta)
	}
}

// --- rolling back -------------------------------------------------------------

// applied builds a world where the change has been applied, as it is after the
// steps above.
func appliedWorld(t *testing.T, mode string) *recWorld {
	t.Helper()
	w := newRecWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply, nil)
	w.reconcile(t)
	w.decide(t, postgresql.ApprovalApproved)
	w.reconcile(t)
	if w.get(t).Status.Phase != agentorcav1alpha1.RecommendationApplied {
		t.Fatalf("setup: not applied: %+v", w.get(t).Status)
	}
	if mode != agentorcav1alpha1.BenchmarkModeProposeApply {
		var b agentorcav1alpha1.RoutingBenchmark
		if err := w.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: evalBench}, &b); err != nil {
			t.Fatal(err)
		}
		b.Spec.Mode = mode
		if err := w.c.Update(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func TestRecommendation_RollbackNeedsApprovalThenRestoresTheSelector(t *testing.T) {
	w := appliedWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply)
	w.act(t, agentorcav1alpha1.RecommendationActionRollback)
	w.reconcile(t)
	if len(w.appr.rows) != 2 || w.appr.rows[1].Kind != postgresql.ApprovalKindRoutingRollback || w.appr.rows[1].SourceKey != "uid-1/rollback" {
		t.Fatalf("approvals = %+v", w.appr.rows)
	}
	rec := w.get(t)
	if rec.Status.Phase != agentorcav1alpha1.RecommendationApplied || !meta.IsStatusConditionTrue(rec.Status.Conditions, conditionRollbackRequested) {
		t.Fatalf("status = %+v", rec.Status)
	}
	if w.selector(t).Spec.CapabilityRouting["stage:chat"] != evalWeak {
		t.Fatal("the change was undone before anyone approved that")
	}

	w.decide(t, postgresql.ApprovalApproved)
	w.reconcile(t)
	if _, ok := w.selector(t).Spec.CapabilityRouting["stage:chat"]; ok {
		t.Error("the route is still there after the rollback")
	}
	if sel := w.selector(t); sel.Spec.CapabilityRouting["capable"] != evalStrong || sel.Annotations[annotationLastRecommendation] != rollbackAnnotationPrefix+recName {
		t.Errorf("selector = %+v", sel)
	}
	if rec := w.get(t); rec.Status.Phase != agentorcav1alpha1.RecommendationRolledBack || w.appr.rows[1].AppliedAt == nil {
		t.Errorf("status = %+v", rec.Status)
	}
}

func TestRecommendation_RollbackIsRefusedIfTheSelectorChangedSinceTheApply(t *testing.T) {
	w := appliedWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply)
	w.act(t, agentorcav1alpha1.RecommendationActionRollback)
	w.reconcile(t)
	w.setSelectorGeneration(t, 9) // somebody else changed it after the apply
	w.decide(t, postgresql.ApprovalApproved)
	w.reconcile(t)
	rec := w.get(t)
	c := meta.FindStatusCondition(rec.Status.Conditions, conditionRollbackBlocked)
	if c == nil || !strings.Contains(c.Message, "undo it by hand") || rec.Status.Phase != agentorcav1alpha1.RecommendationApplied {
		t.Fatalf("condition %+v phase %q", c, rec.Status.Phase)
	}
	if w.selector(t).Spec.CapabilityRouting["stage:chat"] != evalWeak {
		t.Error("a rollback that would also have undone somebody else's change was made")
	}
}

func TestRecommendation_ARejectedRollbackLeavesItAppliedAndIsNotAskedAgain(t *testing.T) {
	w := appliedWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply)
	w.act(t, agentorcav1alpha1.RecommendationActionRollback)
	w.reconcile(t)
	w.decide(t, postgresql.ApprovalRejected)
	w.reconcile(t)
	w.reconcile(t) // the action is still "rollback"; the answer was no
	rec := w.get(t)
	c := meta.FindStatusCondition(rec.Status.Conditions, conditionRollbackRequested)
	if rec.Status.Phase != agentorcav1alpha1.RecommendationApplied || c == nil || c.Status != metav1.ConditionFalse || len(w.appr.rows) != 2 {
		t.Errorf("phase %q condition %+v approvals %d", rec.Status.Phase, c, len(w.appr.rows))
	}
}

func TestRecommendation_RollbackOnAReportOnlyBenchmarkIsBlocked(t *testing.T) {
	w := appliedWorld(t, agentorcav1alpha1.BenchmarkModeReportOnly)
	w.act(t, agentorcav1alpha1.RecommendationActionRollback)
	w.reconcile(t)
	if c := meta.FindStatusCondition(w.get(t).Status.Conditions, conditionRollbackBlocked); c == nil || len(w.appr.rows) != 1 {
		t.Errorf("condition %+v approvals %d", c, len(w.appr.rows))
	}
}

// A regression found by the check afterwards asks for a rollback by itself, but
// never makes one.
func TestRecommendation_ARegressionAsksForARollbackOnlyWhereApplyingIsAllowed(t *testing.T) {
	w := appliedWorld(t, agentorcav1alpha1.BenchmarkModeProposeApply)
	w.markRegressed(t)
	w.reconcile(t)
	if len(w.appr.rows) != 2 || w.appr.rows[1].Kind != postgresql.ApprovalKindRoutingRollback {
		t.Fatalf("approvals = %+v, want a rollback request", w.appr.rows)
	}
	if w.selector(t).Spec.CapabilityRouting["stage:chat"] != evalWeak {
		t.Error("the change was rolled back without anyone approving it")
	}

	quiet := appliedWorld(t, agentorcav1alpha1.BenchmarkModeReportOnly)
	quiet.markRegressed(t)
	quiet.reconcile(t)
	if len(quiet.appr.rows) != 1 || len(quiet.get(t).Status.Conditions) != 0 {
		t.Errorf("approvals %d: on a report-only benchmark a regression is only recorded", len(quiet.appr.rows))
	}
}

// markRegressed records the verdict the check made after a window would.
func (w *recWorld) markRegressed(t *testing.T) {
	t.Helper()
	rec := w.get(t)
	now := metav1.NewTime(w.now)
	rec.Status.PostApply = &agentorcav1alpha1.PostApplyStatus{Verdict: agentorcav1alpha1.PostApplyRegressed, EvaluatedAt: &now}
	if err := w.c.Status().Update(context.Background(), &rec); err != nil {
		t.Fatal(err)
	}
}

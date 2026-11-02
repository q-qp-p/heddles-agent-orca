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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/routingbench"
)

// Conditions of a RoutingRecommendation that a person has acted on.
const (
	// conditionApplyBlocked says why a request to apply could not be raised.
	conditionApplyBlocked = "ApplyBlocked"
	// conditionApplyFailed says the approved patch was refused by the cluster.
	conditionApplyFailed = "ApplyFailed"
	// conditionRollbackRequested is true while a rollback awaits its approval.
	conditionRollbackRequested = "RollbackRequested"
	// conditionRollbackBlocked says why an approved rollback was not made.
	conditionRollbackBlocked = "RollbackBlocked"
)

const (
	// annotationLastRecommendation names the recommendation that last changed a
	// selector, so a change that was made but not recorded is recognised and not
	// mistaken for a selector somebody else edited.
	annotationLastRecommendation = "agentorca.io/last-recommendation"
	rollbackAnnotationPrefix     = "rollback:"

	// defaultRoutingApprovalTimeout is how long an approver has to decide.
	defaultRoutingApprovalTimeout = 7 * 24 * time.Hour
	// routingApprovalPoll is how often a pending approval is looked at.
	routingApprovalPoll = 30 * time.Second
	// applyRetry is how long to wait before retrying a patch the cluster refused.
	applyRetry = 5 * time.Minute

	approvalSuffixApply    = "apply"
	approvalSuffixRollback = "rollback"
)

// RoutingRecommendationReconciler takes a RoutingRecommendation through an
// approval to a change in its ModelSelector, and back again. It only acts when a
// RoutingBenchmark covering the selector's Agents has mode proposeApply and
// names approver groups: otherwise a recommendation is for reading, and the
// operator never changes the selector.
type RoutingRecommendationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Approvals stores the approval requests (PostgreSQL). nil means applying is
	// not available.
	Approvals ApprovalStore
	// ApprovalNotifier tells approvers about a new request. Optional.
	ApprovalNotifier ApprovalNotifier

	// now is the clock; nil means time.Now.
	now func() time.Time
}

// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=routingrecommendations,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=routingrecommendations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelselectors,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// routingApprovalDetails is what an approver sees.
type routingApprovalDetails struct {
	Description    string          `json:"description"`
	Recommendation string          `json:"recommendation"`
	Selector       string          `json:"selector"`
	Detector       string          `json:"detector"`
	Agent          string          `json:"agent,omitempty"`
	Category       string          `json:"category,omitempty"`
	Summary        string          `json:"summary"`
	Caveat         string          `json:"caveat,omitempty"`
	Patch          json.RawMessage `json:"patch"`
	Evidence       json.RawMessage `json:"evidence,omitempty"`
	BaseGeneration int64           `json:"baseGeneration"`
}

// Reconcile advances one recommendation.
func (r *RoutingRecommendationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec agentorcav1alpha1.RoutingRecommendation
	if err := r.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	switch rec.Status.Phase {
	case agentorcav1alpha1.RecommendationProposed:
		return r.proposed(ctx, &rec)
	case agentorcav1alpha1.RecommendationPendingApproval:
		return r.pending(ctx, &rec)
	case agentorcav1alpha1.RecommendationApplied:
		return r.applied(ctx, &rec)
	}
	return ctrl.Result{}, nil
}

func (r *RoutingRecommendationReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// governing returns the selector a recommendation is about and the benchmark
// that covers it, either of which may be nil.
func (r *RoutingRecommendationReconciler) governing(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation) (*agentorcav1alpha1.ModelSelector, *agentorcav1alpha1.RoutingBenchmark, error) {
	var sel agentorcav1alpha1.ModelSelector
	if err := r.Get(ctx, client.ObjectKey{Namespace: rec.Namespace, Name: rec.Spec.SelectorRef}, &sel); err != nil {
		return nil, nil, client.IgnoreNotFound(err)
	}
	if sel.Status.BenchmarkRef == "" {
		return &sel, nil, nil
	}
	var bench agentorcav1alpha1.RoutingBenchmark
	if err := r.Get(ctx, client.ObjectKey{Namespace: rec.Namespace, Name: sel.Status.BenchmarkRef}, &bench); err != nil {
		return &sel, nil, client.IgnoreNotFound(err)
	}
	return &sel, &bench, nil
}

// mayApply reports whether the benchmark lets recommendations be applied: its
// mode is proposeApply and it names who may approve.
func mayApply(b *agentorcav1alpha1.RoutingBenchmark) bool {
	return b != nil && b.Spec.Mode == agentorcav1alpha1.BenchmarkModeProposeApply && len(b.Spec.ApproverGroups) > 0
}

const needsProposeApply = "applying needs a RoutingBenchmark with mode proposeApply and approverGroups that covers this selector's Agents; read the patch and apply it yourself"

// proposed asks for approval when a person has asked to apply, or when the
// benchmark is in proposeApply mode, which raises a request for every
// recommendation that has a patch.
func (r *RoutingRecommendationReconciler) proposed(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation) (ctrl.Result, error) {
	sel, bench, err := r.governing(ctx, rec)
	if err != nil || sel == nil {
		return ctrl.Result{}, err
	}
	asked := rec.Spec.Action == agentorcav1alpha1.RecommendationActionRequestApply
	if !asked && (!mayApply(bench) || rec.Spec.Patch == "") {
		return ctrl.Result{}, nil
	}
	switch {
	case rec.Spec.Patch == "":
		return ctrl.Result{}, r.block(ctx, rec, conditionApplyBlocked, asked, "this recommendation is report only: "+rec.Spec.Reason)
	case !mayApply(bench):
		return ctrl.Result{}, r.block(ctx, rec, conditionApplyBlocked, asked, needsProposeApply)
	case r.Approvals == nil:
		return ctrl.Result{}, r.block(ctx, rec, conditionApplyBlocked, asked, "approvals need PostgreSQL")
	case sel.Generation != rec.Spec.BaseGeneration:
		return ctrl.Result{}, r.setPhase(ctx, rec, agentorcav1alpha1.RecommendationStale,
			fmt.Sprintf("the selector changed (generation %d, was %d) after this was found", sel.Generation, rec.Spec.BaseGeneration))
	}
	stored, err := r.request(ctx, rec, sel, bench, postgresql.ApprovalKindRouting, approvalSuffixApply)
	if err != nil {
		return ctrl.Result{}, err
	}
	base := rec.DeepCopy()
	rec.Status.Phase = agentorcav1alpha1.RecommendationPendingApproval
	rec.Status.Message = "waiting for an approver"
	rec.Status.ApprovalID = stored.ID
	meta.RemoveStatusCondition(&rec.Status.Conditions, conditionApplyBlocked)
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
	}
	return ctrl.Result{RequeueAfter: routingApprovalPoll}, nil
}

// block records why something was not done. Only a request a person made is
// reported: a recommendation nobody asked about stays quiet.
func (r *RoutingRecommendationReconciler) block(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, cond string, asked bool, why string) error {
	if !asked {
		return nil
	}
	if c := meta.FindStatusCondition(rec.Status.Conditions, cond); c != nil && c.Status == metav1.ConditionTrue && c.Message == why {
		return nil
	}
	base := rec.DeepCopy()
	meta.SetStatusCondition(&rec.Status.Conditions, metav1.Condition{Type: cond, Status: metav1.ConditionTrue, Reason: "NotAllowed", Message: why, ObservedGeneration: rec.Generation})
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
	}
	return nil
}

// request raises the approval, or reads back the one already raised: a request
// is one row per recommendation and kind, so asking again never makes a second.
func (r *RoutingRecommendationReconciler) request(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, sel *agentorcav1alpha1.ModelSelector, bench *agentorcav1alpha1.RoutingBenchmark, kind, suffix string) (postgresql.Approval, error) {
	patch := rec.Spec.Patch
	what := "Apply"
	if suffix == approvalSuffixRollback {
		patch, what = rec.Status.AppliedRollbackPatch, "Undo"
	}
	d := routingApprovalDetails{
		Description:    fmt.Sprintf("%s this change to ModelSelector %s: %s", what, sel.Name, rec.Spec.Summary),
		Recommendation: rec.Name, Selector: sel.Name, Detector: rec.Spec.Detector, Agent: rec.Spec.Agent, Category: rec.Spec.Category,
		Summary: rec.Spec.Summary, Caveat: rec.Spec.Caveat, Patch: json.RawMessage(patch), BaseGeneration: rec.Spec.BaseGeneration,
	}
	if rec.Status.Evidence != nil {
		d.Evidence = json.RawMessage(rec.Status.Evidence.Raw)
	}
	details, err := json.Marshal(d)
	if err != nil {
		return postgresql.Approval{}, fmt.Errorf("encoding the approval: %w", err)
	}
	stored, err := r.Approvals.RequestApproval(ctx, postgresql.Approval{
		Tenant: sel.Labels["agentorca.io/tenant"], Namespace: rec.Namespace,
		Kind: kind, SourceKey: fmt.Sprintf("%s/%s", rec.UID, suffix),
		Summary:        fmt.Sprintf("%s routing change for %s: %s", what, sel.Name, rec.Spec.Summary),
		Details:        details,
		ApproverGroups: bench.Spec.ApproverGroups,
		ExpiresAt:      r.clock().Add(defaultRoutingApprovalTimeout),
	})
	if err != nil {
		return stored, fmt.Errorf("raising the approval for %s: %w", rec.Name, err)
	}
	if stored.Created && stored.Status == postgresql.ApprovalPending && r.ApprovalNotifier != nil {
		if err := r.ApprovalNotifier.NotifyApproval(ctx, stored); err != nil {
			log := ctrl.LoggerFrom(ctx)
			log.Error(err, "notifying approvers", "approval", stored.ID)
		}
	}
	return stored, nil
}

// settle expires an approval nobody decided in time and returns its final state.
func (r *RoutingRecommendationReconciler) settle(ctx context.Context, stored postgresql.Approval) postgresql.Approval {
	if stored.Status == postgresql.ApprovalPending && r.clock().After(stored.ExpiresAt) {
		if decided, err := r.Approvals.DecideApproval(ctx, stored.ID, postgresql.ApprovalExpired, "", "no decision in time"); err == nil {
			return decided
		}
	}
	return stored
}

func decision(a postgresql.Approval) string {
	s := a.Status
	if a.DecidedBy != "" {
		s += " by " + a.DecidedBy
	}
	if a.Reason != "" {
		s += ": " + a.Reason
	}
	return s
}

// pending looks at the approval of a recommendation waiting to be applied.
func (r *RoutingRecommendationReconciler) pending(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation) (ctrl.Result, error) {
	sel, bench, err := r.governing(ctx, rec)
	if err != nil || sel == nil {
		return ctrl.Result{}, err
	}
	if r.Approvals == nil || bench == nil {
		return ctrl.Result{RequeueAfter: routingApprovalPoll}, nil
	}
	stored, err := r.request(ctx, rec, sel, bench, postgresql.ApprovalKindRouting, approvalSuffixApply)
	if err != nil {
		return ctrl.Result{}, err
	}
	stored = r.settle(ctx, stored)
	switch stored.Status {
	case postgresql.ApprovalPending:
		return ctrl.Result{RequeueAfter: routingApprovalPoll}, nil
	case postgresql.ApprovalApproved:
		return r.apply(ctx, rec, sel, stored)
	case postgresql.ApprovalRejected:
		return ctrl.Result{}, r.finish(ctx, rec, stored, agentorcav1alpha1.RecommendationRejected, "rejected: "+decision(stored))
	default: // expired or withdrawn
		return ctrl.Result{}, r.finish(ctx, rec, stored, agentorcav1alpha1.RecommendationExpired, "the approval was "+decision(stored))
	}
}

// finish records the end of a recommendation and that the decision reached it.
func (r *RoutingRecommendationReconciler) finish(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, a postgresql.Approval, phase, msg string) error {
	if err := r.setPhase(ctx, rec, phase, msg); err != nil {
		return err
	}
	if err := r.Approvals.MarkApprovalApplied(ctx, a.ID); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "marking the routing decision applied", "approval", a.ID)
	}
	return nil
}

func (r *RoutingRecommendationReconciler) setPhase(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, phase, msg string) error {
	base := rec.DeepCopy()
	rec.Status.Phase, rec.Status.Message = phase, msg
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
	}
	return nil
}

func (r *RoutingRecommendationReconciler) event(obj runtime.Object, eventType, reason, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, eventType, reason, format, args...)
	}
}

// patchSelector applies a merge patch to a selector, conditional on the version
// that was read, and stamps which recommendation made the change. The patch may
// only touch spec.
func (r *RoutingRecommendationReconciler) patchSelector(ctx context.Context, sel *agentorcav1alpha1.ModelSelector, patch, stamp string) error {
	var body map[string]any
	if err := json.Unmarshal([]byte(patch), &body); err != nil {
		return fmt.Errorf("the patch is not a JSON object: %w", err)
	}
	for k := range body {
		if k != "spec" {
			return fmt.Errorf("the patch touches %q; only spec may be changed", k)
		}
	}
	body["metadata"] = map[string]any{
		"resourceVersion": sel.ResourceVersion,
		"annotations":     map[string]any{annotationLastRecommendation: stamp},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return r.Patch(ctx, sel, client.RawPatch(types.MergePatchType, raw))
}

// apply makes an approved change, unless the selector moved on since it was found.
func (r *RoutingRecommendationReconciler) apply(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, sel *agentorcav1alpha1.ModelSelector, a postgresql.Approval) (ctrl.Result, error) {
	if sel.Annotations[annotationLastRecommendation] == rec.Name {
		// An earlier pass changed the selector but did not get to record it.
		return ctrl.Result{}, r.recordApplied(ctx, rec, sel, a, rec.Spec.RollbackPatch, "the change had already been made")
	}
	if sel.Generation != rec.Spec.BaseGeneration {
		return ctrl.Result{}, r.finish(ctx, rec, a, agentorcav1alpha1.RecommendationStale,
			fmt.Sprintf("approved, but the selector changed (generation %d, was %d) after this was found, so it was not applied", sel.Generation, rec.Spec.BaseGeneration))
	}
	current, err := json.Marshal(map[string]any{"spec": sel.Spec})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("encoding the selector: %w", err)
	}
	rollback, err := routingbench.InverseMergePatch(current, []byte(rec.Spec.Patch))
	if err != nil {
		return ctrl.Result{}, r.applyFailed(ctx, rec, err)
	}
	if err := r.patchSelector(ctx, sel, rec.Spec.Patch, rec.Name); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil // the selector moved while we were reading it; look again
		}
		r.event(rec, corev1.EventTypeWarning, "ApplyFailed", "applying the approved patch to %s: %v", sel.Name, err)
		return ctrl.Result{RequeueAfter: applyRetry}, r.applyFailed(ctx, rec, err)
	}
	return ctrl.Result{}, r.recordApplied(ctx, rec, sel, a, string(rollback), "approved by "+a.DecidedBy)
}

func (r *RoutingRecommendationReconciler) applyFailed(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, cause error) error {
	base := rec.DeepCopy()
	meta.SetStatusCondition(&rec.Status.Conditions, metav1.Condition{
		Type: conditionApplyFailed, Status: metav1.ConditionTrue, Reason: "Refused", Message: clipMessage(cause.Error()), ObservedGeneration: rec.Generation,
	})
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
	}
	return nil
}

func (r *RoutingRecommendationReconciler) recordApplied(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, sel *agentorcav1alpha1.ModelSelector, a postgresql.Approval, rollback, why string) error {
	base := rec.DeepCopy()
	now := metav1.NewTime(r.clock())
	rec.Status.Phase = agentorcav1alpha1.RecommendationApplied
	rec.Status.Message = "applied: " + why
	rec.Status.AppliedAt, rec.Status.AppliedGeneration, rec.Status.AppliedRollbackPatch = &now, sel.Generation, rollback
	if rec.Spec.Detector != agentorcav1alpha1.DetectorOverspend {
		// Only a change that sends work to the cheaper model can quietly make
		// answers worse, so only that one is checked afterwards.
		rec.Status.PostApply = &agentorcav1alpha1.PostApplyStatus{
			Verdict: agentorcav1alpha1.PostApplyNotAssessed, EvaluatedAt: &now, Message: "this kind of change is not checked afterwards",
		}
	}
	meta.RemoveStatusCondition(&rec.Status.Conditions, conditionApplyFailed)
	meta.RemoveStatusCondition(&rec.Status.Conditions, conditionApplyBlocked)
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
	}
	if err := r.Approvals.MarkApprovalApplied(ctx, a.ID); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "marking the routing decision applied", "approval", a.ID)
	}
	r.event(sel, corev1.EventTypeNormal, "RoutingRecommendationApplied", "applied %s: %s", rec.Name, rec.Spec.Summary)
	r.event(rec, corev1.EventTypeNormal, "Applied", "patched ModelSelector %s", sel.Name)
	return nil
}

// applied handles a change that is in effect: a rollback, if one is asked for or
// the check afterwards found the change made things worse.
func (r *RoutingRecommendationReconciler) applied(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation) (ctrl.Result, error) {
	sel, bench, err := r.governing(ctx, rec)
	if err != nil || sel == nil {
		return ctrl.Result{}, err
	}
	requested := meta.IsStatusConditionTrue(rec.Status.Conditions, conditionRollbackRequested)
	asked := rec.Spec.Action == agentorcav1alpha1.RecommendationActionRollback
	regressed := rec.Status.PostApply != nil && rec.Status.PostApply.Verdict == agentorcav1alpha1.PostApplyRegressed
	if !requested && !asked && (!regressed || !mayApply(bench)) {
		return ctrl.Result{}, nil
	}
	// The approval row is the memory of a rollback that was already decided: a
	// rejected one is not asked for again however long the action stays set.
	switch {
	case rec.Status.AppliedRollbackPatch == "":
		return ctrl.Result{}, r.block(ctx, rec, conditionRollbackBlocked, asked, "there is no rollback patch to apply")
	case !mayApply(bench):
		return ctrl.Result{}, r.block(ctx, rec, conditionRollbackBlocked, asked, needsProposeApply)
	case r.Approvals == nil:
		return ctrl.Result{}, r.block(ctx, rec, conditionRollbackBlocked, asked, "approvals need PostgreSQL")
	}
	stored, err := r.request(ctx, rec, sel, bench, postgresql.ApprovalKindRoutingRollback, approvalSuffixRollback)
	if err != nil {
		return ctrl.Result{}, err
	}
	stored = r.settle(ctx, stored)
	switch stored.Status {
	case postgresql.ApprovalPending:
		if !requested {
			base := rec.DeepCopy()
			meta.SetStatusCondition(&rec.Status.Conditions, metav1.Condition{
				Type: conditionRollbackRequested, Status: metav1.ConditionTrue, Reason: "WaitingForApproval",
				Message: "waiting for an approver to undo this change", ObservedGeneration: rec.Generation,
			})
			if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
			}
		}
		return ctrl.Result{RequeueAfter: routingApprovalPoll}, nil
	case postgresql.ApprovalApproved:
		return ctrl.Result{}, r.rollBack(ctx, rec, sel, stored)
	default: // rejected, expired or withdrawn: it stays applied
		base := rec.DeepCopy()
		meta.SetStatusCondition(&rec.Status.Conditions, metav1.Condition{
			Type: conditionRollbackRequested, Status: metav1.ConditionFalse, Reason: "NotApproved",
			Message: "the rollback was " + decision(stored), ObservedGeneration: rec.Generation,
		})
		if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
		}
		if err := r.Approvals.MarkApprovalApplied(ctx, stored.ID); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "marking the rollback decision applied", "approval", stored.ID)
		}
		return ctrl.Result{}, nil
	}
}

// rollBack undoes an applied change, unless the selector was changed again since:
// restoring then would also undo that change.
func (r *RoutingRecommendationReconciler) rollBack(ctx context.Context, rec *agentorcav1alpha1.RoutingRecommendation, sel *agentorcav1alpha1.ModelSelector, a postgresql.Approval) error {
	stamp := rollbackAnnotationPrefix + rec.Name
	if sel.Annotations[annotationLastRecommendation] != stamp {
		if sel.Generation != rec.Status.AppliedGeneration {
			base := rec.DeepCopy()
			meta.SetStatusCondition(&rec.Status.Conditions, metav1.Condition{
				Type: conditionRollbackBlocked, Status: metav1.ConditionTrue, Reason: "SelectorChanged",
				Message: fmt.Sprintf("the selector changed (generation %d, was %d) after this was applied; undo it by hand with: %s",
					sel.Generation, rec.Status.AppliedGeneration, rec.Status.AppliedRollbackPatch),
				ObservedGeneration: rec.Generation,
			})
			meta.SetStatusCondition(&rec.Status.Conditions, metav1.Condition{
				Type: conditionRollbackRequested, Status: metav1.ConditionFalse, Reason: "Blocked", Message: "the selector changed since", ObservedGeneration: rec.Generation,
			})
			if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
				return fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
			}
			if err := r.Approvals.MarkApprovalApplied(ctx, a.ID); err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "marking the rollback decision applied", "approval", a.ID)
			}
			return nil
		}
		if err := r.patchSelector(ctx, sel, rec.Status.AppliedRollbackPatch, stamp); err != nil {
			r.event(rec, corev1.EventTypeWarning, "RollbackFailed", "undoing the change in %s: %v", sel.Name, err)
			return fmt.Errorf("rolling back %s: %w", rec.Name, err)
		}
	}
	base := rec.DeepCopy()
	rec.Status.Phase, rec.Status.Message = agentorcav1alpha1.RecommendationRolledBack, "rolled back: "+decision(a)
	meta.RemoveStatusCondition(&rec.Status.Conditions, conditionRollbackRequested)
	meta.RemoveStatusCondition(&rec.Status.Conditions, conditionRollbackBlocked)
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating RoutingRecommendation %s: %w", rec.Name, err)
	}
	if err := r.Approvals.MarkApprovalApplied(ctx, a.ID); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "marking the rollback decision applied", "approval", a.ID)
	}
	r.event(sel, corev1.EventTypeNormal, "RoutingRecommendationRolledBack", "undid %s: %s", rec.Name, rec.Spec.Summary)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RoutingRecommendationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("routingrecommendation") //nolint:staticcheck
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.RoutingRecommendation{}).
		Named("routingrecommendation").
		Complete(r)
}

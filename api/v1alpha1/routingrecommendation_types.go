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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Recommendation phases. A recommendation is Proposed until a person acts on it.
const (
	// RecommendationProposed is a finding nobody has asked to apply.
	RecommendationProposed = "Proposed"
	// RecommendationPendingApproval waits for an approver to accept the change.
	RecommendationPendingApproval = "PendingApproval"
	// RecommendationApplied has been patched into the ModelSelector.
	RecommendationApplied = "Applied"
	// RecommendationRejected was refused by an approver.
	RecommendationRejected = "Rejected"
	// RecommendationStale was overtaken: the ModelSelector changed after the
	// finding, so the patch no longer describes what it was measured against.
	RecommendationStale = "Stale"
	// RecommendationSuperseded was replaced by a newer finding for the same
	// selector, detector and category.
	RecommendationSuperseded = "Superseded"
	// RecommendationRolledBack was applied and then undone.
	RecommendationRolledBack = "RolledBack"
	// RecommendationExpired no longer has evidence, or waited too long.
	RecommendationExpired = "Expired"
)

// Recommendation actions a person can ask for, by changing spec.action.
const (
	// RecommendationActionNone leaves the recommendation for reading.
	RecommendationActionNone = "none"
	// RecommendationActionRequestApply asks for the patch to be applied, which
	// raises an approval request when the benchmark's mode is proposeApply.
	RecommendationActionRequestApply = "requestApply"
	// RecommendationActionRollback asks for an applied patch to be undone.
	RecommendationActionRollback = "rollback"
)

// Recommendation detectors, as routingbench names them.
const (
	DetectorOverspend      = "overspend"
	DetectorUnderpowered   = "underpowered"
	DetectorThreshold      = "threshold"
	DetectorDeadProvider   = "deadProvider"
	DetectorStrategySwitch = "strategySwitch"
)

// Post-apply verdicts.
const (
	// PostApplyConfirmed means the evidence after the change did not contradict it.
	PostApplyConfirmed = "Confirmed"
	// PostApplyRegressed means the evidence after the change says it made routing worse.
	PostApplyRegressed = "Regressed"
	// PostApplyInconclusive means there was not enough evidence after the change.
	PostApplyInconclusive = "Inconclusive"
	// PostApplyNotAssessed means this kind of change is not checked afterwards.
	PostApplyNotAssessed = "NotAssessed"
)

// RoutingRecommendationSpec is a change to a ModelSelector the evidence supports.
// The operator writes everything but action, and it cannot be edited: a finding
// with a different patch is a new recommendation. The rules below compare
// presence as well as value, so a field left out at creation (a report-only
// recommendation has no patch) cannot be added later.
// +kubebuilder:validation:XValidation:rule="self.selectorRef == oldSelf.selectorRef",message="selectorRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.detector == oldSelf.detector",message="detector is immutable"
// +kubebuilder:validation:XValidation:rule="self.summary == oldSelf.summary",message="summary is immutable"
// +kubebuilder:validation:XValidation:rule="self.baseGeneration == oldSelf.baseGeneration",message="baseGeneration is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.benchmarkRef) == has(oldSelf.benchmarkRef) && (!has(self.benchmarkRef) || self.benchmarkRef == oldSelf.benchmarkRef)",message="benchmarkRef is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.agent) == has(oldSelf.agent) && (!has(self.agent) || self.agent == oldSelf.agent)",message="agent is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.category) == has(oldSelf.category) && (!has(self.category) || self.category == oldSelf.category)",message="category is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.patch) == has(oldSelf.patch) && (!has(self.patch) || self.patch == oldSelf.patch)",message="patch is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.rollbackPatch) == has(oldSelf.rollbackPatch) && (!has(self.rollbackPatch) || self.rollbackPatch == oldSelf.rollbackPatch)",message="rollbackPatch is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.reason) == has(oldSelf.reason) && (!has(self.reason) || self.reason == oldSelf.reason)",message="reason is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.caveat) == has(oldSelf.caveat) && (!has(self.caveat) || self.caveat == oldSelf.caveat)",message="caveat is immutable"
type RoutingRecommendationSpec struct {
	// SelectorRef names the ModelSelector, in this namespace, the change is for.
	// +kubebuilder:validation:MinLength=1
	SelectorRef string `json:"selectorRef"`

	// BenchmarkRef names the RoutingBenchmark whose samples back the finding.
	// Empty when it rests on passive data alone, such as a dead provider.
	// +optional
	BenchmarkRef string `json:"benchmarkRef,omitempty"`

	// Detector is the check that found it.
	// +kubebuilder:validation:Enum=overspend;underpowered;threshold;deadProvider;strategySwitch
	Detector string `json:"detector"`

	// Agent is the Agent whose work the finding is about. Empty for a finding
	// about the selector as a whole.
	// +optional
	Agent string `json:"agent,omitempty"`

	// Category is the task category it is about, such as "stage:tool-exec".
	// +optional
	Category string `json:"category,omitempty"`

	// Summary says what the evidence shows, in a sentence.
	Summary string `json:"summary"`

	// Patch is an RFC 7386 merge patch against the ModelSelector, as JSON. Empty
	// when the finding is report-only; reason says why.
	// +optional
	Patch string `json:"patch,omitempty"`

	// RollbackPatch undoes patch from the selector as it was when the finding was
	// made. It is re-derived when the patch is applied.
	// +optional
	RollbackPatch string `json:"rollbackPatch,omitempty"`

	// Reason says why there is no patch.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Caveat is something to weigh even though there is a patch.
	// +optional
	Caveat string `json:"caveat,omitempty"`

	// BaseGeneration is the ModelSelector generation the finding was made against.
	BaseGeneration int64 `json:"baseGeneration"`

	// Action is what a person asks for: none, requestApply or rollback. It is the
	// only field that can change.
	// +kubebuilder:validation:Enum=none;requestApply;rollback
	// +kubebuilder:default=none
	// +optional
	Action string `json:"action,omitempty"`
}

// PostApplyStatus is the check made one window after a change was applied.
type PostApplyStatus struct {
	// Verdict is Confirmed, Regressed, Inconclusive or NotAssessed.
	Verdict string `json:"verdict"`
	// EvaluatedAt is when the check ran.
	// +optional
	EvaluatedAt *metav1.Time `json:"evaluatedAt,omitempty"`
	// Message says what the check found.
	// +optional
	Message string `json:"message,omitempty"`
}

// RoutingRecommendationStatus is the observed state of a recommendation.
type RoutingRecommendationStatus struct {
	// Phase is where the recommendation is in its life.
	// +kubebuilder:validation:Enum=Proposed;PendingApproval;Applied;Rejected;Stale;Superseded;RolledBack;Expired
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message says why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// Evidence is the numbers behind the finding, refreshed on every evaluation:
	// sample counts, net win with its interval, cost per win, projected savings.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Evidence *runtime.RawExtension `json:"evidence,omitempty"`

	// Corroboration lists independent signals that agree with the finding.
	// +optional
	Corroboration []string `json:"corroboration,omitempty"`

	// LastEvaluated is when the evidence was last refreshed.
	// +optional
	LastEvaluated *metav1.Time `json:"lastEvaluated,omitempty"`

	// ApprovalID identifies the approval request raised for the change.
	// +optional
	ApprovalID string `json:"approvalID,omitempty"`

	// AppliedAt and AppliedGeneration say when and as which ModelSelector
	// generation the patch took effect.
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`
	// +optional
	AppliedGeneration int64 `json:"appliedGeneration,omitempty"`

	// AppliedRollbackPatch undoes the change from the selector as it was just
	// before it was applied.
	// +optional
	AppliedRollbackPatch string `json:"appliedRollbackPatch,omitempty"`

	// PostApply is the check made one window after the change.
	// +optional
	PostApply *PostApplyStatus `json:"postApply,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Selector",type=string,JSONPath=`.spec.selectorRef`
// +kubebuilder:printcolumn:name="Detector",type=string,JSONPath=`.spec.detector`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agent`
// +kubebuilder:printcolumn:name="Category",type=string,JSONPath=`.spec.category`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RoutingRecommendation is a change to a ModelSelector that benchmark and
// passive evidence support, with the patch that makes it and the one that undoes
// it. The operator proposes; a person decides.
type RoutingRecommendation struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RoutingRecommendationSpec   `json:"spec,omitempty"`
	Status RoutingRecommendationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RoutingRecommendationList contains a list of RoutingRecommendation.
type RoutingRecommendationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RoutingRecommendation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RoutingRecommendation{}, &RoutingRecommendationList{})
}

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
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	syengine "github.com/heddles/agent-orca/internal/router/switchyard"
)

// ModelSelector condition types.
const (
	// ConditionProvidersResolved says every provider the spec names exists.
	ConditionProvidersResolved = "ProvidersResolved"
	// ConditionSwitchyardValid says the Switchyard algorithm is served and accepted.
	// It is only present for strategy switchyard.
	ConditionSwitchyardValid = "SwitchyardValid"
	// ConditionReady says everything above holds.
	ConditionReady = "Ready"
)

// Reasons of the ModelSelector conditions.
const (
	reasonAllResolved          = "AllResolved"
	reasonProvidersMissing     = "ProvidersMissing"
	reasonInvalidReference     = "InvalidReference"
	reasonAlgorithmValid       = "Valid"
	reasonMissingAlgorithm     = "MissingAlgorithm"
	reasonUnsupportedAlgorithm = "UnsupportedAlgorithm"
	reasonInvalidAlgorithm     = "InvalidAlgorithm"
	reasonMissingRole          = "MissingRole"
	reasonNotApplicable        = "NotApplicable"
	reasonChecksPassed         = "ChecksPassed"
	reasonChecksFailed         = "ChecksFailed"
)

// maxConditionMessage bounds a condition message; Switchyard's own errors can be long.
const maxConditionMessage = 1000

// ModelSelectorReconciler reconciles a ModelSelector object: it checks that the
// providers and roles the spec names exist, validates a Switchyard algorithm, and
// reports the result in status. It never blocks a run: AgentRuns read the spec
// directly, and the model-router falls back to the rule router when a
// Switchyard algorithm cannot decide. The conditions say why it did.
type ModelSelectorReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// validate checks a Switchyard algorithm against the provider names it may
	// target. nil means syengine.ValidateAlgorithm; tests substitute it.
	validate func(spec json.RawMessage, targets []string) error
}

// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelselectors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelselectors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelselectors/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelproviders,verbs=get;list;watch

// Reconcile checks one ModelSelector and publishes the result to its status.
func (r *ModelSelectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sel agentorcav1alpha1.ModelSelector
	if err := r.Get(ctx, req.NamespacedName, &sel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	found := map[string]*agentorcav1alpha1.ModelProvider{}
	for _, name := range referencedProviders(&sel) {
		var mp agentorcav1alpha1.ModelProvider
		switch err := r.Get(ctx, client.ObjectKey{Namespace: sel.Namespace, Name: name}, &mp); {
		case err == nil:
			found[name] = &mp
		case client.IgnoreNotFound(err) != nil:
			return ctrl.Result{}, fmt.Errorf("getting ModelProvider %q: %w", name, err)
		}
	}

	assessment := assessSelector(&sel, found, r.algorithmResult(&sel))

	patch := client.MergeFrom(sel.DeepCopy())
	sel.Status.ObservedGeneration = sel.Generation
	sel.Status.ActiveProviders = assessment.active
	now := metav1.Now()
	for _, c := range assessment.conditions {
		c.ObservedGeneration = sel.Generation
		c.LastTransitionTime = now
		setCondition(&sel.Status.Conditions, c)
	}
	if !assessment.switchyard {
		meta.RemoveStatusCondition(&sel.Status.Conditions, ConditionSwitchyardValid)
	}
	if err := r.Status().Patch(ctx, &sel, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching ModelSelector status: %w", err)
	}
	return ctrl.Result{}, nil
}

// algorithmResult validates the Switchyard algorithm of sel, or reuses the
// result recorded for this generation: validating compiles the Switchyard guest,
// which is worth doing once per spec change, not once per provider event.
func (r *ModelSelectorReconciler) algorithmResult(sel *agentorcav1alpha1.ModelSelector) algorithmOutcome {
	if sel.Spec.Strategy != strategySwitchyard {
		return algorithmOutcome{}
	}
	if sel.Spec.Switchyard == nil || len(sel.Spec.Switchyard.Raw) == 0 {
		return algorithmOutcome{reason: reasonMissingAlgorithm, message: "spec.switchyard is required for strategy switchyard"}
	}
	if sel.Status.ObservedGeneration == sel.Generation {
		if c := meta.FindStatusCondition(sel.Status.Conditions, ConditionSwitchyardValid); c != nil && algorithmReasons[c.Reason] {
			return algorithmOutcome{reason: c.Reason, message: c.Message}
		}
	}
	validate := r.validate
	if validate == nil {
		validate = syengine.ValidateAlgorithm
	}
	err := validate(sel.Spec.Switchyard.Raw, providerNames(sel))
	switch {
	case err == nil:
		return algorithmOutcome{reason: reasonAlgorithmValid}
	case errors.Is(err, syengine.ErrUnsupported):
		return algorithmOutcome{reason: reasonUnsupportedAlgorithm, message: clipMessage(err.Error())}
	default:
		return algorithmOutcome{reason: reasonInvalidAlgorithm, message: clipMessage(err.Error())}
	}
}

// algorithmReasons are the reasons that record the outcome of validating the
// algorithm itself (as opposed to the roles around it).
var algorithmReasons = map[string]bool{
	reasonAlgorithmValid: true, reasonUnsupportedAlgorithm: true, reasonInvalidAlgorithm: true, reasonMissingAlgorithm: true,
}

// algorithmOutcome is the result of validating a Switchyard algorithm; the zero
// value means it was not validated (the strategy is not switchyard).
type algorithmOutcome struct {
	reason  string
	message string
}

// selectorAssessment is what a ModelSelector's status should say.
type selectorAssessment struct {
	conditions []metav1.Condition
	active     []string
	// switchyard is whether the SwitchyardValid condition applies.
	switchyard bool
}

// providerNames are the providers a Switchyard algorithm may target: the
// selector's providers and its fallback chain.
func providerNames(sel *agentorcav1alpha1.ModelSelector) []string {
	var names []string
	for _, pw := range sel.Spec.Providers {
		if !slices.Contains(names, pw.Name) {
			names = append(names, pw.Name)
		}
	}
	for _, name := range sel.Spec.FallbackChain {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// referencedProviders are every ModelProvider name the spec points at.
func referencedProviders(sel *agentorcav1alpha1.ModelSelector) []string {
	names := providerNames(sel)
	add := func(name string) {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if sel.Spec.MetaRouter != nil {
		add(sel.Spec.MetaRouter.ProviderRef)
	}
	for _, name := range sel.Spec.CapabilityRouting {
		add(name)
	}
	return names
}

// assessSelector decides the conditions of sel given which of its providers
// exist (found maps a name to its ModelProvider, or has no entry when it does not
// exist) and the outcome of validating its algorithm. It touches nothing.
func assessSelector(sel *agentorcav1alpha1.ModelSelector, found map[string]*agentorcav1alpha1.ModelProvider, algo algorithmOutcome) selectorAssessment {
	var out selectorAssessment
	resolved := assessProviders(sel, found)
	out.conditions = append(out.conditions, resolved)
	ready := resolved.Status == metav1.ConditionTrue

	if sel.Spec.Strategy == strategySwitchyard {
		out.switchyard = true
		sy := assessSwitchyard(sel, algo)
		out.conditions = append(out.conditions, sy)
		ready = ready && sy.Status == metav1.ConditionTrue
	}

	readyCond := metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: reasonChecksPassed, Message: "the selector is usable"}
	if !ready {
		readyCond.Status, readyCond.Reason = metav1.ConditionFalse, reasonChecksFailed
		readyCond.Message = "see ProvidersResolved and SwitchyardValid"
		if out.switchyard {
			readyCond.Message = "see the ProvidersResolved and SwitchyardValid conditions"
		}
	}
	out.conditions = append(out.conditions, readyCond)

	for _, name := range providerNames(sel) {
		if mp := found[name]; mp != nil && mp.Status.Ready {
			out.active = append(out.active, name)
		}
	}
	sort.Strings(out.active)
	return out
}

// assessProviders checks that every provider the spec names exists and that the
// names it routes to are providers of this selector.
func assessProviders(sel *agentorcav1alpha1.ModelSelector, found map[string]*agentorcav1alpha1.ModelProvider) metav1.Condition {
	var missing, skipped, bad []string
	for _, pw := range sel.Spec.Providers {
		if found[pw.Name] == nil {
			missing = append(missing, pw.Name)
		}
	}
	listed := map[string]bool{}
	for _, pw := range sel.Spec.Providers {
		listed[pw.Name] = true
	}
	for _, name := range sel.Spec.FallbackChain {
		if !listed[name] && found[name] == nil && !slices.Contains(skipped, name) {
			skipped = append(skipped, name)
		}
	}
	known := map[string]bool{}
	for _, name := range providerNames(sel) {
		known[name] = true
	}
	keys := make([]string, 0, len(sel.Spec.CapabilityRouting))
	for k := range sel.Spec.CapabilityRouting {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if name := sel.Spec.CapabilityRouting[k]; !known[name] {
			bad = append(bad, fmt.Sprintf("capabilityRouting[%s] names %q", k, name))
		}
	}
	if sel.Spec.MetaRouter != nil && !known[sel.Spec.MetaRouter.ProviderRef] {
		bad = append(bad, fmt.Sprintf("metaRouter.providerRef names %q", sel.Spec.MetaRouter.ProviderRef))
	}

	c := metav1.Condition{Type: ConditionProvidersResolved}
	switch {
	case len(missing) > 0:
		c.Status, c.Reason = metav1.ConditionFalse, reasonProvidersMissing
		c.Message = fmt.Sprintf("ModelProvider not found in namespace %s: %s", sel.Namespace, strings.Join(missing, ", "))
		if len(bad) > 0 {
			c.Message += "; " + strings.Join(bad, "; ") + ", which is not a provider of this selector"
		}
	case len(bad) > 0:
		c.Status, c.Reason = metav1.ConditionFalse, reasonInvalidReference
		c.Message = strings.Join(bad, "; ") + ", which is not a provider of this selector"
	default:
		c.Status, c.Reason = metav1.ConditionTrue, reasonAllResolved
		c.Message = fmt.Sprintf("%d provider(s) resolved", len(sel.Spec.Providers))
		if len(skipped) > 0 {
			c.Message += fmt.Sprintf("; fallback provider(s) not deployed and skipped: %s", strings.Join(skipped, ", "))
		}
	}
	c.Message = clipMessage(c.Message)
	return c
}

// assessSwitchyard turns the algorithm's validation outcome and the roles it
// needs into the SwitchyardValid condition.
func assessSwitchyard(sel *agentorcav1alpha1.ModelSelector, algo algorithmOutcome) metav1.Condition {
	c := metav1.Condition{Type: ConditionSwitchyardValid, Status: metav1.ConditionFalse, Reason: algo.reason, Message: algo.message}
	if algo.reason != reasonAlgorithmValid {
		if c.Reason == "" {
			c.Reason, c.Message = reasonInvalidAlgorithm, "the algorithm was not validated"
		}
		return c
	}
	var needed []string
	for _, role := range syengine.RequiredGroups(sel.Spec.Switchyard.Raw) {
		if sel.Spec.CapabilityRouting[role] == "" {
			needed = append(needed, role)
		}
	}
	if len(needed) > 0 {
		c.Reason = reasonMissingRole
		c.Message = fmt.Sprintf("this algorithm needs capabilityRouting roles: %s (name the provider that plays each)", strings.Join(needed, ", "))
		return c
	}
	c.Status, c.Reason = metav1.ConditionTrue, reasonAlgorithmValid
	c.Message = "the algorithm is served and accepted by Switchyard"
	return c
}

func clipMessage(msg string) string {
	if len(msg) > maxConditionMessage {
		return msg[:maxConditionMessage] + "..."
	}
	return msg
}

// selectorsForProvider maps a ModelProvider change to the selectors that use it.
func (r *ModelSelectorReconciler) selectorsForProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	var list agentorcav1alpha1.ModelSelectorList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if slices.Contains(referencedProviders(&list.Items[i]), obj.GetName()) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelSelectorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.ModelSelector{}).
		Watches(&agentorcav1alpha1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.selectorsForProvider)).
		Named("modelselector").
		Complete(r)
}

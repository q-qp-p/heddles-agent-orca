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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	syengine "github.com/heddles/agent-orca/internal/router/switchyard"
)

// testRuleBased is the default strategy, named once for the tests.
const testRuleBased = "rule-based"

const (
	stageRouterJSON = `{"type":"stage_router","picker":"efficient_first","capable_target":"strong",` +
		`"efficient_target":"weak","confidence_threshold":0.5}`
	compositeJSON = `{"type":"composite","classifier":{"target":"judge","base_threshold":0.5,"classify_trigger":"user_turn",` +
		`"fail_open":false},"stage":{"capable_target":"strong","efficient_target":"weak","confidence_threshold":0.5}}`
)

func selectorWith(strategy, switchyard string, mutate ...func(*agentorcav1alpha1.ModelSelector)) *agentorcav1alpha1.ModelSelector {
	sel := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "adaptive", Namespace: "ns", Generation: 1},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Strategy: strategy,
			Providers: []agentorcav1alpha1.ProviderWeight{
				{Name: "weak", Weight: 100}, {Name: "strong", Weight: 100}, {Name: "judge"},
			},
			CapabilityRouting: map[string]string{"capable": "strong", "efficient": "weak", "judge": "judge"},
		},
	}
	if switchyard != "" {
		sel.Spec.Switchyard = &runtime.RawExtension{Raw: []byte(switchyard)}
	}
	for _, m := range mutate {
		m(sel)
	}
	return sel
}

func provider(name string, ready bool) *agentorcav1alpha1.ModelProvider {
	return &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel: "openai/" + name, CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "k", Key: "api-key"},
		},
		Status: agentorcav1alpha1.ModelProviderStatus{Ready: ready},
	}
}

func allProviders(ready bool) map[string]*agentorcav1alpha1.ModelProvider {
	m := map[string]*agentorcav1alpha1.ModelProvider{}
	for _, n := range []string{"weak", "strong", "judge"} {
		m[n] = provider(n, ready)
	}
	return m
}

func condition(t *testing.T, a selectorAssessment, typ string) metav1.Condition {
	t.Helper()
	for _, c := range a.conditions {
		if c.Type == typ {
			return c
		}
	}
	t.Fatalf("no %s condition in %+v", typ, a.conditions)
	return metav1.Condition{}
}

func TestAssessSelector_RuleBasedNeedsOnlyItsProviders(t *testing.T) {
	a := assessSelector(selectorWith(testRuleBased, ""), allProviders(true), algorithmOutcome{})
	if c := condition(t, a, ConditionProvidersResolved); c.Status != metav1.ConditionTrue || c.Reason != reasonAllResolved {
		t.Errorf("ProvidersResolved = %+v", c)
	}
	if c := condition(t, a, ConditionReady); c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v", c)
	}
	if a.switchyard {
		t.Error("SwitchyardValid applies to a rule-based selector")
	}
	if !reflect.DeepEqual(a.active, []string{"judge", "strong", "weak"}) {
		t.Errorf("active = %v, want the ready providers sorted", a.active)
	}
}

func TestAssessSelector_ActiveProvidersAreTheReadyOnes(t *testing.T) {
	found := allProviders(true)
	found["weak"].Status.Ready = false
	delete(found, "judge")
	a := assessSelector(selectorWith(testRuleBased, ""), found, algorithmOutcome{})
	if !reflect.DeepEqual(a.active, []string{"strong"}) {
		t.Errorf("active = %v, want only the provider that exists and is ready", a.active)
	}
}

func TestAssessSelector_AMissingProviderIsNamed(t *testing.T) {
	found := allProviders(true)
	delete(found, "strong")
	a := assessSelector(selectorWith(testRuleBased, ""), found, algorithmOutcome{})
	c := condition(t, a, ConditionProvidersResolved)
	if c.Status != metav1.ConditionFalse || c.Reason != reasonProvidersMissing || !strings.Contains(c.Message, "strong") || !strings.Contains(c.Message, "ns") {
		t.Errorf("ProvidersResolved = %+v", c)
	}
	if r := condition(t, a, ConditionReady); r.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v, want False while a provider is missing", r)
	}
}

func TestAssessSelector_AFallbackThatIsNotDeployedIsNotAFailure(t *testing.T) {
	// Runs skip such a provider (a FallbackProviderSkipped event) instead of failing.
	sel := selectorWith(testRuleBased, "", func(s *agentorcav1alpha1.ModelSelector) { s.Spec.FallbackChain = []string{"strong", "backup"} })
	a := assessSelector(sel, allProviders(true), algorithmOutcome{})
	c := condition(t, a, ConditionProvidersResolved)
	if c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "backup") || !strings.Contains(c.Message, "skipped") {
		t.Errorf("ProvidersResolved = %+v, want True with the skipped fallback noted", c)
	}
}

func TestAssessSelector_RoutingMustNameProvidersOfTheSelector(t *testing.T) {
	sel := selectorWith("hybrid", "", func(s *agentorcav1alpha1.ModelSelector) {
		s.Spec.CapabilityRouting = map[string]string{"code": "gpt-9", "reasoning": "strong"}
		s.Spec.MetaRouter = &agentorcav1alpha1.MetaRouterConfig{ProviderRef: "tiny"}
	})
	c := condition(t, assessSelector(sel, allProviders(true), algorithmOutcome{}), ConditionProvidersResolved)
	if c.Status != metav1.ConditionFalse || c.Reason != reasonInvalidReference {
		t.Fatalf("ProvidersResolved = %+v", c)
	}
	for _, want := range []string{`capabilityRouting[code] names "gpt-9"`, `metaRouter.providerRef names "tiny"`} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q lacks %q", c.Message, want)
		}
	}
	if strings.Contains(c.Message, "reasoning") {
		t.Errorf("message %q flags a valid reference", c.Message)
	}
}

func TestAssessSelector_Switchyard(t *testing.T) {
	valid := algorithmOutcome{reason: reasonAlgorithmValid}
	tests := []struct {
		name       string
		sel        *agentorcav1alpha1.ModelSelector
		algo       algorithmOutcome
		wantStatus metav1.ConditionStatus
		wantReason string
		wantInMsg  string
	}{
		{"valid stage router", selectorWith(strategySwitchyard, stageRouterJSON), valid, metav1.ConditionTrue, reasonAlgorithmValid, ""},
		{"valid composite with a judge", selectorWith(strategySwitchyard, compositeJSON), valid, metav1.ConditionTrue, reasonAlgorithmValid, ""},
		{"stage router without its roles", selectorWith(strategySwitchyard, stageRouterJSON, func(s *agentorcav1alpha1.ModelSelector) {
			s.Spec.CapabilityRouting = nil
		}), valid, metav1.ConditionFalse, reasonMissingRole, "capable, efficient"},
		{"composite without a judge", selectorWith(strategySwitchyard, compositeJSON, func(s *agentorcav1alpha1.ModelSelector) {
			delete(s.Spec.CapabilityRouting, "judge")
		}), valid, metav1.ConditionFalse, reasonMissingRole, "judge"},
		{"random needs no roles", selectorWith(strategySwitchyard, `{"type":"random","targets":["strong","weak"]}`, func(s *agentorcav1alpha1.ModelSelector) {
			s.Spec.CapabilityRouting = nil
		}), valid, metav1.ConditionTrue, reasonAlgorithmValid, ""},
		{"unsupported algorithm", selectorWith(strategySwitchyard, `{"type":"advisor"}`),
			algorithmOutcome{reason: reasonUnsupportedAlgorithm, message: "advisor answers on the model's behalf"},
			metav1.ConditionFalse, reasonUnsupportedAlgorithm, "advisor"},
		{"invalid algorithm", selectorWith(strategySwitchyard, stageRouterJSON),
			algorithmOutcome{reason: reasonInvalidAlgorithm, message: `capable_target names "strnog"`},
			metav1.ConditionFalse, reasonInvalidAlgorithm, "strnog"},
		{"no algorithm", selectorWith(strategySwitchyard, ""),
			algorithmOutcome{reason: reasonMissingAlgorithm, message: "spec.switchyard is required"},
			metav1.ConditionFalse, reasonMissingAlgorithm, "required"},
		{"not validated", selectorWith(strategySwitchyard, stageRouterJSON), algorithmOutcome{}, metav1.ConditionFalse, reasonInvalidAlgorithm, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := assessSelector(tt.sel, allProviders(true), tt.algo)
			if !a.switchyard {
				t.Fatal("SwitchyardValid does not apply to a switchyard selector")
			}
			c := condition(t, a, ConditionSwitchyardValid)
			if c.Status != tt.wantStatus || c.Reason != tt.wantReason || !strings.Contains(c.Message, tt.wantInMsg) {
				t.Errorf("SwitchyardValid = %+v, want %s/%s containing %q", c, tt.wantStatus, tt.wantReason, tt.wantInMsg)
			}
			ready := condition(t, a, ConditionReady)
			if (ready.Status == metav1.ConditionTrue) != (tt.wantStatus == metav1.ConditionTrue) {
				t.Errorf("Ready = %+v, want it to follow SwitchyardValid", ready)
			}
		})
	}
}

func TestAlgorithmResult_BoundsLongErrors(t *testing.T) {
	r := &ModelSelectorReconciler{validate: func(json.RawMessage, []string) error { return errors.New(strings.Repeat("x", 5000)) }}
	got := r.algorithmResult(selectorWith(strategySwitchyard, stageRouterJSON))
	if got.reason != reasonInvalidAlgorithm || len(got.message) > maxConditionMessage+len("...") {
		t.Errorf("reason %q, message of %d bytes: a condition message must stay bounded", got.reason, len(got.message))
	}
}

func TestAlgorithmResult_OnlyForSwitchyard(t *testing.T) {
	called := false
	r := &ModelSelectorReconciler{validate: func(json.RawMessage, []string) error { called = true; return nil }}
	if got := r.algorithmResult(selectorWith("hybrid", "")); got != (algorithmOutcome{}) || called {
		t.Errorf("a hybrid selector was validated as Switchyard: %+v (called %v)", got, called)
	}
}

// --- Reconcile, against a fake client ----------------------------------------

func fakeSelectorReconciler(t *testing.T, validate func(json.RawMessage, []string) error, objs ...client.Object) (*ModelSelectorReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&agentorcav1alpha1.ModelSelector{}, &agentorcav1alpha1.ModelProvider{}).Build()
	return &ModelSelectorReconciler{Client: c, Scheme: scheme, validate: validate}, c
}

func reconcileSelector(t *testing.T, r *ModelSelectorReconciler) *agentorcav1alpha1.ModelSelector {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "adaptive"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got agentorcav1alpha1.ModelSelector
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "adaptive"}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func objects(sel *agentorcav1alpha1.ModelSelector, ready bool) []client.Object {
	providers := allProviders(ready)
	out := make([]client.Object, 0, 1+len(providers))
	out = append(out, sel)
	for _, p := range providers {
		out = append(out, p)
	}
	return out
}

func TestReconcile_PublishesConditionsAndActiveProviders(t *testing.T) {
	var calls atomic.Int32
	r, _ := fakeSelectorReconciler(t, func(json.RawMessage, []string) error { calls.Add(1); return nil },
		objects(selectorWith(strategySwitchyard, stageRouterJSON), true)...)

	got := reconcileSelector(t, r)
	for _, typ := range []string{ConditionProvidersResolved, ConditionSwitchyardValid, ConditionReady} {
		if c := meta.FindStatusCondition(got.Status.Conditions, typ); c == nil || c.Status != metav1.ConditionTrue || c.ObservedGeneration != 1 {
			t.Errorf("%s = %+v", typ, c)
		}
	}
	if got.Status.ObservedGeneration != 1 || !reflect.DeepEqual(got.Status.ActiveProviders, []string{"judge", "strong", "weak"}) {
		t.Errorf("status = %+v", got.Status)
	}
	if calls.Load() != 1 {
		t.Errorf("validated %d times, want once", calls.Load())
	}
}

func TestReconcile_ValidatesOncePerGeneration(t *testing.T) {
	var calls atomic.Int32
	var verdict error // reconciles run one after another in this test
	r, c := fakeSelectorReconciler(t, func(json.RawMessage, []string) error { calls.Add(1); return verdict },
		objects(selectorWith(strategySwitchyard, stageRouterJSON), true)...)

	reconcileSelector(t, r)
	reconcileSelector(t, r) // e.g. a provider's status changed
	reconcileSelector(t, r)
	if calls.Load() != 1 {
		t.Fatalf("validated %d times for one generation, want 1: compiling the guest is not free", calls.Load())
	}

	// A spec change bumps the generation: validate again.
	var sel agentorcav1alpha1.ModelSelector
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "adaptive"}, &sel); err != nil {
		t.Fatal(err)
	}
	sel.Generation = 2
	sel.Spec.Switchyard = &runtime.RawExtension{Raw: []byte(`{"type":"advisor"}`)}
	if err := c.Update(context.Background(), &sel); err != nil {
		t.Fatal(err)
	}
	verdict = fmt.Errorf("%w: algorithm \"advisor\"", syengine.ErrUnsupported)
	got := reconcileSelector(t, r)
	if calls.Load() != 2 {
		t.Errorf("validated %d times after a spec change, want 2", calls.Load())
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionSwitchyardValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonUnsupportedAlgorithm || cond.ObservedGeneration != 2 {
		t.Errorf("SwitchyardValid = %+v, want the unsupported verdict for generation 2", cond)
	}
	if got.Status.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d", got.Status.ObservedGeneration)
	}

	// The cached verdict is reused too, not just the success.
	reconcileSelector(t, r)
	if calls.Load() != 2 {
		t.Errorf("a cached failure was validated again (%d calls)", calls.Load())
	}
}

func TestReconcile_AnInvalidAlgorithmIsReportedNotFatal(t *testing.T) {
	r, _ := fakeSelectorReconciler(t, func(json.RawMessage, []string) error { return errors.New(`algorithm.capable_target names "x"`) },
		objects(selectorWith(strategySwitchyard, stageRouterJSON), true)...)
	got := reconcileSelector(t, r) // must not return an error: runs fail open instead
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionSwitchyardValid)
	if cond == nil || cond.Reason != reasonInvalidAlgorithm || !strings.Contains(cond.Message, `names "x"`) {
		t.Errorf("SwitchyardValid = %+v", cond)
	}
	if ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady); ready == nil || ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v", ready)
	}
}

func TestReconcile_ProvidersGoingReadyUpdatesActiveProviders(t *testing.T) {
	r, c := fakeSelectorReconciler(t, nil, objects(selectorWith(testRuleBased, ""), false)...)
	if got := reconcileSelector(t, r); len(got.Status.ActiveProviders) != 0 {
		t.Fatalf("active = %v with no ready provider", got.Status.ActiveProviders)
	}
	var mp agentorcav1alpha1.ModelProvider
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "strong"}, &mp); err != nil {
		t.Fatal(err)
	}
	mp.Status.Ready = true
	if err := c.Status().Update(context.Background(), &mp); err != nil {
		t.Fatal(err)
	}
	if got := reconcileSelector(t, r); !reflect.DeepEqual(got.Status.ActiveProviders, []string{"strong"}) {
		t.Errorf("active = %v, want [strong]", got.Status.ActiveProviders)
	}
}

func TestReconcile_LeavingSwitchyardRemovesItsCondition(t *testing.T) {
	r, c := fakeSelectorReconciler(t, func(json.RawMessage, []string) error { return nil }, objects(selectorWith(strategySwitchyard, stageRouterJSON), true)...)
	if got := reconcileSelector(t, r); meta.FindStatusCondition(got.Status.Conditions, ConditionSwitchyardValid) == nil {
		t.Fatal("setup: no SwitchyardValid condition")
	}
	var sel agentorcav1alpha1.ModelSelector
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "adaptive"}, &sel); err != nil {
		t.Fatal(err)
	}
	sel.Generation = 2
	sel.Spec.Strategy, sel.Spec.Switchyard = testRuleBased, nil
	if err := c.Update(context.Background(), &sel); err != nil {
		t.Fatal(err)
	}
	if got := reconcileSelector(t, r); meta.FindStatusCondition(got.Status.Conditions, ConditionSwitchyardValid) != nil {
		t.Error("a rule-based selector still reports SwitchyardValid")
	}
}

func TestReconcile_MissingSelectorIsNotAnError(t *testing.T) {
	r, _ := fakeSelectorReconciler(t, nil)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "gone"}}); err != nil {
		t.Errorf("Reconcile of a deleted selector = %v", err)
	}
}

func TestSelectorsForProvider(t *testing.T) {
	other := selectorWith(testRuleBased, "", func(s *agentorcav1alpha1.ModelSelector) {
		s.Name = "other"
		s.Spec.Providers = []agentorcav1alpha1.ProviderWeight{{Name: "unrelated"}}
		s.Spec.CapabilityRouting = nil
	})
	viaFallback := selectorWith(testRuleBased, "", func(s *agentorcav1alpha1.ModelSelector) {
		s.Name = "via-fallback"
		s.Spec.Providers = []agentorcav1alpha1.ProviderWeight{{Name: "unrelated"}}
		s.Spec.FallbackChain = []string{"weak"}
		s.Spec.CapabilityRouting = nil
	})
	elsewhere := selectorWith(testRuleBased, "", func(s *agentorcav1alpha1.ModelSelector) { s.Name = "elsewhere"; s.Namespace = "other-ns" })
	r, _ := fakeSelectorReconciler(t, nil, selectorWith(testRuleBased, ""), other, viaFallback, elsewhere)

	reqs := r.selectorsForProvider(context.Background(), provider("weak", true))
	names := make([]string, 0, len(reqs))
	for _, q := range reqs {
		names = append(names, q.Name)
	}
	if !reflect.DeepEqual(sortedStrings(names), []string{"adaptive", "via-fallback"}) {
		t.Errorf("selectors for provider weak = %v, want the ones in its namespace that reference it, fallback chain included", names)
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// --- the real guest, through envtest ---------------------------------------------

var _ = Describe("ModelSelector Controller", func() {
	ctx := context.Background()
	const ns = "default"

	selector := func(name, strategy, switchyard string) *agentorcav1alpha1.ModelSelector {
		sel := selectorWith(strategy, switchyard)
		sel.ObjectMeta = metav1.ObjectMeta{Name: name, Namespace: ns}
		return sel
	}
	cleanup := func(name string) {
		_ = k8sClient.Delete(ctx, &agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
	}

	It("requires spec.switchyard for strategy switchyard", func() {
		err := k8sClient.Create(ctx, selector("sy-no-spec", "switchyard", ""))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.switchyard is required for strategy switchyard"))
	})

	It("rejects spec.switchyard on any other strategy", func() {
		err := k8sClient.Create(ctx, selector("sy-wrong-strategy", testRuleBased, stageRouterJSON))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.switchyard is only used with strategy switchyard"))
	})

	It("accepts a switchyard selector and keeps the algorithm spec intact", func() {
		sel := selector("sy-ok", "switchyard", stageRouterJSON)
		Expect(k8sClient.Create(ctx, sel)).To(Succeed())
		DeferCleanup(cleanup, "sy-ok")

		var got agentorcav1alpha1.ModelSelector
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sy-ok"}, &got)).To(Succeed())
		Expect(got.Spec.Switchyard).NotTo(BeNil())
		var spec map[string]any
		Expect(json.Unmarshal(got.Spec.Switchyard.Raw, &spec)).To(Succeed())
		Expect(spec).To(HaveKeyWithValue("type", "stage_router"))
		Expect(spec).To(HaveKeyWithValue("confidence_threshold", BeNumerically("==", 0.5)),
			"the API server must not prune the algorithm's own fields")
	})

	It("still accepts the existing strategies unchanged", func() {
		Expect(k8sClient.Create(ctx, selector("sy-legacy", testRuleBased, ""))).To(Succeed())
		DeferCleanup(cleanup, "sy-legacy")
	})

	It("validates a real algorithm with Switchyard and reports unsupported ones", func() {
		for name, tc := range map[string]struct{ spec, reason string }{
			"sy-real-ok":  {stageRouterJSON, reasonAlgorithmValid},
			"sy-real-bad": {`{"type":"advisor"}`, reasonUnsupportedAlgorithm},
		} {
			sel := selector(name, "switchyard", tc.spec)
			Expect(k8sClient.Create(ctx, sel)).To(Succeed())
			DeferCleanup(cleanup, name)

			r := &ModelSelectorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
			Expect(err).NotTo(HaveOccurred())

			var got agentorcav1alpha1.ModelSelector
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, ConditionSwitchyardValid)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(tc.reason), "message: %s", cond.Message)
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		}
	})
})

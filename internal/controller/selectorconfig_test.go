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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
)

// testDefaultNS is the namespace the resolver tests use, as the existing
// fixtures do.
const testDefaultNS = "default"

func resolverClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func nsProvider(name string) *agentorcav1alpha1.ModelProvider {
	p := provider(name, true)
	p.Namespace = testDefaultNS
	return p
}

func nsSelector(mutate func(*agentorcav1alpha1.ModelSelector)) *agentorcav1alpha1.ModelSelector {
	sel := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "sel", Namespace: testDefaultNS},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Strategy:  testRuleBased,
			Providers: []agentorcav1alpha1.ProviderWeight{{Name: "a", Weight: 70, RoutingHint: "use for a"}, {Name: "b", Weight: 30}},
		},
	}
	if mutate != nil {
		mutate(sel)
	}
	return sel
}

func TestResolveSelectorRouting_ProvidersWeightsAndDefaults(t *testing.T) {
	c := resolverClient(t, nsProvider("a"), nsProvider("b"))
	got, err := resolveSelectorRouting(context.Background(), c, testDefaultNS, nsSelector(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 2 || got.Providers[0].Name != "a" || got.Providers[0].Weight != 70 || got.Providers[0].RoutingHint != "use for a" || got.Providers[1].Weight != 30 {
		t.Errorf("providers = %+v", got.Providers)
	}
	if !strings.HasSuffix(got.Providers[0].APIKeyFile, "/a/api-key") {
		t.Errorf("APIKeyFile = %q", got.Providers[0].APIKeyFile)
	}
	if got.Strategy != testRuleBased || got.MetaRouterThreshold != defaultMetaRouterThreshold || got.MetaRouterProviderName != "" ||
		got.BudgetPerRunUSD != 0 || got.Switchyard != nil || len(got.SkippedFallbacks) != 0 {
		t.Errorf("resolved = %+v", got)
	}
}

func TestResolveSelectorRouting_FallbacksAreAppendedWithZeroWeightAndMissingOnesSkipped(t *testing.T) {
	c := resolverClient(t, nsProvider("a"), nsProvider("b"), nsProvider("backup"))
	sel := nsSelector(func(s *agentorcav1alpha1.ModelSelector) {
		s.Spec.FallbackChain = []string{"b", "backup", "gone", "gone"}
	})
	got, err := resolveSelectorRouting(context.Background(), c, testDefaultNS, sel)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, p := range got.Providers {
		names[p.Name] = p.Weight
	}
	if !reflect.DeepEqual(names, map[string]int{"a": 70, "b": 30, "backup": 0}) {
		t.Errorf("providers = %v: a fallback-only provider is loaded with weight 0, one already listed keeps its weight", names)
	}
	if !reflect.DeepEqual(got.SkippedFallbacks, []string{"gone"}) {
		t.Errorf("skipped = %v, want [gone] once", got.SkippedFallbacks)
	}
	if !reflect.DeepEqual(got.FallbackChain, []string{"b", "backup", "gone", "gone"}) {
		t.Errorf("the chain is passed through as written: %v", got.FallbackChain)
	}
}

func TestResolveSelectorRouting_AMissingListedProviderFailsAndIsNamed(t *testing.T) {
	c := resolverClient(t, nsProvider("a"))
	_, err := resolveSelectorRouting(context.Background(), c, testDefaultNS, nsSelector(nil))
	if err == nil || !strings.Contains(err.Error(), `getting ModelProvider "b"`) {
		t.Errorf("err = %v, want the missing provider named", err)
	}
}

func TestResolveSelectorRouting_BudgetAndMetaRouter(t *testing.T) {
	c := resolverClient(t, nsProvider("a"), nsProvider("b"))
	sel := nsSelector(func(s *agentorcav1alpha1.ModelSelector) {
		s.Spec.Strategy = "hybrid"
		s.Spec.BudgetCap = &agentorcav1alpha1.BudgetCap{PerRun: "0.50"}
		s.Spec.MetaRouter = &agentorcav1alpha1.MetaRouterConfig{ProviderRef: "b", Threshold: "0.8"}
	})
	got, err := resolveSelectorRouting(context.Background(), c, testDefaultNS, sel)
	if err != nil {
		t.Fatal(err)
	}
	if got.BudgetPerRunUSD != 0.5 || got.MetaRouterProviderName != "b" || got.MetaRouterThreshold != 0.8 {
		t.Errorf("resolved = %+v", got)
	}
	sel.Spec.MetaRouter.Threshold = ""
	if got, _ := resolveSelectorRouting(context.Background(), c, testDefaultNS, sel); got.MetaRouterThreshold != defaultMetaRouterThreshold {
		t.Errorf("threshold = %v, want the default when unset", got.MetaRouterThreshold)
	}
}

func TestResolveSelectorRouting_SwitchyardOnlyForTheSwitchyardStrategy(t *testing.T) {
	c := resolverClient(t, nsProvider("a"), nsProvider("b"))
	spec := &runtime.RawExtension{Raw: []byte(stageRouterJSON)}

	sy := nsSelector(func(s *agentorcav1alpha1.ModelSelector) {
		s.Spec.Strategy, s.Spec.Switchyard = strategySwitchyard, spec
	})
	got, err := resolveSelectorRouting(context.Background(), c, testDefaultNS, sy)
	if err != nil || got.Switchyard == nil || string(got.Switchyard.Algorithm) != stageRouterJSON {
		t.Errorf("switchyard = %+v, %v; want the algorithm passed through byte for byte", got.Switchyard, err)
	}

	// (The CRD rejects this combination; a leftover field must still not switch it on.)
	other := nsSelector(func(s *agentorcav1alpha1.ModelSelector) { s.Spec.Switchyard = spec })
	if got, _ := resolveSelectorRouting(context.Background(), c, testDefaultNS, other); got.Switchyard != nil {
		t.Error("a rule-based selector with a stray spec.switchyard turned Switchyard on")
	}
}

// --- the router configs built from a switchyard selector --------------------

func switchyardFixture() (objs []client.Object, agent *agentorcav1alpha1.Agent) {
	sel := nsSelector(func(s *agentorcav1alpha1.ModelSelector) {
		s.Spec.Strategy = strategySwitchyard
		s.Spec.Switchyard = &runtime.RawExtension{Raw: []byte(stageRouterJSON)}
		s.Spec.CapabilityRouting = map[string]string{"capable": "a", "efficient": "b"}
	})
	agent = &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: testDefaultNS},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "sel"},
	}
	return []client.Object{nsProvider("a"), nsProvider("b"), sel, agent}, agent
}

func assertSwitchyardConfig(t *testing.T, cfg *router.Config) {
	t.Helper()
	if cfg.Strategy != strategySwitchyard || cfg.Switchyard == nil {
		t.Fatalf("strategy %q switchyard %+v", cfg.Strategy, cfg.Switchyard)
	}
	var spec, want map[string]any
	_ = json.Unmarshal(cfg.Switchyard.Algorithm, &spec)
	_ = json.Unmarshal([]byte(stageRouterJSON), &want)
	if !reflect.DeepEqual(spec, want) {
		t.Errorf("algorithm = %s", cfg.Switchyard.Algorithm)
	}
	if cfg.CapabilityRouting["capable"] != "a" || cfg.CapabilityRouting["efficient"] != "b" {
		t.Errorf("roles = %v: the router builds Switchyard's model groups from them", cfg.CapabilityRouting)
	}
}

func TestBuildRouterConfig_CarriesTheSwitchyardAlgorithm(t *testing.T) {
	objs, agent := switchyardFixture()
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: testDefaultNS},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "test-agent", Input: "ping"},
	}
	c := resolverClient(t, append(objs, run)...)
	r := &AgentRunReconciler{Client: c, Scheme: c.Scheme()}
	cfg, _, _, err := r.buildRouterConfig(context.Background(), run, agent, "sa", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSwitchyardConfig(t, cfg)
}

func TestBuildDeploymentRouterConfig_CarriesTheSwitchyardAlgorithm(t *testing.T) {
	objs, agent := switchyardFixture()
	deploy := &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: testDefaultNS},
		Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "test-agent"},
	}
	c := resolverClient(t, append(objs, deploy)...)
	r := &AgentDeploymentReconciler{Client: c, Scheme: c.Scheme()}
	cfg, err := r.buildDeploymentRouterConfig(context.Background(), deploy, agent, "sa", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSwitchyardConfig(t, cfg)
}

// Editing spec.switchyard must roll the deployment's pods: the sidecar reads its
// algorithm once at start, so the change reaches it only through the config hash.
func TestDeploymentConfigHash_ChangesWithTheSwitchyardAlgorithm(t *testing.T) {
	objs, agent := switchyardFixture()
	deploy := &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: testDefaultNS},
		Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "test-agent"},
	}
	c := resolverClient(t, append(objs, deploy)...)
	r := &AgentDeploymentReconciler{Client: c, Scheme: c.Scheme()}
	cfg, err := r.buildDeploymentRouterConfig(context.Background(), deploy, agent, "sa", nil)
	if err != nil {
		t.Fatal(err)
	}

	hash := func(cfg *router.Config) string {
		t.Helper()
		h, err := r.ensureDeploymentRouterConfigMap(context.Background(), deploy, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hash(cfg)
	if again := hash(cfg); again != base {
		t.Fatalf("the hash of an unchanged config moved: %s then %s", base, again)
	}

	changed := *cfg
	changed.Switchyard = &router.SwitchyardConfig{Algorithm: json.RawMessage(strings.Replace(stageRouterJSON, "0.5", "0.7", 1))}
	if hash(&changed) == base {
		t.Error("a different algorithm produced the same hash: edits to spec.switchyard would not roll the pods")
	}

	switched := *cfg
	switched.Strategy = testRuleBased
	switched.Switchyard = nil
	if hash(&switched) == base {
		t.Error("leaving the switchyard strategy produced the same hash")
	}
}

// The model-router's memory follows the selector's strategy at every place a pod
// with a router is built, not just in podbuilder.
func TestPodBuilders_SizeTheRouterForSwitchyard(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: testDefaultNS},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "test-agent", Input: "x"},
	}
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: testDefaultNS},
		Spec: agentorcav1alpha1.AgentSpec{
			ModelSelectorRef: "sel",
			Runtime:          agentorcav1alpha1.AgentRuntime{OCIRef: "busybox:1.36", Framework: "openai-compatible"},
		},
	}
	deploy := &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: testDefaultNS},
		Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "test-agent"},
	}
	rr := &AgentRunReconciler{ModelRouterImage: "router:v1"}
	dr := &AgentDeploymentReconciler{ModelRouterImage: "router:v1"}

	routerMem := func(pod *corev1.Pod) string {
		t.Helper()
		for _, list := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
			for _, c := range list {
				if c.Name == "model-router" {
					return c.Resources.Limits.Memory().String()
				}
			}
		}
		t.Fatalf("pod %s has no model-router", pod.Name)
		return ""
	}
	builders := map[string]func(strategy string) *corev1.Pod{
		"split-pod router": func(s string) *corev1.Pod {
			return rr.buildRouterPod(run, agent, "sa", s, nil, nil, nil, nil, nil, nil)
		},
		"full run pod": func(s string) *corev1.Pod {
			return rr.buildAgentPod(run, agent, "sa", "token", s, nil, nil, nil, nil, nil, nil)
		},
		"warm pod": func(s string) *corev1.Pod {
			return dr.buildWarmPod(deploy, agent, "sa", "token", "hash", s, nil, nil, nil, nil, nil, nil)
		},
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			if got := routerMem(build(strategySwitchyard)); got != "512Mi" {
				t.Errorf("switchyard: router memory limit = %s, want 512Mi", got)
			}
			if got := routerMem(build(testRuleBased)); got != "256Mi" {
				t.Errorf("rule-based: router memory limit = %s, want the 256Mi default", got)
			}
		})
	}
}

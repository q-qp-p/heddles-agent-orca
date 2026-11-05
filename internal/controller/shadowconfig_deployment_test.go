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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
)

// The benchmark follows the Agent: an AgentDeployment of a covered Agent is
// sampled just as its AgentRuns are, so a long-lived deployment is where the
// evidence mostly comes from.
func TestBuildDeploymentRouterConfig_SamplesTheDeploymentsAgent(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "assistant-agent", Namespace: testDefaultNS, Labels: map[string]string{"team": "a"}},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "sel"},
	}
	deploy := &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: testDefaultNS},
		Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: agent.Name},
	}
	bench := &agentorcav1alpha1.RoutingBenchmark{
		ObjectMeta: metav1.ObjectMeta{Name: "bench", Namespace: testDefaultNS},
		Spec: agentorcav1alpha1.RoutingBenchmarkSpec{
			AgentSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
			Shadow:        &agentorcav1alpha1.ShadowSpec{Enabled: true, MaxPerRun: 3},
			Judge:         &agentorcav1alpha1.JudgeSpec{ProviderRef: "judge"},
			Budget:        agentorcav1alpha1.BenchmarkBudget{PerDayUSD: "1"},
		},
	}
	c := resolverClient(t, agent, deploy, bench, nsSelector(nil), nsProvider("a"), nsProvider("b"), nsProvider("judge"))
	r := &AgentDeploymentReconciler{Client: c}

	cfg, err := r.buildDeploymentRouterConfig(context.Background(), deploy, agent, "sa", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shadow == nil || cfg.Shadow.Benchmark != "bench" {
		t.Fatalf("Shadow = %+v, want the benchmark that covers the deployment's Agent", cfg.Shadow)
	}
	if cfg.Shadow.MaxPerRun != 0 {
		t.Errorf("MaxPerRun = %d, want no local cap for a deployment", cfg.Shadow.MaxPerRun)
	}
	if len(cfg.AuxProviders) != 1 || cfg.AuxProviders[0].Name != "judge" {
		t.Errorf("AuxProviders = %+v, want the judge", cfg.AuxProviders)
	}

	// An Agent the benchmark does not cover is left alone.
	agent.Labels = map[string]string{"team": "b"}
	cfg, err = r.buildDeploymentRouterConfig(context.Background(), deploy, agent, "sa", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shadow != nil || len(cfg.AuxProviders) != 0 {
		t.Errorf("an uncovered Agent was sampled: %+v %+v", cfg.Shadow, cfg.AuxProviders)
	}
}

// The Agent is declared once: a warm pod of a deployment gets the sandbox_exec
// tool and its limits just as a dedicated AgentRun pod does.
func TestBuildDeploymentRouterConfig_GivesWarmPodsTheShellTool(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "assistant-agent", Namespace: testDefaultNS},
		Spec: agentorcav1alpha1.AgentSpec{
			ModelSelectorRef: "sel",
			Shell:            &agentorcav1alpha1.ShellAccessSpec{ClassRef: "coder", Image: "img", MaxTimeoutSeconds: 300},
		},
	}
	deploy := &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: testDefaultNS},
		Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: agent.Name},
	}
	c := resolverClient(t, agent, deploy, nsSelector(nil), nsProvider("a"), nsProvider("b"))
	r := &AgentDeploymentReconciler{Client: c}

	cfg, err := r.buildDeploymentRouterConfig(context.Background(), deploy, agent, "sa", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shell == nil || cfg.Shell.MaxTimeoutSeconds != 300 {
		t.Fatalf("Shell = %+v, want the Agent's limit", cfg.Shell)
	}
	if !hasToolDef(cfg.ToolDefinitions, "sandbox_exec") {
		t.Error("the warm pod's tool list has no sandbox_exec")
	}

	// An Agent without spec.shell gets neither.
	agent.Spec.Shell = nil
	cfg, err = r.buildDeploymentRouterConfig(context.Background(), deploy, agent, "sa", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shell != nil || hasToolDef(cfg.ToolDefinitions, "sandbox_exec") {
		t.Errorf("a shell was offered without spec.shell: %+v", cfg.Shell)
	}
}

func hasToolDef(defs []router.ToolDefinition, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

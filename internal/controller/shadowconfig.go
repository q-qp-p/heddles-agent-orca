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
	"log/slog"
	"slices"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/podbuilder"
	"github.com/heddles/agent-orca/internal/router"
)

// Defaults of a RoutingBenchmark's shadow settings, applied again here because
// the API server only defaults fields of an object that was present when it
// was created.
const (
	defaultShadowSampleRate = 0.05
	defaultShadowMaxPerRun  = 3
	defaultJudgeMaxTokens   = 512
)

// shadowRouting is the part of a router.Config that a RoutingBenchmark decides.
// The zero value means the run is not benchmarked.
type shadowRouting struct {
	Config *router.ShadowConfig
	// Aux are the judge and any shadow targets outside the selector. The router
	// can call them but never routes to them.
	Aux []router.ProviderConfig
}

// secretNames lists the providers whose credentials the pod must mount.
func (s shadowRouting) secretNames() []string {
	names := make([]string, 0, len(s.Aux))
	for _, p := range s.Aux {
		names = append(names, p.Name)
	}
	return names
}

// shadowSource says what the benchmarked work runs as. Evidence is filed under
// the Agent either way; the source only decides the local limits.
type shadowSource struct {
	// Deployment is true for an AgentDeployment, whose sidecar keeps running:
	// it has no per-run cap and the operator limits samples per hour instead.
	Deployment bool
	// RunLabels are the AgentRun labels that excludeRunSelector tests.
	RunLabels map[string]string
}

// resolveShadow finds the RoutingBenchmark that samples this Agent's calls,
// whether it runs as an AgentRun, an AgentDeployment or a workflow step. The
// first benchmark by name that covers the Agent wins. A benchmark whose judge or
// shadow providers do not exist is skipped; its Ready condition says why.
func resolveShadow(ctx context.Context, c client.Reader, agent *agentorcav1alpha1.Agent, src shadowSource, routing selectorRouting) (shadowRouting, error) {
	namespace := agent.Namespace
	var list agentorcav1alpha1.RoutingBenchmarkList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		// A cluster that predates the CRD must keep running its agents.
		slog.Warn("listing RoutingBenchmarks; running without shadow sampling", "err", err)
		return shadowRouting{}, nil
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for i := range list.Items {
		b := &list.Items[i]
		if !b.Covers(agent) || b.Spec.Shadow == nil || !b.Spec.Shadow.Enabled || b.Spec.Judge == nil {
			continue
		}
		if !src.Deployment && runExcluded(b.Spec.Shadow, src.RunLabels) {
			return shadowRouting{}, nil
		}
		out, ok, err := buildShadow(ctx, c, namespace, b, src, routing)
		if err != nil {
			return shadowRouting{}, err
		}
		if ok {
			return out, nil
		}
	}
	return shadowRouting{}, nil
}

// runExcluded reports whether the benchmark leaves this AgentRun out.
func runExcluded(s *agentorcav1alpha1.ShadowSpec, runLabels map[string]string) bool {
	if s.ExcludeRunSelector == nil {
		return false
	}
	sel, err := metav1.LabelSelectorAsSelector(s.ExcludeRunSelector)
	if err != nil {
		return false
	}
	return sel.Matches(labels.Set(runLabels))
}

// buildShadow turns one benchmark into router settings. ok is false when a
// provider it needs does not exist.
func buildShadow(ctx context.Context, c client.Reader, namespace string, b *agentorcav1alpha1.RoutingBenchmark, src shadowSource, routing selectorRouting) (shadowRouting, bool, error) {
	sp := b.Spec.Shadow
	cfg := &router.ShadowConfig{
		Benchmark:        b.Name,
		SampleRate:       defaultShadowSampleRate,
		Arms:             sp.Arms,
		Providers:        sp.Providers,
		MaxPerRun:        defaultShadowMaxPerRun,
		IncludeToolTurns: sp.IncludeToolTurns == nil || *sp.IncludeToolTurns,
		Judge:            b.Spec.Judge.ProviderRef,
		PositionSwap:     b.Spec.Judge.PositionSwap == nil || *b.Spec.Judge.PositionSwap,
		Rubric:           b.Spec.Judge.Rubric,
		JudgeMaxTokens:   defaultJudgeMaxTokens,
		StoreTranscripts: b.Spec.StoreTranscripts,
	}
	if cfg.Arms == "" {
		cfg.Arms = agentorcav1alpha1.ShadowArmsCounterfactual
	}
	if sp.SampleRate != "" {
		cfg.SampleRate = parseFloat(sp.SampleRate)
	}
	if sp.MaxPerRun > 0 {
		cfg.MaxPerRun = int(sp.MaxPerRun)
	}
	if src.Deployment {
		cfg.MaxPerRun = 0 // no local cap; the operator limits samples per hour
	}
	if b.Spec.Judge.MaxTokens > 0 {
		cfg.JudgeMaxTokens = int(b.Spec.Judge.MaxTokens)
	}

	routable := map[string]bool{}
	for _, p := range routing.Providers {
		routable[p.Name] = true
	}
	var aux []router.ProviderConfig
	for _, name := range append([]string{cfg.Judge}, cfg.Providers...) {
		if routable[name] || slices.ContainsFunc(aux, func(p router.ProviderConfig) bool { return p.Name == name }) {
			continue
		}
		var mp agentorcav1alpha1.ModelProvider
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &mp); err != nil {
			if apierrors.IsNotFound(err) {
				return shadowRouting{}, false, nil
			}
			return shadowRouting{}, false, fmt.Errorf("getting ModelProvider %q for benchmark %q: %w", name, b.Name, err)
		}
		apiKeyFile := fmt.Sprintf("%s/%s/api-key", podbuilder.ProviderSecretsDir, name)
		aux = append(aux, providerConfigFromSpec(name, &mp, 0, "", apiKeyFile))
	}
	return shadowRouting{Config: cfg, Aux: aux}, true, nil
}

// shadowFor resolves the benchmark for an Agent, fetching the selector it names.
// It is used before the router config is built, to mount the credentials the
// benchmark's providers need.
func shadowFor(ctx context.Context, c client.Reader, agent *agentorcav1alpha1.Agent, src shadowSource) (shadowRouting, error) {
	var sel agentorcav1alpha1.ModelSelector
	if err := c.Get(ctx, client.ObjectKey{Name: agent.Spec.ModelSelectorRef, Namespace: agent.Namespace}, &sel); err != nil {
		return shadowRouting{}, fmt.Errorf("getting ModelSelector: %w", err)
	}
	routing, err := resolveSelectorRouting(ctx, c, agent.Namespace, &sel)
	if err != nil {
		return shadowRouting{}, err
	}
	return resolveShadow(ctx, c, agent, src, routing)
}

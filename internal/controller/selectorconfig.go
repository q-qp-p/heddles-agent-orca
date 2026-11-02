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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/podbuilder"
	"github.com/heddles/agent-orca/internal/router"
)

// selectorRouting is the part of a router.Config that a ModelSelector decides:
// which providers exist and how a request is routed among them. AgentRuns and
// AgentDeployments both build it, so the two cannot drift apart.
type selectorRouting struct {
	// Providers are the selector's providers with their weights, followed by any
	// fallback-chain providers that are not among them (weight 0: eligible only as
	// a fallback).
	Providers []router.ProviderConfig
	// SkippedFallbacks are fallback-chain providers that are not deployed. They are
	// left out rather than blocking the run; the caller reports them.
	SkippedFallbacks []string

	Strategy               string
	CapabilityRouting      map[string]string
	FallbackChain          []string
	MetaRouterProviderName string
	MetaRouterThreshold    float64
	BudgetPerRunUSD        float64
	// Switchyard is the algorithm of strategy "switchyard"; nil for other strategies.
	Switchyard *router.SwitchyardConfig
}

// defaultMetaRouterThreshold is the rule-router confidence below which the
// meta-router is asked.
const defaultMetaRouterThreshold = 0.6

// resolveSelectorRouting loads the ModelProviders a ModelSelector names, in the
// selector's namespace, and derives its routing settings. A provider listed in
// spec.providers must exist; one only listed in the fallback chain may be absent.
func resolveSelectorRouting(ctx context.Context, c client.Reader, namespace string, sel *agentorcav1alpha1.ModelSelector) (selectorRouting, error) {
	out := selectorRouting{
		Strategy:          sel.Spec.Strategy,
		CapabilityRouting: sel.Spec.CapabilityRouting,
		FallbackChain:     sel.Spec.FallbackChain,
	}

	seen := make(map[string]bool)
	for _, pw := range sel.Spec.Providers {
		var mp agentorcav1alpha1.ModelProvider
		if err := c.Get(ctx, client.ObjectKey{Name: pw.Name, Namespace: namespace}, &mp); err != nil {
			return selectorRouting{}, fmt.Errorf("getting ModelProvider %q: %w", pw.Name, err)
		}
		seen[pw.Name] = true
		apiKeyFile := fmt.Sprintf("%s/%s/api-key", podbuilder.ProviderSecretsDir, pw.Name)
		out.Providers = append(out.Providers, providerConfigFromSpec(pw.Name, &mp, pw.Weight, pw.RoutingHint, apiKeyFile))
	}
	// Load fallback chain providers with Weight=0 so tryFallback can find them
	// but they are excluded from weighted primary selection.
	for _, name := range sel.Spec.FallbackChain {
		if seen[name] {
			continue
		}
		seen[name] = true
		var mp agentorcav1alpha1.ModelProvider
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &mp); err != nil {
			if apierrors.IsNotFound(err) {
				// Listed in the fallback chain but not deployed (e.g. disabled in the
				// model-providers chart): skip it rather than block the run.
				out.SkippedFallbacks = append(out.SkippedFallbacks, name)
				continue
			}
			return selectorRouting{}, fmt.Errorf("getting fallback ModelProvider %q: %w", name, err)
		}
		apiKeyFile := fmt.Sprintf("%s/%s/api-key", podbuilder.ProviderSecretsDir, name)
		out.Providers = append(out.Providers, providerConfigFromSpec(name, &mp, 0, "", apiKeyFile))
	}

	if sel.Spec.BudgetCap != nil {
		out.BudgetPerRunUSD = parseFloat(sel.Spec.BudgetCap.PerRun)
	}
	out.MetaRouterThreshold = defaultMetaRouterThreshold
	if sel.Spec.MetaRouter != nil {
		out.MetaRouterProviderName = sel.Spec.MetaRouter.ProviderRef
		if sel.Spec.MetaRouter.Threshold != "" {
			out.MetaRouterThreshold = parseFloat(sel.Spec.MetaRouter.Threshold)
		}
	}
	if sel.Spec.Strategy == strategySwitchyard && sel.Spec.Switchyard != nil && len(sel.Spec.Switchyard.Raw) > 0 {
		out.Switchyard = &router.SwitchyardConfig{Algorithm: sel.Spec.Switchyard.Raw}
	}
	return out, nil
}

// strategySwitchyard is the ModelSelector strategy backed by Switchyard.
const strategySwitchyard = "switchyard"

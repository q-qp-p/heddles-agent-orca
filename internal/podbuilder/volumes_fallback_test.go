/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package podbuilder

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestResolveProviderVolumes_FallbackChainSecretsAreMounted(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	primary := &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "primary-provider", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel: "openai/gpt-4o",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{
				Name: "primary-secret",
				Key:  "api-key",
			},
		},
	}
	fallback := &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "fallback-provider", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel: "anthropic/claude-haiku-4-5-20251001",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{
				Name: "fallback-secret",
				Key:  "api-key",
			},
		},
	}
	selector := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Providers: []agentorcav1alpha1.ProviderWeight{
				{Name: "primary-provider", Weight: 100},
			},
			FallbackChain: []string{"fallback-provider"},
		},
	}
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentSpec{
			ModelSelectorRef: "default",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(primary, fallback, selector, agent).
		Build()

	vols, mounts, err := ResolveProviderVolumes(context.Background(), cl, "default", agent)
	if err != nil {
		t.Fatalf("ResolveProviderVolumes: %v", err)
	}

	// Must have volumes and mounts for BOTH primary and fallback providers.
	wantSecrets := map[string]bool{"primary-secret": false, "fallback-secret": false}
	for _, v := range vols {
		if v.Secret != nil {
			wantSecrets[v.Secret.SecretName] = true
		}
	}
	for secret, found := range wantSecrets {
		if !found {
			t.Errorf("secret %q not mounted as a volume; fallback chain providers must have secrets mounted", secret)
		}
	}

	wantMounts := map[string]bool{
		ProviderSecretsDir + "/primary-provider":  false,
		ProviderSecretsDir + "/fallback-provider": false,
	}
	for _, m := range mounts {
		wantMounts[m.MountPath] = true
	}
	for path, found := range wantMounts {
		if !found {
			t.Errorf("mount path %q missing; fallback provider API key must be reachable in the pod", path)
		}
	}
}

func TestResolveProviderVolumes_FallbackProviderNotDeployed_Skipped(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	// Only the primary provider CR exists; the fallback CR is absent (disabled in chart).
	primary := &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "primary-provider", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel:   "openai/gpt-4o",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "primary-secret", Key: "api-key"},
		},
	}
	selector := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Providers:     []agentorcav1alpha1.ProviderWeight{{Name: "primary-provider", Weight: 100}},
			FallbackChain: []string{"disabled-provider"}, // no CR for this
		},
	}
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(primary, selector, agent). // no disabled-provider object
		Build()

	vols, _, err := ResolveProviderVolumes(context.Background(), cl, "default", agent)
	if err != nil {
		t.Fatalf("expected no error for missing fallback provider, got: %v", err)
	}
	// Only the primary provider's volume should be present.
	if len(vols) != 1 {
		t.Errorf("expected 1 volume (primary only), got %d", len(vols))
	}
}

func TestResolveProviderVolumes_PrimaryAlsoInFallback_NoDuplicateVolume(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	provider := &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-provider", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel: "openai/gpt-4o",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{
				Name: "shared-secret",
				Key:  "api-key",
			},
		},
	}
	selector := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Providers:     []agentorcav1alpha1.ProviderWeight{{Name: "shared-provider", Weight: 100}},
			FallbackChain: []string{"shared-provider"}, // same name in both lists
		},
	}
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider, selector, agent).
		Build()

	vols, mounts, err := ResolveProviderVolumes(context.Background(), cl, "default", agent)
	if err != nil {
		t.Fatalf("ResolveProviderVolumes: %v", err)
	}

	// Provider appearing in both primary and fallback must produce exactly one volume/mount.
	if len(vols) != 1 {
		t.Errorf("expected 1 volume, got %d: duplicate volume for provider in both primary and fallback lists", len(vols))
	}
	if len(mounts) != 1 {
		t.Errorf("expected 1 mount, got %d", len(mounts))
	}

	// Volume must be read-only.
	if !mounts[0].ReadOnly {
		t.Error("provider secret mount must be ReadOnly")
	}
	_ = corev1.Volume{} // keep import
}

func TestResolveProviderVolumes_ExtraProvidersAreMounted(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	provider := func(name string) *agentorcav1alpha1.ModelProvider {
		return &agentorcav1alpha1.ModelProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: agentorcav1alpha1.ModelProviderSpec{
				LiteLLMModel:   "openai/gpt-4o",
				CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "s-" + name, Key: "api-key"},
			},
		}
	}
	selector := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Providers: []agentorcav1alpha1.ProviderWeight{{Name: "main", Weight: 100}},
		},
	}
	agent := &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(selector, provider("main"), provider("judge")).Build()

	_, mounts, err := ResolveProviderVolumes(context.Background(), cl, "default", agent, "judge", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mounts = %+v, want main and judge once each", mounts)
	}
	if _, _, err := ResolveProviderVolumes(context.Background(), cl, "default", agent, "ghost"); err == nil {
		t.Error("a missing extra provider must fail: the router would be configured with a judge it cannot call")
	}
}

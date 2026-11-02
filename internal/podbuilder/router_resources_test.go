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

package podbuilder

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func memLimit(c *corev1.Container) string { return c.Resources.Limits.Memory().String() }

// A Switchyard engine adds ~108 MB of peak memory; the ordinary 256Mi sidecar
// would be left with little for the conversation, so a switchyard selector gets
// more. Every other strategy keeps the defaults.
func TestRouterResources_FollowTheStrategy(t *testing.T) {
	tests := []struct {
		strategy  string
		wantLimit string
	}{
		{"", "256Mi"},
		{"rule-based", "256Mi"},
		{"hybrid", "256Mi"},
		{"llm-meta", "256Mi"},
		{"switchyard", "512Mi"},
	}
	for _, tt := range tests {
		t.Run("strategy "+tt.strategy, func(t *testing.T) {
			cfg := baseConfig()
			cfg.RouterStrategy = tt.strategy
			if got := memLimit(routerContainerOf(t, Build(cfg))); got != tt.wantLimit {
				t.Errorf("Build: memory limit = %s, want %s", got, tt.wantLimit)
			}
			// The split-pod router is built by a different entry point.
			if got := memLimit(routerContainerOf(t, BuildRouterOnly(cfg))); got != tt.wantLimit {
				t.Errorf("BuildRouterOnly: memory limit = %s, want %s", got, tt.wantLimit)
			}
		})
	}
}

func TestRouterResources_SwitchyardKeepsTheCPUAndRaisesTheRequest(t *testing.T) {
	cfg := baseConfig()
	cfg.RouterStrategy = "switchyard"
	r := routerContainerOf(t, Build(cfg)).Resources
	if r.Limits.Cpu().String() != DefaultRouterResources.Limits.Cpu().String() {
		t.Errorf("cpu limit = %s, want the default %s", r.Limits.Cpu(), DefaultRouterResources.Limits.Cpu())
	}
	if r.Requests.Memory().Cmp(*DefaultRouterResources.Requests.Memory()) <= 0 {
		t.Errorf("memory request = %s, want more than the default %s so the scheduler reserves for the engine",
			r.Requests.Memory(), DefaultRouterResources.Requests.Memory())
	}
	// The limit must clear the measured peak (about 152 MB for one engine) with room to spare.
	if r.Limits.Memory().Cmp(resource.MustParse("400Mi")) < 0 {
		t.Errorf("memory limit = %s is too close to the measured engine peak", r.Limits.Memory())
	}
}

func TestRouterResources_AnExplicitOverrideWinsOverTheStrategy(t *testing.T) {
	cfg := baseConfig()
	cfg.RouterStrategy = "switchyard"
	cfg.RouterResources = &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	if got := memLimit(routerContainerOf(t, Build(cfg))); got != "1Gi" {
		t.Errorf("memory limit = %s, want the explicit override", got)
	}
}

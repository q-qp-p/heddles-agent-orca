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
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Covers reports whether the benchmark measures routing for this Agent: the
// Agent is in the benchmark's namespace and is named in agentRefs or matched by
// agentSelector. A nil selector matches nothing; an empty one matches every Agent.
func (b *RoutingBenchmark) Covers(agent *Agent) bool {
	if agent == nil || agent.Namespace != b.Namespace {
		return false
	}
	if slices.Contains(b.Spec.AgentRefs, agent.Name) {
		return true
	}
	if b.Spec.AgentSelector == nil {
		return false
	}
	sel, err := metav1.LabelSelectorAsSelector(b.Spec.AgentSelector)
	return err == nil && sel.Matches(labels.Set(agent.Labels))
}

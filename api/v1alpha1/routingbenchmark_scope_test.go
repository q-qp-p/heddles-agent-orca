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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRoutingBenchmarkCovers(t *testing.T) {
	agent := func(ns, name string, labels map[string]string) *Agent {
		return &Agent{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
	}
	bench := func(refs []string, sel *metav1.LabelSelector) *RoutingBenchmark {
		return &RoutingBenchmark{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns"},
			Spec:       RoutingBenchmarkSpec{AgentRefs: refs, AgentSelector: sel},
		}
	}
	team := &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}
	for name, tc := range map[string]struct {
		b    *RoutingBenchmark
		a    *Agent
		want bool
	}{
		"named":                    {bench([]string{"coder"}, nil), agent("ns", "coder", nil), true},
		"not named":                {bench([]string{"coder"}, nil), agent("ns", "writer", nil), false},
		"label match":              {bench(nil, team), agent("ns", "writer", map[string]string{"team": "a"}), true},
		"label mismatch":           {bench(nil, team), agent("ns", "writer", map[string]string{"team": "b"}), false},
		"named or labelled":        {bench([]string{"coder"}, team), agent("ns", "coder", map[string]string{"team": "b"}), true},
		"empty selector is all":    {bench(nil, &metav1.LabelSelector{}), agent("ns", "any", nil), true},
		"nothing named is nothing": {bench(nil, nil), agent("ns", "coder", nil), false},
		"other namespace":          {bench([]string{"coder"}, nil), agent("elsewhere", "coder", nil), false},
		"no agent":                 {bench(nil, &metav1.LabelSelector{}), nil, false},
		"invalid selector":         {bench(nil, &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "bogus"}}}), agent("ns", "coder", nil), false},
	} {
		if got := tc.b.Covers(tc.a); got != tc.want {
			t.Errorf("%s: Covers = %v, want %v", name, got, tc.want)
		}
	}
}

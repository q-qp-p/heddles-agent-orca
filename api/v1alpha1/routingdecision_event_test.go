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

import "testing"

func TestRoutingDecisionTraceEvent(t *testing.T) {
	ev := RoutingDecision{
		Model: "m", Provider: "p", Strategy: "switchyard", Reason: "Escalated to p: tool calls failed",
		Confidence: "0.90", DecisionID: "d-1", Path: "switchyard", Category: "tool-exec",
		Algorithm: "stage_router", Label: "override",
	}.TraceEvent()
	want := map[string]any{
		"type": "modelSelected", "model": "m", "provider": "p", "strategy": "switchyard",
		"reason": "Escalated to p: tool calls failed", "confidence": 0.9,
		"decisionId": "d-1", "path": "switchyard", "stage": "tool-exec", "algorithm": "stage_router", "label": "override",
	}
	for k, v := range want {
		if ev[k] != v {
			t.Errorf("%s = %v, want %v", k, ev[k], v)
		}
	}
	if _, ok := ev["parentId"]; ok {
		t.Error("an empty parent decision must be left out")
	}
}

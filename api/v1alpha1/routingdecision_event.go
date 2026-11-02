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

import "fmt"

// TraceEvent is the modelSelected event the UI and the archive show for this
// decision. The reason is the sentence the router wrote; the other fields back
// it so a reader can see which rule, label and stage produced it.
func (rd RoutingDecision) TraceEvent() map[string]any {
	conf := 0.0
	_, _ = fmt.Sscanf(rd.Confidence, "%f", &conf)
	ev := map[string]any{
		"type":       "modelSelected",
		"model":      rd.Model,
		"reason":     rd.Reason,
		"confidence": conf,
		"provider":   rd.Provider,
		"strategy":   rd.Strategy,
	}
	for k, v := range map[string]string{
		"decisionId": rd.DecisionID,
		"path":       rd.Path,
		"stage":      rd.Category,
		"algorithm":  rd.Algorithm,
		"label":      rd.Label,
		"parentId":   rd.ParentDecisionID,
	} {
		if v != "" {
			ev[k] = v
		}
	}
	return ev
}

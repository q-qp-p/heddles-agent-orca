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

package router

import (
	"fmt"
	"strings"
)

// The labels the Switchyard stage_router puts in its evidence.
const (
	labelFallOpen    = "fall_open"
	labelOverride    = "override"
	labelCapableHold = "capable_hold"
	labelDimensions  = "dimensions"
)

// algorithmStageRouter is the Switchyard stage_router algorithm.
const algorithmStageRouter = "stage_router"

// explainDecision turns a decision into one plain sentence for the logs, the
// trace and the UI: what was chosen and why, followed by the structured facts
// in brackets so a reader can filter on them. chosen is the provider that
// serves the call. failedFrom is the provider that failed, set only for a
// fallback decision.
func explainDecision(sel Selection, chosen, failedFrom string) string {
	var why string
	switch sel.Path {
	case pathFallback:
		return fmt.Sprintf("Fell back to %s: %s failed, so the next provider in the fallback chain took the call", chosen, failedFrom)
	case pathSwitchyard:
		why = explainSwitchyard(sel, chosen)
	default:
		why = explainRule(sel, chosen)
	}
	return why + decisionFacts(sel)
}

// explainSwitchyard describes a decision a Switchyard algorithm made.
func explainSwitchyard(sel Selection, chosen string) string {
	if sel.Algorithm == algorithmStageRouter {
		switch sel.Category.SYLabel {
		case labelFallOpen:
			return fmt.Sprintf("Chose %s, the efficient model: nothing so far suggests this call needs more", chosen)
		case labelOverride:
			return fmt.Sprintf("Escalated to %s, the capable model: tool calls have been failing repeatedly", chosen)
		case labelCapableHold:
			return fmt.Sprintf("Stayed on %s, the capable model: this run was escalated earlier and has not recovered", chosen)
		case labelDimensions:
			return fmt.Sprintf("Chose %s: stage_router scored the recent tool activity and it fits this model", chosen)
		}
	}
	if label := sel.Category.SYLabel; label != "" {
		return fmt.Sprintf("Chose %s: %s labelled this call %q", chosen, sel.Algorithm, label)
	}
	return fmt.Sprintf("Chose %s: picked by %s", chosen, sel.Algorithm)
}

// explainRule describes a decision the rule router or the meta-router made.
func explainRule(sel Selection, chosen string) string {
	switch {
	case sel.Path == pathNoProviders:
		return "No provider is configured, so there was nothing to choose"
	case sel.Path == pathVision:
		return fmt.Sprintf("Chose %s: the conversation contains images", chosen)
	case sel.Path == pathContext:
		return fmt.Sprintf("Chose %s: the conversation is large and needs a big context window", chosen)
	case sel.Path == pathBudget:
		return fmt.Sprintf("Chose %s: less than 20%% of the run budget is left", chosen)
	case sel.Path == pathTimeout:
		return fmt.Sprintf("Chose %s: the run is close to its time limit", chosen)
	case strings.HasPrefix(sel.Path, pathCapability):
		return fmt.Sprintf("Chose %s: the task matches the %q capability", chosen, strings.TrimPrefix(sel.Path, pathCapability))
	case sel.Path == pathWeighted:
		return fmt.Sprintf("Chose %s: no rule applied, so it was picked by weight among the providers", chosen)
	case sel.Path == pathMeta || sel.Path == pathMetaCache:
		return fmt.Sprintf("Chose %s: the rules were unsure, so the meta-router picked it", chosen)
	case sel.Path == pathSwitchyardFailOpen:
		return fmt.Sprintf("Chose %s with the rule router because Switchyard could not decide", chosen)
	}
	return fmt.Sprintf("Chose %s", chosen)
}

// decisionFacts renders the structured fields that back a sentence.
func decisionFacts(sel Selection) string {
	var facts []string
	if sel.Category.Stage != "" {
		facts = append(facts, "stage "+sel.Category.Stage)
	}
	if sel.Algorithm != "" {
		fact := sel.Algorithm
		if sel.Category.SYLabel != "" {
			fact += " " + sel.Category.SYLabel
		}
		facts = append(facts, fact)
	}
	if len(facts) == 0 {
		return ""
	}
	return " (" + strings.Join(facts, "; ") + ")"
}

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
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestExplainDecision(t *testing.T) {
	stageRouter := func(label, stage string) Selection {
		return Selection{Path: pathSwitchyard, Algorithm: algorithmStageRouter, Category: Category{SYLabel: label, Stage: stage}}
	}
	tests := []struct {
		name       string
		sel        Selection
		failedFrom string
		want       string
	}{
		{"stage_router fall_open", stageRouter(labelFallOpen, stageChat), "",
			"Chose weak, the efficient model: nothing so far suggests this call needs more (stage chat; stage_router fall_open)"},
		{"stage_router override", stageRouter(labelOverride, stageToolExec), "",
			"Escalated to weak, the capable model: tool calls have been failing repeatedly (stage tool-exec; stage_router override)"},
		{"stage_router capable_hold", stageRouter(labelCapableHold, stageToolExec), "",
			"Stayed on weak, the capable model: this run was escalated earlier and has not recovered (stage tool-exec; stage_router capable_hold)"},
		{"stage_router dimensions", stageRouter(labelDimensions, stageToolExec), "",
			"Chose weak: stage_router scored the recent tool activity and it fits this model (stage tool-exec; stage_router dimensions)"},
		{"another algorithm with a label", Selection{Path: pathSwitchyard, Algorithm: "llm_classifier", Category: Category{SYLabel: "hard"}}, "",
			`Chose weak: llm_classifier labelled this call "hard" (llm_classifier hard)`},
		{"another algorithm without a label", Selection{Path: pathSwitchyard, Algorithm: "random"}, "",
			"Chose weak: picked by random (random)"},
		{"vision rule", Selection{Path: pathVision, Category: Category{Stage: stageChat}}, "",
			"Chose weak: the conversation contains images (stage chat)"},
		{"context rule", Selection{Path: pathContext}, "",
			"Chose weak: the conversation is large and needs a big context window"},
		{"budget rule", Selection{Path: pathBudget}, "",
			"Chose weak: less than 20% of the run budget is left"},
		{"timeout rule", Selection{Path: pathTimeout}, "",
			"Chose weak: the run is close to its time limit"},
		{"capability rule", Selection{Path: pathCapability + "code"}, "",
			`Chose weak: the task matches the "code" capability`},
		{"weighted pick", Selection{Path: pathWeighted}, "",
			"Chose weak: no rule applied, so it was picked by weight among the providers"},
		{"meta-router", Selection{Path: pathMeta}, "",
			"Chose weak: the rules were unsure, so the meta-router picked it"},
		{"meta-router cache", Selection{Path: pathMetaCache}, "",
			"Chose weak: the rules were unsure, so the meta-router picked it"},
		{"switchyard fail-open", Selection{Path: pathSwitchyardFailOpen, Algorithm: algorithmStageRouter}, "",
			"Chose weak with the rule router because Switchyard could not decide (stage_router)"},
		{"fallback", Selection{Path: pathFallback, Algorithm: algorithmStageRouter, Category: Category{Stage: stageChat}}, "strong",
			"Fell back to weak: strong failed, so the next provider in the fallback chain took the call"},
		{"no providers", Selection{Path: pathNoProviders}, "",
			"No provider is configured, so there was nothing to choose"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := explainDecision(tt.sel, "weak", tt.failedFrom); got != tt.want {
				t.Errorf("explainDecision =\n %q\nwant\n %q", got, tt.want)
			}
		})
	}
}

// Every path a selector can produce must have a sentence of its own; a path
// that falls through to the generic "Chose X" tells the reader nothing.
func TestExplainDecision_NoPathFallsThroughToTheGenericSentence(t *testing.T) {
	for _, path := range []string{pathVision, pathContext, pathBudget, pathTimeout, pathCapability + "x", pathWeighted, pathMeta, pathMetaCache, pathSwitchyardFailOpen, pathNoProviders} {
		if got := explainDecision(Selection{Path: path}, "weak", ""); got == "Chose weak" {
			t.Errorf("path %q has no sentence", path)
		}
	}
}

// A reader of the router log must be able to tell why a model was chosen
// without opening the database: the sentence and its structured fields are on
// the one "routing request" line.
func TestRoutingRequestLogCarriesTheReason(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	llm, _ := fakeProvider(t, textTurnSSE(finishReasonStop))
	r, _ := budgetRouter(t, llm.URL, newFakeOperator(t, operatorBudget{}), BudgetConfig{})
	chatOnce(t, r)

	var line string
	for l := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(l, "routing request") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no routing request line in:\n%s", buf.String())
	}
	for _, want := range []string{"reason=", "Chose ", "path=rule:weighted", "algorithm=", "label=", "stage=chat"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %q:\n%s", want, line)
		}
	}
}

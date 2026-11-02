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

package routingbench

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// compositeSelector is a switchyard selector whose judge score is cut at 0.5.
func compositeSelector() Selector {
	return Selector{
		Name: selName, Strategy: "switchyard",
		Providers: []ProviderWeight{{Name: strong, Weight: 1}, {Name: weak, Weight: 1}},
		Switchyard: map[string]any{
			"type":       "composite",
			"classifier": map[string]any{"target": "judge", "base_threshold": 0.5},
		},
	}
}

// scoreCalls makes n calls of agent whose judge scores are spread evenly across
// (0, 1). The selector routed each at cut 0.5. The judge found the capable model
// better exactly when the score is below hardBelow.
func scoreCalls(agent string, n int, hardBelow float64) []Call {
	calls := make([]Call, 0, n)
	for i := range n {
		p := (float64(i) + 0.5) / float64(n)
		res := Equal
		if p < hardBelow {
			res = CapableBetter
		}
		calls = append(calls, Call{
			GroupKey: GroupKey{Agent: agent, Category: stageC, Capable: strong, Efficient: weak},
			Pair: Pair{
				Result: res, RoutedCapable: p < 0.5, Confidence: p,
				CapableCostUSD: 0.010, EfficientCostUSD: 0.001,
			},
			Algorithm: algorithmComposite,
		})
	}
	return calls
}

func thresholdInput(calls ...[]Call) Input {
	n := 0
	for _, cs := range calls {
		n += len(cs)
	}
	all := make([]Call, 0, n)
	agents := map[string]string{}
	for _, cs := range calls {
		all = append(all, cs...)
		agents[cs[0].Agent] = selName
	}
	return Input{Calls: all, Selectors: map[string]Selector{selName: compositeSelector()}, AgentSelector: agents}
}

func TestThresholdFindsACheaperCutThatKeepsQuality(t *testing.T) {
	// Only scores below 0.30 need the capable model, yet everything under 0.50
	// goes to it. A cut at 0.30 saves the money without losing an answer.
	fs := Detect(thresholdInput(scoreCalls("a", 200, 0.30)))
	if len(fs) != 1 || fs[0].Detector != DetectorThreshold {
		t.Fatalf("findings = %+v, want one threshold finding", fs)
	}
	f := fs[0]
	if f.Selector != selName || f.Agent != "" {
		t.Errorf("selector %q agent %q: a threshold belongs to the selector", f.Selector, f.Agent)
	}
	near(t, "from", f.Evidence.FromThreshold, 0.5, 1e-9)
	near(t, "to", f.Evidence.ToThreshold, 0.30, 1e-9)
	if f.Evidence.DeltaCostUSD >= 0 || f.Evidence.LossChange != 0 {
		t.Errorf("cost change %.4f, loss change %.4f: want cheaper and no worse", f.Evidence.DeltaCostUSD, f.Evidence.LossChange)
	}
	if f.Patch == nil {
		t.Fatalf("no patch (reason %q)", f.Reason)
	}
	if string(f.Patch.Forward) != `{"spec":{"switchyard":{"classifier":{"base_threshold":0.3}}}}` ||
		string(f.Patch.Rollback) != `{"spec":{"switchyard":{"classifier":{"base_threshold":0.5}}}}` {
		t.Errorf("forward %s rollback %s", f.Patch.Forward, f.Patch.Rollback)
	}
	// The frontier names every cut nothing beats, and marks the current one.
	var cur bool
	for _, p := range f.Evidence.Frontier {
		if p.Current && p.Threshold == 0.5 {
			cur = true
		}
	}
	if len(f.Evidence.Frontier) == 0 || !cur {
		t.Errorf("frontier = %+v, want points including the current cut", f.Evidence.Frontier)
	}
}

func TestThresholdBuysQualityOnlyWhenAWinIsWorthItsPrice(t *testing.T) {
	// Scores below 0.70 need the capable model but the cut is at 0.50: raising
	// it to 0.70 removes 20% losses for $0.0018 a call, $0.009 per avoided loss.
	in := thresholdInput(scoreCalls("a", 200, 0.70))
	if fs := Detect(in); len(fs) != 0 {
		t.Fatalf("with no limit on price per win the sweep may only save money, got %+v", fs)
	}
	limit := 0.05
	in.Options.MaxUSDPerNetWin = &limit
	fs := Detect(in)
	if len(fs) != 1 {
		t.Fatalf("findings = %+v, want one", fs)
	}
	near(t, "to", fs[0].Evidence.ToThreshold, 0.70, 1e-9)
	if fs[0].Evidence.LossChange >= 0 || !strings.Contains(fs[0].Summary, "fewer calls to the model whose answer lost") {
		t.Errorf("finding = %+v", fs[0])
	}
	dear := 0.005
	in.Options.MaxUSDPerNetWin = &dear
	if fs := Detect(in); len(fs) != 0 {
		t.Errorf("an avoided loss costs more than the limit, yet findings = %+v", fs)
	}
}

func TestThresholdStaysQuietWithoutEnoughTrustworthyEvidence(t *testing.T) {
	// 20 calls is under the bar.
	if fs := Detect(thresholdInput(scoreCalls("a", 20, 0.30))); len(fs) != 0 {
		t.Errorf("too few calls: %+v", fs)
	}
	// The routing did not follow the cut: every second call went the other way.
	calls := scoreCalls("a", 200, 0.30)
	for i := range calls {
		if i%2 == 0 {
			calls[i].RoutedCapable = !calls[i].RoutedCapable
		}
	}
	if fs := Detect(thresholdInput(calls)); len(fs) != 0 {
		t.Errorf("routing that ignores the threshold: %+v", fs)
	}
	// A different algorithm's score is not a judge score.
	other := scoreCalls("a", 200, 0.30)
	for i := range other {
		other[i].Algorithm = "stage_router"
	}
	if fs := Detect(thresholdInput(other)); len(fs) != 0 {
		t.Errorf("another algorithm's calls: %+v", fs)
	}
	// A selector that is not composite has no such threshold.
	in := thresholdInput(scoreCalls("a", 200, 0.30))
	in.Selectors[selName] = Selector{Name: selName, Strategy: "switchyard", Switchyard: map[string]any{"type": "stage_router"}}
	if fs := Detect(in); len(fs) != 0 {
		t.Errorf("stage_router: %+v", fs)
	}
}

func TestThresholdIsHeldBackWhenOneAgentWouldGetWorse(t *testing.T) {
	// Agent a has plenty of calls and would be fine at 0.30. Agent b has 40 calls
	// and every one between 0.30 and 0.50 needs the capable model, so it would
	// lose 8 of them. Pooled, b is too small to show; on its own it is clear.
	b := scoreCalls("b", 40, 0.50)
	fs := Detect(thresholdInput(scoreCalls("a", 1000, 0.30), b))
	if len(fs) != 1 || fs[0].Detector != DetectorThreshold {
		t.Fatalf("findings = %+v, want one threshold finding", fs)
	}
	if fs[0].Patch != nil || !strings.Contains(fs[0].Reason, "agent b") || fs[0].Action == nil {
		t.Errorf("patch %v reason %q: want the change held back because of b", fs[0].Patch, fs[0].Reason)
	}
}

func TestSetThresholdPatchRefusesWhatItCannotDoSafely(t *testing.T) {
	cases := map[string]struct {
		sel Selector
		v   float64
		why string
	}{
		"not switchyard": {Selector{Strategy: "rule-based"}, 0.3, "only a switchyard"},
		"not composite":  {Selector{Strategy: "switchyard", Switchyard: map[string]any{"type": "stage_router"}}, 0.3, "composite"},
		"out of range":   {compositeSelector(), 1.2, "outside (0, 1)"},
		"zero":           {compositeSelector(), 0, "outside (0, 1)"},
	}
	for name, tc := range cases {
		_, err := BuildPatch(tc.sel, Action{Kind: ActionSetThreshold, Value: tc.v})
		var nl *NoLeverError
		if !errors.As(err, &nl) || !strings.Contains(nl.Reason, tc.why) {
			t.Errorf("%s: err = %v, want a NoLeverError mentioning %q", name, err, tc.why)
		}
	}
}

// --- strategy switch -------------------------------------------------------

// routedCalls makes calls of one Agent on a rule-based selector. nc were routed
// to the capable model and it won in kc of them; ne to the efficient one and
// the capable model would have won in ke.
func routedCalls(nc, kc, ne, ke int) []Call {
	var out []Call
	mk := func(routedCapable bool, n, wins int) {
		for i := range n {
			res := Equal
			if i < wins {
				res = CapableBetter
			}
			out = append(out, Call{
				GroupKey: GroupKey{Agent: "a", Category: stageC, Capable: strong, Efficient: weak},
				Pair:     Pair{Result: res, RoutedCapable: routedCapable, CapableCostUSD: 0.01, EfficientCostUSD: 0.001},
			})
		}
	}
	mk(true, nc, kc)
	mk(false, ne, ke)
	return out
}

func ruleInput(calls []Call) Input {
	return Input{
		Calls: calls, AgentSelector: map[string]string{"a": selName},
		Selectors: map[string]Selector{selName: {
			Name: selName, Strategy: "rule-based",
			Providers:         []ProviderWeight{{Name: strong, Weight: 50}, {Name: weak, Weight: 50}},
			CapabilityRouting: map[string]string{"capable": strong},
		}},
	}
}

func TestStrategySwitchWhenTheRulesRouteNoBetterThanChance(t *testing.T) {
	// The capable model wins half the time whichever model the rules chose.
	fs := Detect(ruleInput(routedCalls(300, 150, 300, 150)))
	if len(fs) != 1 || fs[0].Detector != DetectorStrategySwitch {
		t.Fatalf("findings = %+v, want one strategySwitch", fs)
	}
	f := fs[0]
	if f.Caveat == "" || !strings.Contains(f.Caveat, "cannot be estimated") {
		t.Errorf("caveat = %q: the finding must say the effect cannot be predicted from these samples", f.Caveat)
	}
	want := `{"spec":{"capabilityRouting":{"capable":"strong","efficient":"weak"},"strategy":"switchyard",` +
		`"switchyard":{"capable_target":"strong","confidence_threshold":0.5,"efficient_target":"weak","picker":"efficient_first","type":"stage_router"}}}`
	if f.Patch == nil || string(f.Patch.Forward) != want {
		t.Fatalf("patch = %+v (reason %q)\nwant    %s", f.Patch, f.Reason, want)
	}
	// The rollback keeps the capable role the selector already had, drops the
	// efficient role it did not have, and removes the switchyard block.
	wantBack := `{"spec":{"capabilityRouting":{"capable":"strong","efficient":null},"strategy":"rule-based","switchyard":null}}`
	if string(f.Patch.Rollback) != wantBack {
		t.Errorf("rollback = %s\nwant       %s", f.Patch.Rollback, wantBack)
	}
	if f.Evidence.RoutingLift == nil || *f.Evidence.RoutingLift != 0 {
		t.Errorf("routing lift = %v, want 0", f.Evidence.RoutingLift)
	}
}

func TestStrategySwitchStaysQuietWhenRoutingWorks(t *testing.T) {
	cases := map[string][]Call{
		// Routing sends the hard calls up: 80% against 20%.
		"routing works": routedCalls(300, 240, 300, 60),
		// Too little data to rule out that routing works.
		"little data": routedCalls(30, 15, 30, 15),
		// Everything went one way; there is no routing to judge.
		"never routed to the cheaper model": routedCalls(300, 150, 0, 0),
	}
	for name, calls := range cases {
		if fs := Detect(ruleInput(calls)); len(fs) != 0 {
			t.Errorf("%s: findings = %+v", name, fs)
		}
	}
	in := ruleInput(routedCalls(300, 150, 300, 150))
	sel := in.Selectors[selName]
	sel.Strategy = "switchyard"
	in.Selectors[selName] = sel
	if fs := Detect(in); len(fs) != 0 {
		t.Errorf("a selector that already uses switchyard: %+v", fs)
	}
}

func TestSwitchStrategyPatchNeedsBothModelsInTheSelector(t *testing.T) {
	sel := Selector{Strategy: "rule-based", Providers: []ProviderWeight{{Name: strong}}}
	_, err := BuildPatch(sel, Action{Kind: ActionSwitchStrategy, Capable: strong, Efficient: weak})
	var nl *NoLeverError
	if !errors.As(err, &nl) || !strings.Contains(nl.Reason, "both models") {
		t.Errorf("err = %v", err)
	}
	if _, err := BuildPatch(sel, Action{Kind: ActionSwitchStrategy, Capable: strong, Efficient: strong}); err == nil {
		t.Error("a stage_router between a model and itself was accepted")
	}
}

func TestFindingsSerialiseForTheRecommendationEvidence(t *testing.T) {
	fs := Detect(thresholdInput(scoreCalls("a", 200, 0.30)))
	raw, err := json.Marshal(fs[0].Evidence)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"fromThreshold":0.5`, `"toThreshold":0.3`, `"frontier":[`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("evidence JSON lacks %s: %s", key, raw)
		}
	}
}

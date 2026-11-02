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
	"strings"
	"testing"
	"time"
)

func clusterOf(sel Selector, agents ...string) Cluster {
	c := Cluster{Selectors: map[string]Selector{sel.Name: sel}, AgentSelector: map[string]string{}}
	for _, a := range agents {
		c.AgentSelector[a] = sel.Name
	}
	return c
}

func ruleSelector() Selector {
	return Selector{
		Name: selName, Strategy: "rule-based",
		Providers:         []ProviderWeight{{Name: strong, Weight: 50}, {Name: weak, Weight: 50}},
		CapabilityRouting: map[string]string{"capable": strong, "efficient": weak},
	}
}

var window = Query{
	Namespace: "ns", Agents: []string{"a"},
	Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Now: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
}

// Analyze joins what was sampled to what was served, so a saving is scaled from
// the sample to the traffic of the whole window.
func TestAnalyzeScalesSavingsFromTheSampleToTheTraffic(t *testing.T) {
	ev := Observations{
		Samples: batch("a", stageC, 100, 1, 1, 0),
		Volumes: []Volume{
			{Agent: "a", Category: stageC, Provider: strong, Calls: 140},
			{Agent: "a", Category: stageC, Provider: strong, Calls: 160}, // two rows for one provider add up
			{Agent: "a", Category: stageC, Provider: weak, Calls: 900},   // not the capable model
			{Agent: "b", Category: stageC, Provider: strong, Calls: 5000},
		},
	}
	fs := Analyze(ev, clusterOf(ruleSelector(), "a"), window, Options{})
	if len(fs) != 1 || fs[0].Detector != DetectorOverspend {
		t.Fatalf("findings = %+v, want one overspend", fs)
	}
	if fs[0].Evidence.ProjectedMonthlySavingsUSD == nil {
		t.Fatal("no projected savings")
	}
	// 300 capable calls in 7 days at $0.009 each, over a 30-day month.
	near(t, "monthly savings", *fs[0].Evidence.ProjectedMonthlySavingsUSD, 0.009*300*30/7, 1e-9)
}

func TestAnalyzeOrientsByPriceWhenTheSelectorNamesNoRoles(t *testing.T) {
	sel := ruleSelector()
	sel.CapabilityRouting = nil
	c := clusterOf(sel, "a")
	c.ProviderCost = map[string]float64{strong: 0.00002, weak: 0.000002}
	ev := Observations{
		Samples: batch("a", stageC, 100, 1, 1, 0),
		Volumes: []Volume{{Agent: "a", Category: stageC, Provider: strong, Calls: 300}},
	}
	fs := Analyze(ev, c, window, Options{})
	if len(fs) != 1 || fs[0].Action.Provider != weak {
		t.Fatalf("findings = %+v, want strong recognised as the dearer model by price", fs)
	}
}

func TestAnalyzeFeedsEveryDetector(t *testing.T) {
	sel := compositeSelector()
	sel.CapabilityRouting = map[string]string{"capable": strong, "efficient": weak}
	calls := scoreCalls("a", 200, 0.30)
	samples := make([]Sample, 0, len(calls))
	for _, c := range calls {
		s := Sample{
			Agent: "a", Category: stageC, ServedProvider: weak, ShadowProvider: strong, Winner: WinnerTie,
			ServedCostUSD: c.EfficientCostUSD, ShadowCostUSD: c.CapableCostUSD, Algorithm: algorithmComposite, Confidence: c.Confidence,
		}
		if c.RoutedCapable {
			s.ServedProvider, s.ShadowProvider = strong, weak
			s.ServedCostUSD, s.ShadowCostUSD = c.CapableCostUSD, c.EfficientCostUSD
		}
		if c.Result == CapableBetter {
			s.Winner = WinnerServed
			if !c.RoutedCapable {
				s.Winner = WinnerShadow
			}
		}
		samples = append(samples, s)
	}
	ev := Observations{
		Samples:       samples,
		ProviderStats: []ProviderStat{{Selector: selName, Provider: weak, Calls: 50, Errors: 45}},
	}
	got := map[string]bool{}
	for _, f := range Analyze(ev, clusterOf(sel, "a"), window, Options{}) {
		got[f.Detector] = true
	}
	if !got[DetectorThreshold] || !got[DetectorDeadProvider] {
		t.Errorf("detectors that fired = %v, want threshold (from the sampled scores) and deadProvider (from passive stats)", got)
	}
}

func TestAnalyzeIsDeterministicAndSerialisable(t *testing.T) {
	ev := Observations{
		Samples: batch("a", stageC, 100, 1, 1, 0),
		Volumes: []Volume{{Agent: "a", Category: stageC, Provider: strong, Calls: 300}},
	}
	a, err := json.Marshal(Analyze(ev, clusterOf(ruleSelector(), "a"), window, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Analyze(ev, clusterOf(ruleSelector(), "a"), window, Options{}))
	if string(a) != string(b) {
		t.Error("the same observations gave different findings")
	}
	if !strings.Contains(string(a), `"Detector":"overspend"`) {
		t.Errorf("findings JSON = %s", a)
	}
}

func TestAnalyzeWithNothingFindsNothing(t *testing.T) {
	if fs := Analyze(Observations{}, Cluster{}, window, Options{}); len(fs) != 0 {
		t.Errorf("findings = %+v", fs)
	}
}

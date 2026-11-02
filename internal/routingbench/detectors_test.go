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
	"math"
	"slices"
	"strings"
	"testing"
)

const (
	selName = "sel"
	strong  = "strong"
	weak    = "weak"
	stageC  = "stage:chat"
	stageT  = "stage:tool-exec"
)

// tiers treats strong as the capable model and weak as the efficient one.
func tiers(string) Tiers { return Tiers{Capable: strong, Efficient: weak} }

// batch makes n samples of one Agent and category in which strong served and
// the judge gave each verdict: wins is the number of strong wins, losses the
// number of weak wins, the rest ties. A fraction inconsistent of the ties are
// marked as the judge disagreeing with itself.
func batch(agent, category string, n, wins, losses int, inconsistent float64) []Sample {
	out := make([]Sample, 0, n)
	for i := range n {
		s := Sample{
			Agent: agent, Category: category, ServedProvider: strong, ShadowProvider: weak,
			Winner: WinnerTie, ServedCostUSD: 0.010, ShadowCostUSD: 0.001,
		}
		switch {
		case i < wins:
			s.Winner = WinnerServed
		case i < wins+losses:
			s.Winner = WinnerShadow
		default:
			s.Inconsistent = float64(i-wins-losses) < inconsistent*float64(n)
		}
		out = append(out, s)
	}
	return out
}

func one(samples []Sample, mutate func(*Group)) Group {
	g := Aggregate(samples, tiers)[0]
	g.RoutedCapableCalls, g.WindowDays = 300, 7
	if mutate != nil {
		mutate(&g)
	}
	return g
}

// --- orientation -----------------------------------------------------------

func TestAggregateOrientsByRoleWhicheverModelServed(t *testing.T) {
	samples := []Sample{
		{Agent: "a", Category: stageC, ServedProvider: strong, ShadowProvider: weak, Winner: WinnerServed, ServedCostUSD: 0.01, ShadowCostUSD: 0.001},
		{Agent: "a", Category: stageC, ServedProvider: weak, ShadowProvider: strong, Winner: WinnerShadow, ServedCostUSD: 0.001, ShadowCostUSD: 0.01},
		{Agent: "a", Category: stageC, ServedProvider: weak, ShadowProvider: strong, Winner: WinnerServed, ServedCostUSD: 0.001, ShadowCostUSD: 0.01},
		{Agent: "a", Category: stageC, ServedProvider: strong, ShadowProvider: weak, Winner: WinnerTie, ServedCostUSD: 0.01, ShadowCostUSD: 0.001},
	}
	gs := Aggregate(samples, tiers)
	if len(gs) != 1 || gs[0].N() != 4 {
		t.Fatalf("groups = %+v, want one group of four pairs", gs)
	}
	want := []Result{CapableBetter, CapableBetter, EfficientBetter, Equal}
	for i, p := range gs[0].Pairs {
		if p.Result != want[i] {
			t.Errorf("pair %d result = %d, want %d", i, p.Result, want[i])
		}
		if p.CapableCostUSD != 0.01 || p.EfficientCostUSD != 0.001 {
			t.Errorf("pair %d costs = %v/%v: the capable model's cost must stay on the capable side whichever served", i, p.CapableCostUSD, p.EfficientCostUSD)
		}
	}
	if !gs[0].Pairs[0].RoutedCapable || gs[0].Pairs[1].RoutedCapable {
		t.Error("RoutedCapable must follow which model served")
	}
}

func TestAggregateFallsBackToPriceAndSkipsWhatItCannotOrient(t *testing.T) {
	priced := func(string) Tiers {
		return Tiers{CostPerToken: map[string]float64{"big": 5, "mid": 2, "same1": 1, "same2": 1}}
	}
	samples := []Sample{
		{Agent: "a", Category: stageC, ServedProvider: "mid", ShadowProvider: "big", Winner: WinnerShadow},
		{Agent: "a", Category: stageC, ServedProvider: "same1", ShadowProvider: "same2", Winner: WinnerTie}, // equal price
		{Agent: "a", Category: stageC, ServedProvider: "big", ShadowProvider: "big", Winner: WinnerTie},     // same model
		{Agent: "a", Category: stageC, ServedProvider: "big", ShadowProvider: "mid", Winner: "invalid"},     // unusable verdict
	}
	gs := Aggregate(samples, priced)
	if len(gs) != 1 || gs[0].N() != 1 || gs[0].Capable != "big" || gs[0].Efficient != "mid" || gs[0].Pairs[0].Result != CapableBetter {
		t.Errorf("groups = %+v, want one pair, big over mid, won by big", gs)
	}
}

func TestAggregateKeepsAgentsAndCategoriesApartInStableOrder(t *testing.T) {
	samples := slices.Concat(
		batch("writer", stageT, 2, 1, 0, 0),
		batch("coder", stageT, 2, 1, 0, 0),
		batch("coder", stageC, 2, 1, 0, 0),
	)
	gs := Aggregate(samples, tiers)
	got := make([]string, 0, len(gs))
	for _, g := range gs {
		got = append(got, g.Agent+"/"+g.Category)
	}
	if strings.Join(got, ",") != "coder/stage:chat,coder/stage:tool-exec,writer/stage:tool-exec" {
		t.Errorf("groups = %v", got)
	}
}

// --- worth-it numbers ------------------------------------------------------

func TestEvaluateKnownDataset(t *testing.T) {
	// 10 pairs: strong better in 6, weak better in 2, equal in 2. Strong costs
	// 0.010 against weak's 0.001, so each call costs 0.009 more.
	g := one(batch("a", stageC, 10, 6, 2, 0), nil)
	w := Evaluate(g, Options{})
	if w.N != 10 || w.CapableBetter != 6 || w.EfficientBetter != 2 || w.Equal != 2 {
		t.Fatalf("counts = %+v", w)
	}
	near(t, "net win", w.NetWin, 0.4, 1e-12)
	near(t, "delta cost", w.DeltaCostUSD, 0.009, 1e-12)
	if w.USDPerNetWin == nil {
		t.Fatal("USDPerNetWin is nil though the capable model wins more often")
	}
	near(t, "usd per net win", *w.USDPerNetWin, 0.009/0.4, 1e-12)
	if !w.NetWinCI.Contains(0.4) || w.NetWinCI.Lo >= w.NetWinCI.Hi {
		t.Errorf("net win interval %+v should surround 0.4", w.NetWinCI)
	}
	// Every call in the batch was served by the capable model.
	if w.EscalationPrecision == nil || *w.EscalationPrecision != 0.6 || w.MissRate != nil || w.RoutingLift != nil {
		t.Errorf("precision %v miss %v lift %v; with no calls routed to the cheaper model only precision exists",
			w.EscalationPrecision, w.MissRate, w.RoutingLift)
	}
	if again := Evaluate(g, Options{}); again.NetWinCI != w.NetWinCI || again.DeltaCostUSDCI != w.DeltaCostUSDCI {
		t.Error("the same input gave different intervals: the bootstrap must be seeded")
	}
}

func TestEvaluateUSDPerNetWinIsUndefinedWhenTheCapableModelDoesNotWinMore(t *testing.T) {
	w := Evaluate(one(batch("a", stageC, 10, 2, 4, 0), nil), Options{})
	if w.USDPerNetWin != nil || w.NetWin >= 0 {
		t.Errorf("net win %.2f, usd/win %v: want a negative net win and no cost per win", w.NetWin, w.USDPerNetWin)
	}
}

func TestEvaluateRoutingLiftComparesCallsRoutedEitherWay(t *testing.T) {
	// Calls routed to strong: strong better in 40 of 50. Routed to weak: strong
	// would have been better in 5 of 50. Routing sends the hard calls up.
	s := batch("a", stageC, 50, 40, 0, 0)
	for i := range 50 {
		w := WinnerTie
		if i < 5 {
			w = WinnerShadow // the judge preferred strong, which was the shadow
		}
		s = append(s, Sample{Agent: "a", Category: stageC, ServedProvider: weak, ShadowProvider: strong, Winner: w, ServedCostUSD: 0.001, ShadowCostUSD: 0.01})
	}
	w := Evaluate(Aggregate(s, tiers)[0], Options{})
	near(t, "precision", *w.EscalationPrecision, 0.8, 1e-12)
	near(t, "miss rate", *w.MissRate, 0.1, 1e-12)
	near(t, "lift", *w.RoutingLift, 0.7, 1e-12)
	if w.RoutingLiftCI.Lo <= 0 {
		t.Errorf("lift interval %+v should exclude 0: routing clearly beats chance", *w.RoutingLiftCI)
	}
}

func TestEvaluateEmptyGroup(t *testing.T) {
	w := Evaluate(Group{}, Options{})
	if w.N != 0 || !math.IsNaN(w.NetWinCI.Lo) || w.USDPerNetWin != nil {
		t.Errorf("empty group = %+v", w)
	}
}

// --- detectors -------------------------------------------------------------

func input(g ...Group) Input {
	return Input{
		Groups: g,
		Selectors: map[string]Selector{selName: {
			Name: selName, Strategy: "rule-based",
			Providers: []ProviderWeight{{Name: strong, Weight: 50}, {Name: weak, Weight: 50}},
		}},
		AgentSelector: map[string]string{"a": selName},
	}
}

// shared puts Agent b on the same selector as Agent a.
func shared(in Input) Input {
	in.AgentSelector["b"] = selName
	return in
}

func TestOverspendFiresWhenTheCheaperModelIsAsGood(t *testing.T) {
	g := one(batch("a", stageC, 100, 1, 1, 0), nil) // 98 of 100 equal, 1 each way
	fs := Detect(input(g))
	if len(fs) != 1 || fs[0].Detector != DetectorOverspend {
		t.Fatalf("findings = %+v, want one overspend", fs)
	}
	f := fs[0]
	if f.Action == nil || f.Action.Kind != ActionRouteCategory || f.Action.Provider != weak || f.Action.Category != stageC {
		t.Errorf("action = %+v, want to route the stage to the weak model", f.Action)
	}
	// 0.009 saved per call x 300 capable calls a week -> 30/7 weeks a month.
	if f.Evidence.ProjectedMonthlySavingsUSD == nil {
		t.Fatal("no projected savings")
	}
	near(t, "monthly savings", *f.Evidence.ProjectedMonthlySavingsUSD, 0.009*300*30/7, 1e-9)
	if f.Patch == nil || string(f.Patch.Forward) != `{"spec":{"capabilityRouting":{"stage:chat":"weak"}}}` {
		t.Errorf("patch = %+v (reason %q)", f.Patch, f.Reason)
	}
	if string(f.Patch.Rollback) != `{"spec":{"capabilityRouting":{"stage:chat":null}}}` {
		t.Errorf("rollback = %s, want the route deleted again", f.Patch.Rollback)
	}
	if f.Selector != selName {
		t.Errorf("selector = %q", f.Selector)
	}
}

func TestDetectorsStayQuietWhenTheEvidenceIsNotThereYet(t *testing.T) {
	cases := map[string]Group{
		// 29 pairs is below the 30-pair bar, however clean they look.
		"too few pairs": one(batch("a", stageC, 29, 0, 0, 0), nil),
		// Noisy small sample: 4 vs 3 wins out of 30 says nothing.
		"noise at small n": one(batch("a", stageC, 30, 4, 3, 0), nil),
		// The capable model is clearly better, so the cheaper one is not as good.
		"capable clearly better": one(batch("a", stageC, 60, 30, 0, 0), nil),
		// The judge contradicted itself on most pairs.
		"judge unreliable": one(batch("a", stageC, 60, 0, 0, 0.8), nil),
		// The capable model serves nothing here, so there is nothing to save.
		"no capable traffic": one(batch("a", stageC, 60, 0, 0, 0), func(g *Group) { g.RoutedCapableCalls = 0 }),
	}
	for name, g := range cases {
		if fs := Detect(input(g)); len(fs) != 0 {
			t.Errorf("%s: findings = %+v, want none", name, fs)
		}
	}
}

// mixed serves half the calls on each model and has strong win clearly more
// often than weak, at a cost per win the caller can compare to a limit.
func mixed(agent string, strongWins int) []Sample {
	s := batch(agent, stageT, 40, strongWins, 0, 0)
	for i := range 40 {
		w := WinnerTie
		if i < strongWins/2 {
			w = WinnerShadow
		}
		s = append(s, Sample{Agent: agent, Category: stageT, ServedProvider: weak, ShadowProvider: strong, Winner: w, ServedCostUSD: 0.001, ShadowCostUSD: 0.01})
	}
	return s
}

func TestUnderpoweredNeedsALimitAndFiresWithinIt(t *testing.T) {
	g := Aggregate(mixed("a", 30), tiers)[0]
	g.RoutedCapableCalls, g.WindowDays = 100, 7
	// net win = (30 + 15)/80 = 0.5625; the capable model costs 0.009 more per call,
	// so a win costs 0.009/0.5625 = $0.016.
	in := input(g)
	if fs := Detect(in); len(fs) != 0 {
		t.Fatalf("with no limit set the detector must stay inactive, got %+v", fs)
	}
	limit := 0.05
	in.Options.MaxUSDPerNetWin = &limit
	fs := Detect(in)
	if len(fs) != 1 || fs[0].Detector != DetectorUnderpowered {
		t.Fatalf("findings = %+v, want one underpowered", fs)
	}
	f := fs[0]
	if f.Action.Provider != strong || f.Action.Category != stageT {
		t.Errorf("action = %+v, want the stage routed to the strong model", f.Action)
	}
	near(t, "usd per win", *f.Evidence.USDPerNetWin, 0.009/0.5625, 1e-9)
	if string(f.Patch.Forward) != `{"spec":{"capabilityRouting":{"stage:tool-exec":"strong"}}}` {
		t.Errorf("patch = %s", f.Patch.Forward)
	}

	tight := 0.005
	in.Options.MaxUSDPerNetWin = &tight
	if fs := Detect(in); len(fs) != 0 {
		t.Errorf("a win costs more than the limit, yet findings = %+v", fs)
	}
}

func TestRatingsCorroborateButNeverTrigger(t *testing.T) {
	g := Aggregate(mixed("a", 30), tiers)[0]
	g.RoutedCapableCalls, g.WindowDays = 100, 7
	limit := 0.05
	in := input(g)
	in.Options.MaxUSDPerNetWin = &limit
	in.Ratings = map[AgentCategory]Ratings{{"a", stageT}: {Up: 1, Down: 6}}
	fs := Detect(in)
	if len(fs) != 1 || len(fs[0].Corroboration) != 1 || !strings.Contains(fs[0].Corroboration[0], "6 thumbs down") {
		t.Fatalf("findings = %+v, want one finding corroborated by the thumbs down", fs)
	}
	// Ratings alone, with no samples, find nothing.
	quiet := input()
	quiet.Ratings = in.Ratings
	if fs := Detect(quiet); len(fs) != 0 {
		t.Errorf("ratings alone produced %+v", fs)
	}
}

func TestDeadProviderNeedsOnlyPassiveData(t *testing.T) {
	in := input()
	in.Selectors[selName] = Selector{
		Name: selName, Strategy: "rule-based",
		Providers:     []ProviderWeight{{Name: strong, Weight: 50}, {Name: weak, Weight: 50, RoutingHint: "cheap"}},
		FallbackChain: []string{weak, "other"},
	}
	in.ProviderStats = []ProviderStat{
		{Selector: selName, Provider: weak, Calls: 40, Errors: 30},
		{Selector: selName, Provider: strong, Calls: 40, Errors: 2},
		{Selector: selName, Provider: "flaky", Calls: 5, Errors: 5}, // too few calls to judge
	}
	fs := Detect(in)
	if len(fs) != 1 || fs[0].Detector != DetectorDeadProvider || fs[0].Action.Provider != weak {
		t.Fatalf("findings = %+v, want weak reported dead", fs)
	}
	want := `{"spec":{"fallbackChain":["other"],"providers":[{"name":"strong","weight":50}]}}`
	if string(fs[0].Patch.Forward) != want {
		t.Errorf("patch = %s\nwant    %s", fs[0].Patch.Forward, want)
	}
	// The rollback restores both lists as they were, hints included.
	wantBack := `{"spec":{"fallbackChain":["weak","other"],"providers":[{"name":"strong","weight":50},{"name":"weak","weight":50,"routingHint":"cheap"}]}}`
	if string(fs[0].Patch.Rollback) != wantBack {
		t.Errorf("rollback = %s\nwant       %s", fs[0].Patch.Rollback, wantBack)
	}
}

func TestDeadProviderIsReportedButNotRemovedWhenItIsTheOnlyOne(t *testing.T) {
	in := input()
	in.Selectors[selName] = Selector{Name: selName, Strategy: "rule-based", Providers: []ProviderWeight{{Name: weak, Weight: 100}}}
	in.ProviderStats = []ProviderStat{{Selector: selName, Provider: weak, Calls: 40, Errors: 40}}
	fs := Detect(in)
	if len(fs) != 1 || fs[0].Patch != nil || !strings.Contains(fs[0].Reason, "nothing to route to") {
		t.Errorf("findings = %+v, want a report-only finding that says why", fs)
	}
}

// --- patches ---------------------------------------------------------------

func TestBuildPatchRouteCategoryRollbackRestoresThePreviousRoute(t *testing.T) {
	sel := Selector{Name: selName, Strategy: "rule-based", Providers: []ProviderWeight{{Name: strong}, {Name: weak}},
		CapabilityRouting: map[string]string{stageC: strong, "capable": strong}}
	p, err := BuildPatch(sel, Action{Kind: ActionRouteCategory, Category: stageC, Provider: weak})
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Forward) != `{"spec":{"capabilityRouting":{"stage:chat":"weak"}}}` || string(p.Rollback) != `{"spec":{"capabilityRouting":{"stage:chat":"strong"}}}` {
		t.Errorf("forward %s rollback %s", p.Forward, p.Rollback)
	}
	var fwd map[string]any
	if err := json.Unmarshal(p.Forward, &fwd); err != nil {
		t.Errorf("forward is not JSON: %v", err)
	}
}

func TestBuildPatchRefusesWhatItCannotDoSafely(t *testing.T) {
	base := Selector{Name: selName, Strategy: "rule-based", Providers: []ProviderWeight{{Name: strong, Weight: 1}, {Name: weak, Weight: 1}}}
	cases := map[string]struct {
		sel Selector
		a   Action
		why string
	}{
		"switchyard ignores stage routes": {
			sel: Selector{Name: selName, Strategy: "switchyard", Providers: base.Providers},
			a:   Action{Kind: ActionRouteCategory, Category: stageC, Provider: weak}, why: "switchyard",
		},
		"not a stage":      {base, Action{Kind: ActionRouteCategory, Category: "sy:override", Provider: weak}, "not a task stage"},
		"empty stage":      {base, Action{Kind: ActionRouteCategory, Category: "stage:", Provider: weak}, "not a task stage"},
		"unknown provider": {base, Action{Kind: ActionRouteCategory, Category: stageC, Provider: "ghost"}, "not one of the selector"},
		"remove unknown":   {base, Action{Kind: ActionRemoveProvider, Provider: "ghost"}, "not one of the selector"},
		"route still points at it": {
			Selector{Name: selName, Strategy: "rule-based", Providers: base.Providers, CapabilityRouting: map[string]string{stageC: weak}},
			Action{Kind: ActionRemoveProvider, Provider: weak}, "still sends traffic",
		},
	}
	for name, tc := range cases {
		_, err := BuildPatch(tc.sel, tc.a)
		var nl *NoLeverError
		if !errors.As(err, &nl) || !strings.Contains(nl.Reason, tc.why) {
			t.Errorf("%s: err = %v, want a NoLeverError mentioning %q", name, err, tc.why)
		}
	}
	if _, err := BuildPatch(base, Action{Kind: "Explode"}); err == nil {
		t.Error("an unknown action kind was accepted")
	}
}

// --- agents that share a selector -------------------------------------------

func overspendGroup(agent string) Group {
	return one(batch(agent, stageC, 100, 1, 1, 0), nil)
}

func TestSharedSelectorIsChangedOnlyWhenEveryAgentAgrees(t *testing.T) {
	t.Run("both agents agree", func(t *testing.T) {
		fs := Detect(shared(input(overspendGroup("a"), overspendGroup("b"))))
		if len(fs) != 2 || fs[0].Patch == nil || fs[1].Patch == nil {
			t.Fatalf("findings = %+v, want both patched", fs)
		}
	})
	t.Run("the other agent is not benchmarked enough", func(t *testing.T) {
		thin := one(batch("b", stageC, 10, 1, 1, 0), nil)
		fs := Detect(shared(input(overspendGroup("a"), thin)))
		if len(fs) != 1 || fs[0].Patch != nil || !strings.Contains(fs[0].Reason, "b, which has too few judged pairs") {
			t.Fatalf("findings = %+v, want a's finding held back for lack of b's evidence", fs)
		}
	})
	t.Run("the other agent's evidence disagrees", func(t *testing.T) {
		// For b the capable model is clearly better, so b has no overspend finding.
		b := one(batch("b", stageC, 60, 30, 0, 0), nil)
		fs := Detect(shared(input(overspendGroup("a"), b)))
		if len(fs) != 1 || fs[0].Patch != nil || !strings.Contains(fs[0].Reason, "separate selectors") {
			t.Fatalf("findings = %+v, want a's finding held back because b disagrees", fs)
		}
		if fs[0].Action == nil {
			t.Error("a held-back finding must still say what it would change")
		}
	})
	t.Run("an agent that does not share the selector is not consulted", func(t *testing.T) {
		in := input(overspendGroup("a"))
		in.AgentSelector["b"] = "other"
		if fs := Detect(in); len(fs) != 1 || fs[0].Patch == nil {
			t.Fatalf("findings = %+v, want a patched", fs)
		}
	})
}

func TestDetectIsDeterministic(t *testing.T) {
	in := shared(input(overspendGroup("b"), overspendGroup("a")))
	a, _ := json.Marshal(Detect(in))
	b, _ := json.Marshal(Detect(in))
	if string(a) != string(b) {
		t.Error("the same input gave different findings")
	}
}

func TestFindingForAnUnknownSelectorIsReportOnly(t *testing.T) {
	in := input(overspendGroup("a"))
	delete(in.AgentSelector, "a")
	fs := Detect(in)
	if len(fs) != 1 || fs[0].Patch != nil || !strings.Contains(fs[0].Reason, "not known") {
		t.Errorf("findings = %+v", fs)
	}
}

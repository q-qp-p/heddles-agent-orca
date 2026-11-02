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
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Detector names.
const (
	DetectorOverspend    = "overspend"
	DetectorUnderpowered = "underpowered"
	DetectorDeadProvider = "deadProvider"
)

// ProviderStat is how a provider fared in one selector over the window.
type ProviderStat struct {
	Selector string
	Provider string
	Calls    int
	Errors   int
}

// AgentCategory keys per-Agent, per-category data such as ratings.
type AgentCategory struct{ Agent, Category string }

// Ratings counts people's thumbs on calls of one Agent and category.
type Ratings struct{ Up, Down int }

// Input is everything the detectors read.
type Input struct {
	Groups []Group
	// Selectors are the selectors in play, by name; AgentSelector says which
	// one each Agent routes through.
	Selectors     map[string]Selector
	AgentSelector map[string]string
	// ProviderStats is passive data: it needs no shadow samples.
	ProviderStats []ProviderStat
	// Ratings corroborate a finding; on their own they trigger nothing.
	Ratings map[AgentCategory]Ratings
	Options Options
}

// Evidence is the numbers behind a finding.
type Evidence struct {
	Samples          int      `json:"samples"`
	NetWin           float64  `json:"netWin"`
	NetWinLo         float64  `json:"netWinLo"`
	NetWinHi         float64  `json:"netWinHi"`
	DeltaCostUSD     float64  `json:"deltaCostUsd"`
	USDPerNetWin     *float64 `json:"usdPerNetWin,omitempty"`
	InconsistentRate float64  `json:"inconsistentRate"`
	// ProjectedMonthlySavingsUSD is set by overspend findings when the traffic
	// is known.
	ProjectedMonthlySavingsUSD *float64 `json:"projectedMonthlySavingsUsd,omitempty"`
	// ErrorRate and Calls back a deadProvider finding.
	ErrorRate float64 `json:"errorRate,omitempty"`
	Calls     int     `json:"calls,omitempty"`
}

// Finding is something the evidence supports. When Patch is nil the finding is
// report-only: Reason says why no change is proposed.
type Finding struct {
	Detector string
	Agent    string
	Category string
	Selector string
	Summary  string
	Evidence Evidence
	Action   *Action
	Patch    *Patch
	Reason   string
	// Corroboration lists independent signals that agree with the finding.
	Corroboration []string
}

// Detect runs every detector over the input and returns the findings in a
// stable order. A finding that would change a selector other Agents also use is
// kept, but is only given a patch when all of them agree.
func Detect(in Input) []Finding {
	o := in.Options.withDefaults()
	var out []Finding
	for _, g := range in.Groups {
		if g.N() < o.MinSamples {
			continue
		}
		w := Evaluate(g, o)
		if f, ok := overspend(g, w, in, o); ok {
			out = append(out, f)
		}
		if f, ok := underpowered(g, w, in, o); ok {
			out = append(out, f)
		}
	}
	out = append(out, deadProviders(in, o)...)
	out = settle(out, in, o)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Detector != b.Detector {
			return a.Detector < b.Detector
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.Category < b.Category
	})
	return out
}

func evidenceOf(w Worth) Evidence {
	return Evidence{
		Samples: w.N, NetWin: w.NetWin, NetWinLo: w.NetWinCI.Lo, NetWinHi: w.NetWinCI.Hi,
		DeltaCostUSD: w.DeltaCostUSD, USDPerNetWin: w.USDPerNetWin, InconsistentRate: w.InconsistentRate,
	}
}

func (in Input) selectorOf(agent string) (Selector, bool) {
	name, ok := in.AgentSelector[agent]
	if !ok {
		return Selector{}, false
	}
	sel, ok := in.Selectors[name]
	if !ok {
		sel = Selector{Name: name}
	}
	return sel, true
}

// overspend: the cheaper model is as good in this category, yet the capable one
// still serves some of its calls at a higher price.
func overspend(g Group, w Worth, in Input, o Options) (Finding, bool) {
	if w.InconsistentRate > o.JudgeUnreliableRate || math.IsNaN(w.NetWinCI.Hi) {
		return Finding{}, false
	}
	// The cheaper model is non-inferior when even the best case for the capable
	// model is within the margin, and the capable one really is dearer.
	if w.NetWinCI.Hi >= o.NonInferiorityMargin || !(w.DeltaCostUSDCI.Lo > 0) || g.RoutedCapableCalls == 0 {
		return Finding{}, false
	}
	f := Finding{
		Detector: DetectorOverspend, Agent: g.Agent, Category: g.Category,
		Evidence: evidenceOf(w),
		Action:   &Action{Kind: ActionRouteCategory, Category: g.Category, Provider: g.Efficient},
	}
	if g.WindowDays > 0 {
		save := w.DeltaCostUSD * float64(g.RoutedCapableCalls) * 30 / g.WindowDays
		f.Evidence.ProjectedMonthlySavingsUSD = &save
	}
	f.Summary = fmt.Sprintf("%s serves %s calls of %s that %s answers as well: across %d judged pairs the capable model is ahead by %.0f%% at most (limit %.0f%%), for $%.4f more per call",
		g.Capable, g.Category, g.Agent, g.Efficient, w.N, w.NetWinCI.Hi*100, o.NonInferiorityMargin*100, w.DeltaCostUSD)
	if r, ok := in.Ratings[AgentCategory{g.Agent, g.Category}]; ok && r.Up > 0 && r.Up >= r.Down {
		f.Corroboration = append(f.Corroboration, fmt.Sprintf("%d thumbs up against %d down on this category", r.Up, r.Down))
	}
	return f, true
}

// underpowered: the capable model is clearly better in this category and the
// improvement is worth its price, yet the cheaper model serves some of its calls.
func underpowered(g Group, w Worth, in Input, o Options) (Finding, bool) {
	if o.MaxUSDPerNetWin == nil || w.InconsistentRate > o.JudgeUnreliableRate || math.IsNaN(w.NetWinCI.Lo) {
		return Finding{}, false
	}
	if w.NetWinCI.Lo <= o.MinNetWinDelta || w.NetWinCI.Lo <= 0 || w.USDPerNetWin == nil || *w.USDPerNetWin > *o.MaxUSDPerNetWin {
		return Finding{}, false
	}
	if w.MissRate == nil || *w.MissRate <= 0 {
		return Finding{}, false // the cheaper model never serves calls the capable one would win
	}
	f := Finding{
		Detector: DetectorUnderpowered, Agent: g.Agent, Category: g.Category,
		Evidence: evidenceOf(w),
		Action:   &Action{Kind: ActionRouteCategory, Category: g.Category, Provider: g.Capable},
	}
	f.Summary = fmt.Sprintf("%s is too weak for %s calls of %s: %s wins %.0f%% more often (at least %.0f%%), at $%.4f per extra win (limit $%.4f)",
		g.Efficient, g.Category, g.Agent, g.Capable, w.NetWin*100, w.NetWinCI.Lo*100, *w.USDPerNetWin, *o.MaxUSDPerNetWin)
	if r, ok := in.Ratings[AgentCategory{g.Agent, g.Category}]; ok && r.Down > r.Up {
		f.Corroboration = append(f.Corroboration, fmt.Sprintf("%d thumbs down against %d up on this category", r.Down, r.Up))
	}
	return f, true
}

// deadProviders reports providers failing so often a selector should stop using
// them. It needs no shadow samples: passive outcomes alone trigger it.
func deadProviders(in Input, o Options) []Finding {
	var out []Finding
	for _, st := range in.ProviderStats {
		if st.Calls < o.MinProviderCalls {
			continue
		}
		rate := float64(st.Errors) / float64(st.Calls)
		if rate < o.DeadProviderErrorRate {
			continue
		}
		out = append(out, Finding{
			Detector: DetectorDeadProvider, Selector: st.Selector,
			Summary: fmt.Sprintf("%s failed %d of %d calls (%.0f%%) through selector %s",
				st.Provider, st.Errors, st.Calls, rate*100, st.Selector),
			Evidence: Evidence{ErrorRate: rate, Calls: st.Calls},
			Action:   &Action{Kind: ActionRemoveProvider, Selector: st.Selector, Provider: st.Provider},
		})
	}
	return out
}

// settle gives each finding its selector, builds its patch, and holds back any
// change to a selector that other Agents also use unless all of them agree.
// Selectors are resolved for every finding first, so that the actions being
// compared across Agents are complete.
func settle(fs []Finding, in Input, o Options) []Finding {
	known := make([]bool, len(fs))
	for i := range fs {
		f := &fs[i]
		if f.Action == nil {
			continue
		}
		if f.Action.Kind == ActionRemoveProvider {
			known[i] = true
			continue
		}
		sel, ok := in.selectorOf(f.Agent)
		if !ok {
			f.noPatch("the Agent's ModelSelector is not known")
			continue
		}
		f.Selector, f.Action.Selector = sel.Name, sel.Name
		known[i] = true
	}
	for i := range fs {
		f := &fs[i]
		if f.Action == nil || !known[i] {
			continue
		}
		sel := in.Selectors[f.Action.Selector]
		sel.Name = f.Action.Selector
		patch, err := BuildPatch(sel, *f.Action)
		if err != nil {
			var nl *NoLeverError
			if errors.As(err, &nl) {
				f.noPatch(nl.Reason)
			} else {
				f.noPatch(err.Error())
			}
			continue
		}
		if f.Detector != DetectorDeadProvider {
			if why := disagreement(*f, fs, in, o); why != "" {
				f.noPatch(why)
				continue
			}
		}
		f.Patch = &patch
	}
	return fs
}

func (f *Finding) noPatch(reason string) { f.Reason, f.Patch = reason, nil }

// disagreement returns why a change to f's selector is not safe for every Agent
// that uses it, or "" when it is. Every Agent on the selector must have enough
// judged pairs in the category and a finding with the same action.
func disagreement(f Finding, all []Finding, in Input, o Options) string {
	var sharers []string
	for agent, sel := range in.AgentSelector {
		if sel == f.Selector && agent != f.Agent {
			sharers = append(sharers, agent)
		}
	}
	if len(sharers) == 0 {
		return ""
	}
	sort.Strings(sharers)
	var lacking, differing []string
	for _, agent := range sharers {
		samples := 0
		for _, g := range in.Groups {
			if g.Agent == agent && g.Category == f.Category {
				samples += g.N()
			}
		}
		if samples < o.MinSamples {
			lacking = append(lacking, agent)
			continue
		}
		agrees := false
		for _, other := range all {
			if other.Detector == f.Detector && other.Agent == agent && other.Category == f.Category &&
				other.Action != nil && *other.Action == *f.Action {
				agrees = true
			}
		}
		if !agrees {
			differing = append(differing, agent)
		}
	}
	switch {
	case len(lacking) > 0:
		return fmt.Sprintf("selector %s is also used by %s, which has too few judged pairs for %s; benchmark it before changing the selector",
			f.Selector, strings.Join(lacking, ", "), f.Category)
	case len(differing) > 0:
		return fmt.Sprintf("selector %s is also used by %s, whose evidence does not support the same change for %s; give the Agents separate selectors",
			f.Selector, strings.Join(differing, ", "), f.Category)
	}
	return ""
}

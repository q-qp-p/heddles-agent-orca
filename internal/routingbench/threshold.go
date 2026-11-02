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
	"fmt"
	"math"
	"slices"
	"sort"
)

// Detector names added with the sweep and the strategy switch.
const (
	DetectorThreshold      = "threshold"
	DetectorStrategySwitch = "strategySwitch"
)

// algorithmComposite is the Switchyard algorithm whose judge score is compared
// with classifier.base_threshold: a high score (the cheaper model can solve the
// task) goes to the efficient model, a low one to the capable model.
const algorithmComposite = "composite"

// thresholdAgreement is how often the logged routing must follow the current
// threshold before the sweep trusts its model of the selector. Switchyard
// calls the cut-off a base threshold, so it may be adjusted per call; when the
// logged decisions do not follow it, the sweep says nothing.
const thresholdAgreement = 0.95

// FrontierPoint is one threshold's cost and quality loss, measured on the
// logged calls.
type FrontierPoint struct {
	Threshold float64 `json:"threshold"`
	// CostUSD is the mean cost of the answer the threshold would have routed to.
	CostUSD float64 `json:"costUsd"`
	// Loss is the share of calls routed to the model whose answer lost.
	Loss    float64 `json:"loss"`
	Current bool    `json:"current,omitempty"`
}

// policyOutcome replays a call under a threshold: the efficient model serves
// it when the judge's score reaches the threshold. Cost is that answer's cost
// and loss is 1 when the other model's answer was better.
func policyOutcome(c Call, threshold float64) (cost, loss float64) {
	if c.Confidence >= threshold {
		if c.Result == CapableBetter {
			loss = 1
		}
		return c.EfficientCostUSD, loss
	}
	if c.Result == EfficientBetter {
		loss = 1
	}
	return c.CapableCostUSD, loss
}

// thresholdGrid is the candidate cut-offs, from 0.05 to 0.95 in steps of 0.05,
// plus the current one.
func thresholdGrid(current float64) []float64 {
	var g []float64
	for i := 1; i <= 19; i++ {
		g = append(g, float64(i)*0.05)
	}
	if !slices.ContainsFunc(g, func(v float64) bool { return math.Abs(v-current) < 1e-9 }) {
		g = append(g, current)
	}
	sort.Float64s(g)
	return g
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// thresholdFindings sweeps the cut-off of each composite selector over the
// calls the benchmark replayed, and proposes a different one when the evidence
// clears the bar: cheaper without a loss in quality beyond the margin, or
// better at a price inside maxUSDPerNetWin per avoided loss.
func thresholdFindings(in Input, o Options) []Finding {
	bySelector := map[string][]Call{}
	for _, c := range in.Calls {
		name, ok := in.AgentSelector[c.Agent]
		if ok && c.Algorithm == algorithmComposite && c.Confidence > 0 {
			bySelector[name] = append(bySelector[name], c)
		}
	}
	names := make([]string, 0, len(bySelector))
	for n := range bySelector {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []Finding
	for _, name := range names {
		sel := in.Selectors[name]
		current, ok := compositeBaseThreshold(sel.Switchyard)
		calls := bySelector[name]
		if !ok || sel.Strategy != strategySwitchyard || len(calls) < o.MinSamples {
			continue
		}
		agree := 0
		for _, c := range calls {
			if c.RoutedCapable == (c.Confidence < current) {
				agree++
			}
		}
		if float64(agree)/float64(len(calls)) < thresholdAgreement {
			continue
		}
		if f, ok := sweep(name, current, calls, o); ok {
			if why := agentVeto(calls, current, f.Action.Value, o); why != "" {
				f.Reason = why
			}
			out = append(out, f)
		}
	}
	return out
}

func sweep(selector string, current float64, calls []Call, o Options) (Finding, bool) {
	grid := thresholdGrid(current)
	n := len(calls)
	cost := make(map[float64][]float64, len(grid))
	loss := make(map[float64][]float64, len(grid))
	points := make([]FrontierPoint, 0, len(grid))
	for _, t := range grid {
		cs, ls := make([]float64, n), make([]float64, n)
		var cm, lm float64
		for i, c := range calls {
			cs[i], ls[i] = policyOutcome(c, t)
			cm += cs[i]
			lm += ls[i]
		}
		cost[t], loss[t] = cs, ls
		points = append(points, FrontierPoint{Threshold: round2(t), CostUSD: cm / float64(n), Loss: lm / float64(n), Current: math.Abs(t-current) < 1e-9})
	}
	frontier := paretoFrontier(points)

	meanDiff := func(a, b []float64) func(idx []int) (float64, bool) {
		return func(idx []int) (float64, bool) {
			s := 0.0
			for _, i := range idx {
				s += a[i] - b[i]
			}
			return s / float64(len(idx)), true
		}
	}
	type candidate struct {
		t              float64
		dCost, dLoss   float64
		costCI, lossCI Interval
	}
	var cheaper, better []candidate
	for _, t := range grid {
		if math.Abs(t-current) < 1e-9 {
			continue
		}
		c := candidate{t: t}
		c.dCost, _ = meanDiff(cost[t], cost[current])(allIdx(n))
		c.dLoss, _ = meanDiff(loss[t], loss[current])(allIdx(n))
		c.costCI = bootstrap(n, o.Resamples, o.Confidence, o.Seed, meanDiff(cost[t], cost[current]))
		c.lossCI = bootstrap(n, o.Resamples, o.Confidence, o.Seed, meanDiff(loss[t], loss[current]))
		if math.IsNaN(c.costCI.Hi) || math.IsNaN(c.lossCI.Hi) {
			continue
		}
		if c.costCI.Hi < 0 && c.lossCI.Hi < o.NonInferiorityMargin {
			cheaper = append(cheaper, c)
		}
		if o.MaxUSDPerNetWin != nil && c.lossCI.Hi < 0 && c.dCost > 0 && c.dLoss < 0 && c.dCost/-c.dLoss <= *o.MaxUSDPerNetWin {
			better = append(better, c)
		}
	}
	var pick *candidate
	var kind string
	switch {
	case len(better) > 0:
		sort.SliceStable(better, func(i, j int) bool { return better[i].dLoss < better[j].dLoss })
		pick, kind = &better[0], "better"
	case len(cheaper) > 0:
		sort.SliceStable(cheaper, func(i, j int) bool { return cheaper[i].dCost < cheaper[j].dCost })
		pick, kind = &cheaper[0], "cheaper"
	default:
		return Finding{}, false
	}

	to := round2(pick.t)
	f := Finding{
		Detector: DetectorThreshold, Selector: selector,
		Action: &Action{Kind: ActionSetThreshold, Selector: selector, Value: to},
		Evidence: Evidence{
			Samples: n, DeltaCostUSD: pick.dCost, FromThreshold: current, ToThreshold: to,
			LossChange: pick.dLoss, LossChangeHi: pick.lossCI.Hi, Frontier: frontier,
		},
	}
	if kind == "cheaper" {
		f.Summary = fmt.Sprintf("Moving %s's base threshold from %.2f to %.2f would have cost $%.4f less per call (at most $%.4f less, at least $%.4f) across %d judged calls, with no more than %.0f%% more calls routed to the model whose answer lost",
			selector, current, to, -pick.dCost, -pick.costCI.Lo, -pick.costCI.Hi, n, pick.lossCI.Hi*100)
	} else {
		f.Summary = fmt.Sprintf("Moving %s's base threshold from %.2f to %.2f would have sent %.1f%% fewer calls to the model whose answer lost (at least %.1f%% fewer) across %d judged calls, for $%.4f more per call: $%.4f per avoided loss, inside the limit of $%.4f",
			selector, current, to, -pick.dLoss*100, -pick.lossCI.Hi*100, n, pick.dCost, pick.dCost/-pick.dLoss, *o.MaxUSDPerNetWin)
	}
	return f, true
}

// agentVeto returns why moving the threshold is not safe for every Agent that
// uses the selector, or "". The sweep pools the Agents' calls; an Agent with
// enough calls of its own whose answers clearly get worse holds the change back.
func agentVeto(calls []Call, from, to float64, o Options) string {
	byAgent := map[string][]Call{}
	for _, c := range calls {
		byAgent[c.Agent] = append(byAgent[c.Agent], c)
	}
	agents := make([]string, 0, len(byAgent))
	for a := range byAgent {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	for _, a := range agents {
		cs := byAgent[a]
		if len(cs) < max(o.MinSamples/2, 1) {
			continue
		}
		ci := bootstrap(len(cs), o.Resamples, o.Confidence, o.Seed, func(idx []int) (float64, bool) {
			d := 0.0
			for _, i := range idx {
				_, lt := policyOutcome(cs[i], to)
				_, lf := policyOutcome(cs[i], from)
				d += lt - lf
			}
			return d / float64(len(idx)), true
		})
		if ci.Lo > o.NonInferiorityMargin {
			return fmt.Sprintf("agent %s uses the same selector and would clearly get worse answers at %.2f; give it a selector of its own or change the threshold by hand", a, to)
		}
	}
	return ""
}

func allIdx(n int) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	return idx
}

// paretoFrontier keeps the thresholds no other threshold beats on both cost and
// loss, in threshold order. The current threshold is always kept so the two can
// be compared.
func paretoFrontier(points []FrontierPoint) []FrontierPoint {
	var out []FrontierPoint
	for i, p := range points {
		dominated := false
		for j, q := range points {
			if i == j {
				continue
			}
			if q.CostUSD <= p.CostUSD && q.Loss <= p.Loss && (q.CostUSD < p.CostUSD || q.Loss < p.Loss) {
				dominated = true
				break
			}
		}
		if !dominated || p.Current {
			out = append(out, p)
		}
	}
	return out
}

// strategySwitches finds rule-based selectors whose routing barely separates
// the calls the capable model would win from the rest, and proposes a
// stage_router in their place. The samples cannot predict how a stage_router
// would behave, since it escalates on tool failures the replays do not show,
// so the finding says so.
func strategySwitches(in Input, o Options) []Finding {
	bySelector := map[string][]Call{}
	for _, c := range in.Calls {
		if name, ok := in.AgentSelector[c.Agent]; ok && in.Selectors[name].Strategy != strategySwitchyard {
			bySelector[name] = append(bySelector[name], c)
		}
	}
	names := make([]string, 0, len(bySelector))
	for n := range bySelector {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []Finding
	for _, name := range names {
		type pair struct{ capable, efficient string }
		count := map[pair]int{}
		for _, c := range bySelector[name] {
			count[pair{c.Capable, c.Efficient}]++
		}
		var dominant pair
		best := 0
		for p, n := range count {
			if n > best || (n == best && (p.capable < dominant.capable || (p.capable == dominant.capable && p.efficient < dominant.efficient))) {
				dominant, best = p, n
			}
		}
		if best < o.MinSamples {
			continue
		}
		var kc, nc, ke, ne int
		for _, c := range bySelector[name] {
			if c.Capable != dominant.capable || c.Efficient != dominant.efficient {
				continue
			}
			switch {
			case c.RoutedCapable:
				nc++
				if c.Result == CapableBetter {
					kc++
				}
			default:
				ne++
				if c.Result == CapableBetter {
					ke++
				}
			}
		}
		if nc == 0 || ne == 0 {
			continue // the selector sends everything one way; there is no routing to judge
		}
		lift, ci := NewcombeDiff(kc, nc, ke, ne, o.Confidence)
		if ci.Hi >= o.MinRoutingLift {
			continue
		}
		f := Finding{
			Detector: DetectorStrategySwitch, Selector: name,
			Action: &Action{Kind: ActionSwitchStrategy, Selector: name, Capable: dominant.capable, Efficient: dominant.efficient},
			Evidence: Evidence{
				Samples: nc + ne, RoutingLift: &lift, RoutingLiftHi: ci.Hi,
			},
			Caveat: "a stage_router escalates when tool calls fail, which replayed samples do not show, so its effect on cost and quality cannot be estimated from them",
		}
		f.Summary = fmt.Sprintf("%s's rules send the calls %s would win to %s no more often than chance (lift %.0f%%, at most %.0f%%, across %d judged calls); a stage_router between %s and %s routes on how the run is going instead",
			name, dominant.capable, dominant.capable, lift*100, ci.Hi*100, nc+ne, dominant.efficient, dominant.capable)
		out = append(out, f)
	}
	return out
}

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
	"context"
	"time"
)

// Query says whose evidence to load and how far back.
type Query struct {
	Namespace string
	// Agents are the Agents the benchmark covers.
	Agents []string
	// Since and Now bound the window.
	Since, Now time.Time
}

// Volume is how many calls a provider served for one Agent and category in the
// window, benchmarked or not. It scales a saving from the sample to the traffic.
type Volume struct {
	Agent, Category, Provider string
	Calls                     int
}

// Observations is everything the detectors read from the database.
type Observations struct {
	Samples       []Sample
	Volumes       []Volume
	ProviderStats []ProviderStat
	Ratings       map[AgentCategory]Ratings
}

// Source loads observations. *postgresql.Store implements it.
type Source interface {
	LoadObservations(ctx context.Context, q Query) (Observations, error)
}

// Cluster is what the detectors need to know about the cluster: the selectors,
// which one each Agent routes through, and what each provider costs.
type Cluster struct {
	Selectors     map[string]Selector
	AgentSelector map[string]string
	// ProviderCost is each provider's input plus output price per token, used to
	// tell the capable model from the efficient one when the selector names no
	// roles.
	ProviderCost map[string]float64
}

// tiersOf reads the capable and efficient roles from the Agent's selector.
func (c Cluster) tiersOf(agent string) Tiers {
	t := Tiers{CostPerToken: c.ProviderCost}
	if sel, ok := c.Selectors[c.AgentSelector[agent]]; ok {
		t.Capable, t.Efficient = sel.CapabilityRouting["capable"], sel.CapabilityRouting["efficient"]
	}
	return t
}

// AnalyzeDowngrade checks a change that sent one Agent's kind of work to the
// cheaper model against the pairs replayed since it was applied. No pairs for
// that Agent and category is Inconclusive.
func AnalyzeDowngrade(ev Observations, c Cluster, agent, category string, opts Options) (verdict string, w Worth) {
	for _, g := range Aggregate(ev.Samples, c.tiersOf) {
		if g.Agent == agent && g.Category == category {
			return CheckDowngrade(g, opts)
		}
	}
	return VerdictInconclusive, Worth{}
}

// Analyze turns loaded observations into findings. It is pure: the same observations,
// cluster and options always give the same findings.
func Analyze(ev Observations, c Cluster, q Query, opts Options) []Finding {
	days := q.Now.Sub(q.Since).Hours() / 24
	volume := map[Volume]int{}
	for _, v := range ev.Volumes {
		volume[Volume{Agent: v.Agent, Category: v.Category, Provider: v.Provider}] += v.Calls
	}
	groups := Aggregate(ev.Samples, c.tiersOf)
	for i := range groups {
		g := &groups[i]
		g.RoutedCapableCalls = volume[Volume{Agent: g.Agent, Category: g.Category, Provider: g.Capable}]
		g.WindowDays = days
	}
	return Detect(Input{
		Groups: groups, Calls: Orient(ev.Samples, c.tiersOf),
		Selectors: c.Selectors, AgentSelector: c.AgentSelector,
		ProviderStats: ev.ProviderStats, Ratings: ev.Ratings, Options: opts,
	})
}

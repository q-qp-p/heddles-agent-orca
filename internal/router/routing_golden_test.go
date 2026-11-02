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
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite the expected decisions in testdata/routing")

// routingFixture is one recorded routing scenario: the configuration and
// request state going in, and the decision that came out. A routing change
// shows up as a fixture diff, so every behavior change is deliberate and
// visible in review.
type routingFixture struct {
	Description string `json:"description"`

	Providers         []fixtureProvider `json:"providers"`
	CapabilityRouting map[string]string `json:"capabilityRouting,omitempty"`
	BudgetPerRunUSD   float64           `json:"budgetPerRunUSD,omitempty"`
	SpentUSD          float64           `json:"spentUSD,omitempty"`
	ElapsedSec        float64           `json:"elapsedSec,omitempty"`

	Messages            []Message `json:"messages"`
	EstimatedTokens     int       `json:"estimatedTokens,omitempty"`
	RemainingTimeoutSec float64   `json:"remainingTimeoutSec,omitempty"`
	// RandPick pins the weighted-random draw.
	RandPick int `json:"randPick,omitempty"`

	Expect fixtureDecision `json:"expect"`
}

type fixtureProvider struct {
	Name              string   `json:"name"`
	Weight            int      `json:"weight"`
	Capabilities      []string `json:"capabilities,omitempty"`
	ContextWindow     int      `json:"contextWindow,omitempty"`
	MaxRequestTokens  int      `json:"maxRequestTokens,omitempty"`
	CostPerInputToken float64  `json:"costPerInputToken,omitempty"`
	LatencyProfile    string   `json:"latencyProfile,omitempty"`
}

type fixtureDecision struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
	Stage    string `json:"stage"`
}

func (f routingFixture) decide(t *testing.T) fixtureDecision {
	t.Helper()
	cfg := &Config{
		Strategy:          strategyRuleBased,
		CapabilityRouting: f.CapabilityRouting,
		BudgetPerRunUSD:   f.BudgetPerRunUSD,
	}
	for _, p := range f.Providers {
		cfg.Providers = append(cfg.Providers, ProviderConfig{
			Name: p.Name, LiteLLMModel: "test/" + p.Name, Weight: p.Weight, Capabilities: p.Capabilities,
			ContextWindow: p.ContextWindow, MaxRequestTokens: p.MaxRequestTokens,
			CostPerInputToken: p.CostPerInputToken, LatencyProfile: p.LatencyProfile,
		})
	}
	rule := NewRuleRouter(cfg)
	rule.randIntn = func(int) int { return f.RandPick }
	rule.UpdateSpend(f.SpentUSD)
	rule.startedAt = time.Now().Add(-time.Duration(f.ElapsedSec * float64(time.Second)))

	sel, err := newLegacySelector(cfg, rule, nil).Select(context.Background(), SelectionInput{
		Messages:            f.Messages,
		EstimatedTokens:     f.EstimatedTokens,
		RemainingTimeoutSec: f.RemainingTimeoutSec,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Provider == nil {
		t.Fatalf("no provider selected")
	}
	return fixtureDecision{Provider: sel.Provider.Name, Path: sel.Path, Stage: sel.Category.Stage}
}

// TestRoutingGolden replays every fixture in testdata/routing. Run with
// -update to record decisions after an intended routing change, then review
// the fixture diff.
func TestRoutingGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "routing", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no routing fixtures found (err %v)", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file[:len(file)-len(".json")]), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var f routingFixture
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("decoding %s: %v", file, err)
			}
			got := f.decide(t)
			if *updateGolden {
				f.Expect = got
				var out bytes.Buffer
				enc := json.NewEncoder(&out)
				enc.SetIndent("", "  ")
				enc.SetEscapeHTML(false)
				if err := enc.Encode(f); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, out.Bytes(), 0o644); err != nil { //nolint:gosec // fixture file
					t.Fatal(err)
				}
				return
			}
			if got != f.Expect {
				t.Errorf("%s\ndecision changed: got %+v, recorded %+v\n(if intended, rerun with -update and review the fixture diff)",
					f.Description, got, f.Expect)
			}
		})
	}
}

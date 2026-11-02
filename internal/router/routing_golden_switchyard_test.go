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
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// switchyardFixture is one recorded Switchyard decision: the algorithm and
// models going in, a tool-call history, what the judge (if any) answers, and the
// decision that came out. Like the legacy fixtures, a change in what Switchyard
// decides shows up as a diff to review, and is how a Switchyard bump is checked.
type switchyardFixture struct {
	Description       string            `json:"description"`
	Algorithm         json.RawMessage   `json:"algorithm"`
	Providers         []fixtureProvider `json:"providers"`
	CapabilityRouting map[string]string `json:"capabilityRouting"`
	FallbackChain     []string          `json:"fallbackChain,omitempty"`
	History           fixtureHistory    `json:"history"`
	Judge             fixtureJudge      `json:"judge,omitempty"`
	Expect            switchyardOutcome `json:"expect"`
}

// fixtureHistory is a conversation of Rounds tool calls of Tool, each answered with Result.
type fixtureHistory struct {
	Rounds int    `json:"rounds"`
	Tool   string `json:"tool,omitempty"`
	Result string `json:"result,omitempty"`
}

// fixtureJudge scripts the judge model: it answers a verdict with Score, or fails with Error.
type fixtureJudge struct {
	Score float64 `json:"score,omitempty"`
	Error string  `json:"error,omitempty"`
}

type switchyardOutcome struct {
	Provider   string   `json:"provider"`
	Path       string   `json:"path"`
	Algorithm  string   `json:"algorithm"`
	Label      string   `json:"label"`
	Candidates []string `json:"candidates"`
	JudgeCalls int      `json:"judgeCalls"`
}

func (f switchyardFixture) decide(t *testing.T) switchyardOutcome {
	t.Helper()
	cfg := &Config{
		Strategy:          strategySwitchyard,
		CapabilityRouting: f.CapabilityRouting,
		FallbackChain:     f.FallbackChain,
		Switchyard:        &SwitchyardConfig{Algorithm: f.Algorithm},
	}
	for _, p := range f.Providers {
		cfg.Providers = append(cfg.Providers, ProviderConfig{Name: p.Name, LiteLLMModel: "test/" + p.Name, Weight: p.Weight})
	}
	judgeCalls := 0
	forward := func(context.Context, *ProviderConfig, ChatCompletionRequest) ([]byte, error) {
		judgeCalls++
		if f.Judge.Error != "" {
			return nil, errors.New(f.Judge.Error)
		}
		return verdictBody(f.Judge.Score, 100), nil
	}
	// The rule router behind Switchyard draws at random when no rule applies; pin
	// the draw so a fail-open fixture records a decision, not a coin flip.
	rule := NewRuleRouter(cfg)
	rule.randIntn = func(int) int { return 0 }
	s := newSwitchyardSelector(cfg, newLegacySelector(cfg, rule, nil), forward)
	defer func() { _ = s.Close() }()

	var msgs []Message
	if f.History.Rounds > 0 {
		msgs = toolRounds(f.History.Rounds, f.History.Tool, f.History.Result)
	} else {
		msgs = []Message{{Role: "user", Content: "Fix the failing test in parser.go"}}
	}
	sel, err := s.Select(context.Background(), SelectionInput{Messages: msgs, Session: SessionInfo{ID: "golden"}})
	if err != nil || sel.Provider == nil {
		t.Fatalf("Select = %+v, %v", sel, err)
	}
	return switchyardOutcome{
		Provider: sel.Provider.Name, Path: sel.Path, Algorithm: sel.Algorithm,
		Label: sel.Category.SYLabel, Candidates: sel.Candidates, JudgeCalls: judgeCalls,
	}
}

func equalOutcome(a, b switchyardOutcome) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// TestRoutingGoldenSwitchyard replays testdata/routing/switchyard. Run with
// -update after an intended change (or a Switchyard bump) and review the diff.
func TestRoutingGoldenSwitchyard(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "routing", "switchyard", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no switchyard fixtures found (err %v)", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file[:len(file)-len(".json")]), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var f switchyardFixture
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
			if !equalOutcome(got, f.Expect) {
				t.Errorf("%s\ndecision changed: got %+v, recorded %+v\n(if intended, rerun with -update and review the fixture diff)",
					f.Description, got, f.Expect)
			}
		})
	}
}

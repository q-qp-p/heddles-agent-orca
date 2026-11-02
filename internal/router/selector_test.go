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
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
)

// metaFakeForward answers every meta-router prompt with reply and counts calls.
func metaFakeForward(reply string, calls *atomic.Int32, err error) chatForwarder {
	return func(context.Context, *ProviderConfig, ChatCompletionRequest) ([]byte, error) {
		calls.Add(1)
		if err != nil {
			return nil, err
		}
		return []byte(`{"choices":[{"message":{"content":"` + reply + `"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`), nil
	}
}

func hybridConfig() *Config {
	return &Config{
		Strategy:               "hybrid",
		Providers:              ruleRouterTestProviders(),
		CapabilityRouting:      map[string]string{},
		FallbackChain:          []string{providerBig, providerCheap, "fastonly"},
		MetaRouterProviderName: providerCheap,
		MetaRouterThreshold:    0.6,
		AgentName:              "agent-a",
		WorkflowStep:           "implement",
	}
}

func newTestLegacySelector(cfg *Config, forward chatForwarder) *legacySelector {
	var meta *MetaRouter
	if cfg.Strategy == "hybrid" || cfg.Strategy == "llm-meta" {
		meta = NewMetaRouter(cfg, forward)
	}
	return newLegacySelector(cfg, NewRuleRouter(cfg), meta)
}

func TestLegacySelector_RulePath(t *testing.T) {
	cfg := hybridConfig()
	cfg.Strategy = strategyRuleBased
	s := newTestLegacySelector(cfg, nil)

	sel, err := s.Select(context.Background(), SelectionInput{Messages: ruleUserTurn("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider == nil || sel.Path != pathWeighted || sel.Strategy != strategyRuleBased {
		t.Errorf("selection = %+v", sel)
	}
	if sel.DecisionID == "" {
		t.Errorf("no decision ID")
	}
	if len(sel.AuxUsage) != 0 {
		t.Errorf("rule-based selection made aux calls: %+v", sel.AuxUsage)
	}
}

func TestLegacySelector_DecisionIDsAreUnique(t *testing.T) {
	cfg := hybridConfig()
	cfg.Strategy = strategyRuleBased
	s := newTestLegacySelector(cfg, nil)
	seen := map[string]bool{}
	for range 50 {
		sel, _ := s.Select(context.Background(), SelectionInput{Messages: ruleUserTurn("hi")})
		if seen[sel.DecisionID] {
			t.Fatalf("duplicate decision ID %q", sel.DecisionID)
		}
		seen[sel.DecisionID] = true
	}
}

func TestLegacySelector_CategoryAndCandidates(t *testing.T) {
	cfg := hybridConfig()
	cfg.Strategy = strategyRuleBased
	s := newTestLegacySelector(cfg, nil)

	msgs := []Message{{Role: "user", Content: "fix it"}, {Role: "tool", Content: "ok", ToolCallID: "t1"}}
	sel, _ := s.Select(context.Background(), SelectionInput{Messages: msgs})

	want := Category{Stage: stageToolExec, StageConf: stageConfToolExec, Version: categoryVersion, WorkflowStep: "implement", Agent: "agent-a"}
	if sel.Category != want {
		t.Errorf("Category = %+v, want %+v", sel.Category, want)
	}
	if sel.Candidates[0] != sel.Provider.Name {
		t.Errorf("Candidates[0] = %q, want the chosen provider %q", sel.Candidates[0], sel.Provider.Name)
	}
	if len(sel.Candidates) != 3 || len(slices.Compact(slices.Sorted(slices.Values(sel.Candidates)))) != 3 {
		t.Errorf("Candidates = %v, want chosen + fallbacks without duplicates", sel.Candidates)
	}
}

func TestLegacySelector_MetaRouterConsultedWhenRulesUnsure(t *testing.T) {
	var calls atomic.Int32
	cfg := hybridConfig()
	s := newTestLegacySelector(cfg, metaFakeForward(providerBig, &calls, nil))

	in := SelectionInput{Messages: ruleUserTurn("something ambiguous")}
	sel, err := s.Select(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name != providerBig || sel.Path != "meta" || sel.Confidence != confMeta {
		t.Errorf("selection = provider %q path %q conf %v", sel.Provider.Name, sel.Path, sel.Confidence)
	}
	if len(sel.AuxUsage) != 1 || sel.AuxUsage[0].Provider != providerCheap || sel.AuxUsage[0].Usage.PromptTokens != 10 {
		t.Errorf("AuxUsage = %+v, want the meta-router call charged to its provider", sel.AuxUsage)
	}

	// Same task, same state: served from the meta-router cache, no new LLM call.
	again, _ := s.Select(context.Background(), in)
	if again.Path != "meta-cache" || len(again.AuxUsage) != 0 || calls.Load() != 1 {
		t.Errorf("second call: path %q aux %d calls %d, want meta-cache/0/1", again.Path, len(again.AuxUsage), calls.Load())
	}
}

func TestLegacySelector_ConfidentRuleSkipsMetaRouter(t *testing.T) {
	var calls atomic.Int32
	cfg := hybridConfig()
	s := newTestLegacySelector(cfg, metaFakeForward(providerCheap, &calls, nil))

	imageMsg := []Message{{Role: "user", Content: []any{map[string]any{"type": "image_url"}}}}
	sel, _ := s.Select(context.Background(), SelectionInput{Messages: imageMsg})
	if sel.Path != pathVision || sel.Provider.Name != providerBig {
		t.Errorf("path %q provider %q, want the vision rule on big", sel.Path, sel.Provider.Name)
	}
	if calls.Load() != 0 {
		t.Errorf("meta-router called %d times for a 0.95-confidence rule", calls.Load())
	}
}

func TestLegacySelector_MetaFailureKeepsRuleDecision(t *testing.T) {
	var calls atomic.Int32
	cfg := hybridConfig()
	s := newTestLegacySelector(cfg, metaFakeForward("", &calls, errors.New("provider down")))

	sel, err := s.Select(context.Background(), SelectionInput{Messages: ruleUserTurn("ambiguous")})
	if err != nil {
		t.Fatalf("a meta-router failure must not fail selection: %v", err)
	}
	if sel.Path != pathWeighted || sel.Provider == nil {
		t.Errorf("path %q provider %v, want the rule decision kept", sel.Path, sel.Provider)
	}
}

func TestLegacySelector_UnknownProviderFallsBackToFirst(t *testing.T) {
	cfg := hybridConfig()
	cfg.Strategy = strategyRuleBased
	cfg.CapabilityRouting = map[string]string{"code": "deleted-provider"}
	s := newTestLegacySelector(cfg, nil)

	sel, _ := s.Select(context.Background(), SelectionInput{Messages: ruleUserTurn("implement a function")})
	if sel.Provider == nil || sel.Provider.Name != cfg.Providers[0].Name {
		t.Errorf("provider = %v, want the first configured provider", sel.Provider)
	}
}

func TestLegacySelector_NoProviders(t *testing.T) {
	cfg := &Config{Strategy: strategyRuleBased}
	s := newTestLegacySelector(cfg, nil)
	sel, err := s.Select(context.Background(), SelectionInput{Messages: ruleUserTurn("hi")})
	if err != nil || sel.Provider != nil {
		t.Errorf("provider %v err %v, want nil provider and no error", sel.Provider, err)
	}
}

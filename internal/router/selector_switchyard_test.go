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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	syStrong = "strong"
	syWeak   = "weak"
	syJudge  = "judge"

	stageRouterSpec = `{"type":"stage_router","picker":"efficient_first","capable_target":"strong",` +
		`"efficient_target":"weak","confidence_threshold":0.5}`
	judgedSpec = `{"type":"composite","classifier":{"target":"judge","base_threshold":0.5,` +
		`"classify_trigger":"user_turn","fail_open":false},"stage":{"capable_target":"strong",` +
		`"efficient_target":"weak","confidence_threshold":0.5}}`

	failingBashOutput = "Traceback (most recent call last):\n  File x\nError: command failed with exit code 1\n" +
		"FAILED tests/test_parser.py::test_x"
	cleanEditOutput = "The file parser.go has been updated."
)

func switchyardTestConfig(spec string) *Config {
	return &Config{
		RunName:  "run-1",
		Strategy: strategySwitchyard,
		Providers: []ProviderConfig{
			{Name: syWeak, LiteLLMModel: "openai/weak", Weight: 1, CostPerInputToken: 0.000001},
			{Name: syStrong, LiteLLMModel: "openai/strong", Weight: 1, CostPerInputToken: 0.00001},
			{Name: syJudge, LiteLLMModel: "openai/judge", Weight: 0, CostPerInputToken: 0.000002, CostPerOutputToken: 0.000004},
		},
		CapabilityRouting: map[string]string{"capable": syStrong, "efficient": syWeak, "judge": syJudge},
		Switchyard:        &SwitchyardConfig{Algorithm: json.RawMessage(spec)},
	}
}

// toolRounds is a conversation holding n tool calls of the given tool, each answered with result.
func toolRounds(n int, tool, result string) []Message {
	msgs := make([]Message, 1, 1+2*n)
	msgs[0] = Message{Role: "user", Content: "Fix the failing test in parser.go"}
	for i := range n {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			Message{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: id, Type: "function",
				Function: FunctionCall{Name: tool, Arguments: `{"file_path":"parser.go"}`}}}},
			Message{Role: "tool", ToolCallID: id, Content: result})
	}
	return msgs
}

func noJudge(context.Context, *ProviderConfig, ChatCompletionRequest) ([]byte, error) {
	return nil, errors.New("no model calls expected")
}

func newSY(t *testing.T, cfg *Config, forward chatForwarder) *switchyardSelector {
	t.Helper()
	legacy := newLegacySelector(cfg, NewRuleRouter(cfg), nil)
	s := newSwitchyardSelector(cfg, legacy, forward)
	t.Cleanup(func() { _ = s.Close() })
	if s.initErr != nil {
		t.Fatalf("engine did not start: %v", s.initErr)
	}
	return s
}

func selectSY(t *testing.T, s *switchyardSelector, msgs []Message, session string) Selection {
	t.Helper()
	sel, err := s.Select(context.Background(), SelectionInput{Messages: msgs, Session: SessionInfo{ID: session}})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	return sel
}

func TestSwitchyardSelector_RoutesByToolHistory(t *testing.T) {
	s := newSY(t, switchyardTestConfig(stageRouterSpec), noJudge)

	clean := selectSY(t, s, toolRounds(5, "Edit", cleanEditOutput), "clean")
	if clean.Provider.Name != syWeak || clean.Path != pathSwitchyard || clean.Algorithm != "stage_router" {
		t.Errorf("clean history: provider %s path %s algorithm %s", clean.Provider.Name, clean.Path, clean.Algorithm)
	}
	if !reflect.DeepEqual(clean.Candidates, []string{syWeak, syStrong}) {
		t.Errorf("candidates = %v, want the efficient model with the capable one as its fallback", clean.Candidates)
	}
	if clean.Confidence < 0.5 || clean.Category.SYLabel != "dimensions" || len(clean.Signals) == 0 {
		t.Errorf("evidence not carried: confidence %v label %q signals %s", clean.Confidence, clean.Category.SYLabel, clean.Signals)
	}
	if clean.Category.Stage == "" || clean.DecisionID == "" {
		t.Errorf("the stage category and decision ID must be set on every decision: %+v", clean)
	}

	failing := selectSY(t, s, toolRounds(5, "Bash", failingBashOutput), "failing")
	if failing.Provider.Name != syStrong || failing.Category.SYLabel != "override" {
		t.Errorf("failing history: provider %s label %q, want the capable model by hard override", failing.Provider.Name, failing.Category.SYLabel)
	}
}

func TestSwitchyardSelector_CandidatesAppendTheFallbackChain(t *testing.T) {
	cfg := switchyardTestConfig(stageRouterSpec)
	cfg.FallbackChain = []string{syJudge, syWeak}
	s := newSY(t, cfg, noJudge)
	sel := selectSY(t, s, toolRounds(5, "Edit", cleanEditOutput), "s")
	if want := []string{syWeak, syStrong, syJudge}; !reflect.DeepEqual(sel.Candidates, want) {
		t.Errorf("candidates = %v, want %v: Switchyard's order first, then the chain without repeats", sel.Candidates, want)
	}
}

func TestSwitchyardSelector_FailsOpenToTheRuleRouter(t *testing.T) {
	// "advisor" answers on the model's behalf: not served, so the engine never starts.
	cfg := switchyardTestConfig(`{"type":"advisor"}`)
	legacy := newLegacySelector(cfg, NewRuleRouter(cfg), nil)
	s := newSwitchyardSelector(cfg, legacy, noJudge)
	defer func() { _ = s.Close() }()
	if s.initErr == nil {
		t.Fatal("an unsupported algorithm started an engine")
	}

	sel, err := s.Select(context.Background(), SelectionInput{Messages: toolRounds(1, "Edit", cleanEditOutput)})
	if err != nil || sel.Provider == nil {
		t.Fatalf("Select = %+v, %v: routing must never fail because Switchyard cannot decide", sel, err)
	}
	if sel.Path != pathSwitchyardFailOpen || !strings.Contains(sel.Reason, "switchyard failed") || !strings.Contains(sel.Reason, "advisor") {
		t.Errorf("path %q reason %q: the failure must be visible", sel.Path, sel.Reason)
	}
	if !strings.Contains(string(sel.Signals), "error") || sel.Algorithm != "advisor" {
		t.Errorf("signals %s algorithm %q", sel.Signals, sel.Algorithm)
	}
	if sel.Category.Stage == "" || sel.DecisionID == "" {
		t.Errorf("a fail-open decision is still a full decision: %+v", sel)
	}
}

func TestSwitchyardSelector_FailsOpenOnAMissingRole(t *testing.T) {
	cfg := switchyardTestConfig(stageRouterSpec)
	delete(cfg.CapabilityRouting, "capable")
	delete(cfg.CapabilityRouting, "efficient")
	s := newSY(t, cfg, noJudge)
	sel := selectSY(t, s, toolRounds(5, "Edit", cleanEditOutput), "s")
	// Whatever Switchyard makes of an empty capable/efficient group, the router
	// must come back with a usable provider.
	if sel.Provider == nil {
		t.Fatalf("no provider: %+v", sel)
	}
}

func verdictBody(pSolve float64, promptTokens int) []byte {
	text := fmt.Sprintf(`{"crux":"bounded task","primary_rule":"SUP-1","capability_boundary":"supported","p_solve":%v}`, pSolve)
	b, _ := json.Marshal(map[string]any{
		"id": "j", "object": "chat.completion",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}},
		"usage":   map[string]any{"prompt_tokens": promptTokens, "completion_tokens": 20},
	})
	return b
}

func TestSwitchyardSelector_JudgeCallsGoThroughTheRoutersForwardersAndAreCharged(t *testing.T) {
	var gotProvider string
	var gotReq ChatCompletionRequest
	s := newSY(t, switchyardTestConfig(judgedSpec), func(_ context.Context, p *ProviderConfig, req ChatCompletionRequest) ([]byte, error) {
		gotProvider, gotReq = p.Name, req
		return verdictBody(0.95, 300), nil
	})
	sel := selectSY(t, s, []Message{{Role: "user", Content: "rename a variable"}}, "s")

	if sel.Provider.Name != syWeak || sel.Category.SYLabel != "llm-classifier" {
		t.Errorf("provider %s label %q, want the efficient model for an easy task, labelled by its source", sel.Provider.Name, sel.Category.SYLabel)
	}
	if gotProvider != syJudge || len(gotReq.Messages) == 0 || gotReq.Stream {
		t.Errorf("judge call = provider %q messages %d stream %v", gotProvider, len(gotReq.Messages), gotReq.Stream)
	}
	if len(sel.AuxUsage) != 1 || sel.AuxUsage[0].Provider != syJudge || sel.AuxUsage[0].Usage.PromptTokens != 300 {
		t.Errorf("AuxUsage = %+v, want the judge call so the run is charged for it", sel.AuxUsage)
	}
	if sel.Confidence != 0.95 {
		t.Errorf("confidence = %v, want the judge's score", sel.Confidence)
	}
}

func TestSwitchyardSelector_AFailedJudgeStillFailsOpenAndKeepsTheBill(t *testing.T) {
	var calls atomic.Int32
	s := newSY(t, switchyardTestConfig(judgedSpec), func(context.Context, *ProviderConfig, ChatCompletionRequest) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("judge down")
	})
	sel := selectSY(t, s, []Message{{Role: "user", Content: "rename a variable"}}, "s")
	if sel.Path != pathSwitchyardFailOpen || sel.Provider == nil {
		t.Errorf("path %q provider %v", sel.Path, sel.Provider)
	}
	if calls.Load() == 0 || !strings.Contains(sel.Reason, "judge down") {
		t.Errorf("calls %d reason %q: the judge failure should be the stated cause", calls.Load(), sel.Reason)
	}
}

func TestSwitchyardSelector_SessionsDoNotShareState(t *testing.T) {
	s := newSY(t, switchyardTestConfig(stageRouterSpec), noJudge)
	clean := toolRounds(5, "Edit", cleanEditOutput)

	if got := selectSY(t, s, toolRounds(5, "Bash", failingBashOutput), "alpha"); got.Provider.Name != syStrong {
		t.Fatalf("setup: failures selected %s", got.Provider.Name)
	}
	if got := selectSY(t, s, clean, "alpha"); got.Provider.Name != syStrong {
		t.Errorf("same session selected %s, want the capable model held after escalating", got.Provider.Name)
	}
	if got := selectSY(t, s, clean, "beta"); got.Provider.Name != syWeak {
		t.Errorf("another session selected %s, want its own (efficient) state", got.Provider.Name)
	}
}

func TestSwitchyardSelector_NewSessionStartsClean(t *testing.T) {
	cfg := switchyardTestConfig(stageRouterSpec)
	cfg.SessionID = "warm" // the session the pod was claimed for
	s := newSY(t, cfg, noJudge)
	clean := toolRounds(5, "Edit", cleanEditOutput)

	selectSY(t, s, toolRounds(5, "Bash", failingBashOutput), "warm")
	s.mu.Lock()
	before := s.engine
	s.mu.Unlock()

	s.NewSession("warm") // the same conversation: keep the engine and its holds
	s.mu.Lock()
	same := s.engine == before
	s.mu.Unlock()
	if !same {
		t.Error("NewSession rebuilt the engine for the same session")
	}
	if got := selectSY(t, s, clean, "warm"); got.Provider.Name != syStrong {
		t.Errorf("the hold was lost: selected %s", got.Provider.Name)
	}

	s.NewSession("next") // a warm pod claimed for another conversation
	// Even addressed by the old session ID, nothing may carry over.
	if got := selectSY(t, s, clean, "warm"); got.Provider.Name != syWeak {
		t.Errorf("after a new session selected %s, want a clean engine (efficient)", got.Provider.Name)
	}
}

func TestSwitchyardSelector_ConcurrentSelectsAreSafe(t *testing.T) {
	s := newSY(t, switchyardTestConfig(stageRouterSpec), noJudge)
	done := make(chan struct{})
	for i := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 3 {
				_, _ = s.Select(context.Background(), SelectionInput{
					Messages: toolRounds(i%4, "Edit", cleanEditOutput), Session: SessionInfo{ID: fmt.Sprintf("s%d", i%3)},
				})
			}
		}()
	}
	for range 8 {
		<-done
	}
}

func TestSwitchyardGroups(t *testing.T) {
	cfg := switchyardTestConfig(stageRouterSpec)
	cfg.CapabilityRouting["subagent.explore"] = syWeak
	cfg.CapabilityRouting["code"] = syStrong // a keyword hint, not a role
	cfg.CapabilityRouting["capable"] = "deleted-provider"

	groups, subagent := switchyardGroups(cfg)
	if want := []string{syWeak, syStrong}; !reflect.DeepEqual(groups["any"], want) {
		t.Errorf("any = %v, want the weighted providers %v (the judge, weight 0, does not answer)", groups["any"], want)
	}
	if _, ok := groups["capable"]; ok {
		t.Errorf("capable names a provider that no longer exists, but groups = %v", groups)
	}
	if !reflect.DeepEqual(groups["efficient"], []string{syWeak}) || !reflect.DeepEqual(groups["judge"], []string{syJudge}) {
		t.Errorf("groups = %v", groups)
	}
	if _, ok := groups["code"]; ok {
		t.Errorf("a keyword hint became a group: %v", groups)
	}
	if !reflect.DeepEqual(subagent["explore"], []string{syWeak}) || !reflect.DeepEqual(subagent["any"], groups["any"]) {
		t.Errorf("subagent groups = %v", subagent)
	}
}

func TestSwitchyardGroups_NoWeightedProvidersMeansAllMayAnswer(t *testing.T) {
	cfg := switchyardTestConfig(stageRouterSpec)
	for i := range cfg.Providers {
		cfg.Providers[i].Weight = 0
	}
	groups, subagent := switchyardGroups(cfg)
	if len(groups["any"]) != 3 || subagent != nil {
		t.Errorf("any = %v subagent = %v", groups["any"], subagent)
	}
}

func TestToolSemantics(t *testing.T) {
	got := toolSemantics([]ToolDefinition{
		{Name: "lookup", SideEffect: "read"},
		{Name: "update", SideEffect: "write"},
		{Name: "open_valve", SideEffect: "actuate"},
		{Name: "mystery"},
	})
	if !reflect.DeepEqual(got.Observe, []string{"lookup"}) || !reflect.DeepEqual(got.Mutate, []string{"update", "open_valve"}) {
		t.Errorf("semantics = %+v: reads observe, writes and actuations mutate, unclassified tools are left out", got)
	}
}

func TestSwitchyardRequest_CarriesOnlyStandardChatFields(t *testing.T) {
	body, err := switchyardRequest([]Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "c1", Index: 3, Function: FunctionCall{Name: "Edit", Arguments: "{}"},
			ExtraContent: json.RawMessage(`{"google":{"thought_signature":"sig"}}`),
		}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, banned := range []string{"extra_content", "thought_signature", `"index"`} {
		if strings.Contains(s, banned) {
			t.Errorf("request carries %s: %s", banned, s)
		}
	}
	for _, want := range []string{`"type":"function"`, `"tool_call_id":"c1"`, `"content":""`} {
		if !strings.Contains(s, want) {
			t.Errorf("request lacks %s: %s", want, s)
		}
	}
}

func TestParseSignals_IsTolerant(t *testing.T) {
	tests := []struct {
		name, raw  string
		conf       float64
		label, src string
	}{
		{"stage router", `{"algorithm":"stage_router","evidence":{"source":"dimensions","confidence":0.87}}`, 0.87, "dimensions", "dimensions"},
		{"judge score", `{"evidence":{"source":"llm-classifier","score":0.95,"threshold":0.5}}`, 0.95, "llm-classifier", "llm-classifier"},
		{"judge verdict", `{"evidence":{"source":"classifier","p_solve":0.4,"primary_rule":"SUP-2"}}`, 0.4, "SUP-2", "classifier"},
		{"capability boundary", `{"evidence":{"capability_boundary":"unsupported"}}`, 0, "unsupported", ""},
		{"no evidence", `{"algorithm":"random"}`, 0, "", ""},
		{"not json", `nope`, 0, "", ""},
		{"empty", ``, 0, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf, label, src := parseSignals(json.RawMessage(tt.raw))
			if conf != tt.conf || label != tt.label || src != tt.src {
				t.Errorf("got %v %q %q, want %v %q %q", conf, label, src, tt.conf, tt.label, tt.src)
			}
		})
	}
}

// --- through the real request path -------------------------------------------

func syRouter(t *testing.T, mutate func(*Config)) (*Router, map[string]*capturingProvider) {
	t.Helper()
	providers := map[string]*capturingProvider{
		syWeak: newCapturingProvider(t, false), syStrong: newCapturingProvider(t, false),
	}
	r := newSelectionTestRouter(t, func(c *Config) {
		c.Strategy = strategySwitchyard
		c.CapabilityRouting = map[string]string{"capable": syStrong, "efficient": syWeak}
		c.Switchyard = &SwitchyardConfig{Algorithm: json.RawMessage(stageRouterSpec)}
		c.Providers = []ProviderConfig{
			{Name: syWeak, LiteLLMModel: "openai/weak", BaseURL: providers[syWeak].srv.URL, APIKeyFile: c.SATokenFile, Weight: 1},
			{Name: syStrong, LiteLLMModel: "openai/strong", BaseURL: providers[syStrong].srv.URL, APIKeyFile: c.SATokenFile, Weight: 1},
		}
		if mutate != nil {
			mutate(c)
		}
	})
	return r, providers
}

func chatBody(t *testing.T, msgs []Message) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "default", "stream": true, "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSwitchyard_ARequestGoesToTheProviderSwitchyardChose(t *testing.T) {
	for _, tc := range []struct {
		name         string
		history      []Message
		wantHit, not string
	}{
		{"clean production", toolRounds(5, "Edit", cleanEditOutput), syWeak, syStrong},
		{"repeated failures", toolRounds(5, "Bash", failingBashOutput), syStrong, syWeak},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, providers := syRouter(t, nil)
			if code := serve(r, chatBody(t, tc.history)).Code; code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			if got := len(providers[tc.wantHit].requests()); got != 1 {
				t.Errorf("%s received %d requests, want 1", tc.wantHit, got)
			}
			if got := len(providers[tc.not].requests()); got != 0 {
				t.Errorf("%s received %d requests, want none", tc.not, got)
			}
		})
	}
}

func TestSwitchyard_TelemetryRecordsTheAlgorithmAndPath(t *testing.T) {
	rec := newEventRecorder(t)
	r, _ := syRouter(t, func(c *Config) { c.OperatorAPIURL = rec.srv.URL; c.SelectorName = "adaptive" })

	if code := serve(r, chatBody(t, toolRounds(5, "Bash", failingBashOutput))).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	r.Finalize()

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	e := events[0]
	if e.Algorithm != "stage_router" || e.Path != pathSwitchyard || e.Provider != syStrong || e.SYLabel != "override" {
		t.Errorf("event = algorithm %q path %q provider %q label %q", e.Algorithm, e.Path, e.Provider, e.SYLabel)
	}
	if len(e.Signals) == 0 || e.Confidence != 0 && e.Confidence > 1 {
		t.Errorf("signals %s confidence %v", e.Signals, e.Confidence)
	}
}

// When the chosen provider fails, the next of Switchyard's own candidates is
// tried: the other model it ranked, even with no fallback chain configured.
func TestSwitchyard_FallbackFollowsSwitchyardsCandidates(t *testing.T) {
	failing := newCapturingProvider(t, true)
	working := newCapturingProvider(t, false)
	r := newSelectionTestRouter(t, func(c *Config) {
		c.Strategy = strategySwitchyard
		c.CapabilityRouting = map[string]string{"capable": syStrong, "efficient": syWeak}
		c.Switchyard = &SwitchyardConfig{Algorithm: json.RawMessage(stageRouterSpec)}
		c.Providers = []ProviderConfig{
			{Name: syWeak, LiteLLMModel: "openai/weak", BaseURL: failing.srv.URL, APIKeyFile: c.SATokenFile, Weight: 1},
			{Name: syStrong, LiteLLMModel: "openai/strong", BaseURL: working.srv.URL, APIKeyFile: c.SATokenFile, Weight: 1},
		}
	})
	if len(r.cfg.FallbackChain) != 0 {
		t.Fatal("setup: this test needs an empty fallback chain")
	}
	if code := serve(r, chatBody(t, toolRounds(5, "Edit", cleanEditOutput))).Code; code != http.StatusOK {
		t.Fatalf("status = %d, want the request served by the second candidate", code)
	}
	if len(failing.requests()) != 1 || len(working.requests()) != 1 {
		t.Errorf("weak got %d, strong got %d requests; want 1 and 1", len(failing.requests()), len(working.requests()))
	}
}

func TestSwitchyard_JudgeCostIsChargedToTheRun(t *testing.T) {
	judgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(verdictBody(0.95, 1000))
	}))
	t.Cleanup(judgeSrv.Close)
	r, _ := syRouter(t, func(c *Config) {
		c.CapabilityRouting["judge"] = syJudge
		c.Switchyard = &SwitchyardConfig{Algorithm: json.RawMessage(judgedSpec)}
		c.Providers = append(c.Providers, ProviderConfig{
			Name: syJudge, LiteLLMModel: "openai/judge", BaseURL: judgeSrv.URL, APIKeyFile: c.SATokenFile,
			CostPerInputToken: 0.000002, CostPerOutputToken: 0.000004,
		})
	})
	if code := serve(r, chatBody(t, []Message{{Role: "user", Content: "rename a variable"}})).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	want := 1000*0.000002 + 20*0.000004
	if got := r.SpendUSD(); got < want-1e-9 {
		t.Errorf("spend = %v, want at least the judge's %v", got, want)
	}
}

func TestSwitchyard_KillSwitchAndMissingConfigRouteWithTheRuleRouter(t *testing.T) {
	t.Run("kill switch", func(t *testing.T) {
		t.Setenv(envDisableSwitchyard, "true")
		r, _ := syRouter(t, nil)
		if _, ok := r.selector.(*legacySelector); !ok {
			t.Errorf("selector is %T, want the legacy selector while %s=true", r.selector, envDisableSwitchyard)
		}
		if code := serve(r, chatBody(t, toolRounds(5, "Bash", failingBashOutput))).Code; code != http.StatusOK {
			t.Errorf("status = %d: the rule router must still serve", code)
		}
	})
	t.Run("no algorithm configured", func(t *testing.T) {
		r, _ := syRouter(t, func(c *Config) { c.Switchyard = nil })
		if _, ok := r.selector.(*legacySelector); !ok {
			t.Errorf("selector is %T, want the legacy selector without an algorithm", r.selector)
		}
	})
	t.Run("other strategies never load Switchyard", func(t *testing.T) {
		r, _ := syRouter(t, func(c *Config) { c.Strategy = "rule-based" })
		if _, ok := r.selector.(*legacySelector); !ok {
			t.Errorf("selector is %T for a rule-based selector", r.selector)
		}
	})
}

type recordingSessionSelector struct {
	stubSelector
	sessions []string
}

func (s *recordingSessionSelector) NewSession(id string) { s.sessions = append(s.sessions, id) }

// ClaimRun is called once per router in these tests: it restarts the cancel
// watcher, and a second claim races with the first one's watcher on cancelCtx
// (a flaw of ClaimRun itself, unrelated to selection).
func claimTestRouter(t *testing.T) (*Router, *recordingSessionSelector) {
	t.Helper()
	r := newSelectionTestRouter(t, func(c *Config) { c.RunName = "" })
	rec := &recordingSessionSelector{}
	r.selector = rec
	return r, rec
}

func TestClaimRun_StartsTheSelectorsNewSessionAndTakesTheRunsIdentity(t *testing.T) {
	r, rec := claimTestRouter(t)
	r.ClaimRun(WarmRunInput{RunName: "run-2", SessionID: "chat-7", ParentRunName: "parent", WorkflowStep: "review"})
	if !reflect.DeepEqual(rec.sessions, []string{"chat-7"}) {
		t.Errorf("sessions = %v, want the claimed run's session", rec.sessions)
	}
	if r.cfg.SessionID != "chat-7" || r.cfg.ParentRunName != "parent" || r.cfg.WorkflowStep != "review" {
		t.Errorf("identity = %q/%q/%q", r.cfg.SessionID, r.cfg.ParentRunName, r.cfg.WorkflowStep)
	}
}

func TestClaimRun_WithoutASessionTheRunIsItsOwn(t *testing.T) {
	r, rec := claimTestRouter(t)
	r.ClaimRun(WarmRunInput{RunName: "run-3"}) // an older operator sends no session
	if r.cfg.SessionID != "run-3" || r.cfg.ParentRunName != "" || !reflect.DeepEqual(rec.sessions, []string{"run-3"}) {
		t.Errorf("session %q parent %q sessions %v", r.cfg.SessionID, r.cfg.ParentRunName, rec.sessions)
	}
}

// BenchmarkSwitchyardSelect measures one decision over a tool-heavy conversation
// of the given length. The budget is a few milliseconds at 200 messages: a
// decision runs before every LLM call.
func BenchmarkSwitchyardSelect(b *testing.B) {
	for _, rounds := range []int{5, 100, 250} { // 2 messages per round + the task
		cfg := switchyardTestConfig(stageRouterSpec)
		legacy := newLegacySelector(cfg, NewRuleRouter(cfg), nil)
		s := newSwitchyardSelector(cfg, legacy, noJudge)
		if s.initErr != nil {
			b.Fatal(s.initErr)
		}
		msgs := toolRounds(rounds, "Edit", cleanEditOutput)
		b.Run(fmt.Sprintf("messages=%d", len(msgs)), func(b *testing.B) {
			b.ReportAllocs()
			in := SelectionInput{Messages: msgs, Session: SessionInfo{ID: "bench"}}
			for b.Loop() {
				if _, err := s.Select(context.Background(), in); err != nil {
					b.Fatal(err)
				}
			}
		})
		_ = s.Close()
	}
}

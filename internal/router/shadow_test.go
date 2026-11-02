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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShadowSampled_IsDeterministicAndHonoursTheRate(t *testing.T) {
	if !shadowSampled("x", 1) || shadowSampled("x", 0) || shadowSampled("", 0.5) {
		t.Fatal("rate 1 samples everything; rate 0 and an empty ID sample nothing")
	}
	in := 0
	for i := range 2000 {
		id := fmt.Sprintf("decision-%d", i)
		got := shadowSampled(id, 0.25)
		if got != shadowSampled(id, 0.25) {
			t.Fatalf("%s: the sample changed between calls", id)
		}
		if got {
			in++
		}
	}
	if in < 400 || in > 600 {
		t.Errorf("%d of 2000 sampled at rate 0.25, want about 500", in)
	}
}

func TestParseVerdict(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		wantErr          bool
	}{
		{"plain", `{"winner":"A","confidence":0.9,"reason":"r"}`, verdictA, false},
		{"fenced", "```json\n{\"winner\":\"b\",\"confidence\":0.4}\n```", verdictB, false},
		{"tie", `Verdict: {"winner":"TIE"}`, verdictTie, false},
		{"unknown winner", `{"winner":"C"}`, "", true},
		{"no json", `A is better`, "", true},
		{"broken json", `{"winner":`, "", true},
	} {
		v, err := parseVerdict(tc.text)
		if (err != nil) != tc.wantErr || v.Winner != tc.want {
			t.Errorf("%s: winner %q err %v, want %q err=%v", tc.name, v.Winner, err, tc.want, tc.wantErr)
		}
	}
	if v, _ := parseVerdict(`{"winner":"A","confidence":7}`); v.Confidence != 1 {
		t.Errorf("confidence = %v, want it clamped to 1", v.Confidence)
	}
}

func TestServedOrShadow(t *testing.T) {
	for _, tc := range []struct {
		v       string
		swapped bool
		want    string
	}{
		{verdictA, false, winnerServed}, {verdictB, false, winnerShadow},
		{verdictA, true, winnerShadow}, {verdictB, true, winnerServed},
		{verdictTie, false, winnerTie}, {verdictTie, true, winnerTie},
	} {
		if got := servedOrShadow(tc.v, tc.swapped); got != tc.want {
			t.Errorf("servedOrShadow(%s, %v) = %s, want %s", tc.v, tc.swapped, got, tc.want)
		}
	}
}

// --- through the real handler ------------------------------------------------

// shadowWorld is a served provider, a shadow provider, a judge and an operator
// that grants or denies leases, wired into one router.
type shadowWorld struct {
	r         *Router
	judgeBody func(call int) string
	shadowTC  bool // the shadow provider answers with a tool call
	grant     atomic.Bool

	mu       sync.Mutex
	results  []ShadowResultEnvelope
	leases   int
	judged   atomic.Int32
	shadowed atomic.Int32
}

func completion(content string, tool bool) string {
	msg := map[string]any{"role": "assistant", "content": content}
	if tool {
		msg["tool_calls"] = []map[string]any{{"id": "c1", "type": "function",
			"function": map[string]any{"name": "delete_everything", "arguments": `{}`}}}
	}
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 20},
	})
	return string(b)
}

func newShadowWorld(t *testing.T, mutate func(*ShadowConfig)) *shadowWorld {
	t.Helper()
	w := &shadowWorld{judgeBody: func(int) string { return `{"winner":"A","confidence":0.8,"reason":"served is fine"}` }}
	w.grant.Store(true)

	op := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		switch {
		case strings.HasSuffix(req.URL.Path, "/shadow-lease"):
			if !w.grant.Load() {
				http.Error(rw, "exhausted", http.StatusTooManyRequests)
				return
			}
			w.mu.Lock()
			w.leases++
			w.mu.Unlock()
			_, _ = rw.Write([]byte(`{"leaseId":5}`))
		case strings.HasSuffix(req.URL.Path, "/shadow-results"):
			var env ShadowResultEnvelope
			_ = json.Unmarshal(body, &env)
			w.mu.Lock()
			w.results = append(w.results, env)
			w.mu.Unlock()
			rw.WriteHeader(http.StatusNoContent)
		default:
			rw.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(op.Close)

	shadow := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		w.shadowed.Add(1)
		_, _ = rw.Write([]byte(completion("shadow answer", w.shadowTC)))
	}))
	t.Cleanup(shadow.Close)
	judge := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		call := int(w.judged.Add(1))
		_, _ = rw.Write([]byte(completion(w.judgeBody(call), false)))
	}))
	t.Cleanup(judge.Close)
	served, _ := fakeProvider(t, usageTurnSSE(100, 20))

	sc := &ShadowConfig{
		Benchmark: "bench", SampleRate: 1, Arms: "counterfactual", MaxPerRun: 3,
		IncludeToolTurns: true, Judge: "judge", PositionSwap: true, JudgeMaxTokens: 64,
	}
	if mutate != nil {
		mutate(sc)
	}
	w.r = newSelectionTestRouter(t, func(c *Config) {
		c.OperatorAPIURL = op.URL
		c.Strategy = strategyRuleBased
		c.Providers = c.Providers[:1]
		c.Providers[0].BaseURL = served.URL
		c.Providers[0].CostPerInputToken, c.Providers[0].CostPerOutputToken = 0.000002, 0.00001
		c.Providers = append(c.Providers, ProviderConfig{
			Name: "b", LiteLLMModel: "openai/b", APIKeyFile: c.Providers[0].APIKeyFile, BaseURL: shadow.URL, Weight: 0,
			CostPerInputToken: 0.0000001, CostPerOutputToken: 0.0000005,
		})
		c.AuxProviders = []ProviderConfig{{Name: "judge", LiteLLMModel: "openai/judge", APIKeyFile: c.Providers[0].APIKeyFile, BaseURL: judge.URL, CostPerInputToken: 0.000001, CostPerOutputToken: 0.000004}}
		c.Shadow = sc
	})
	return w
}

// run serves one turn and waits for the replay to finish.
func (w *shadowWorld) run(t *testing.T) {
	t.Helper()
	if code := serve(w.r, conversation(t, 2)).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	w.r.shadow.wait(10 * time.Second)
}

func (w *shadowWorld) only(t *testing.T) ShadowResultEnvelope {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.results) != 1 {
		t.Fatalf("got %d shadow results, want 1: %+v", len(w.results), w.results)
	}
	return w.results[0]
}

func TestShadow_ReplaysJudgesAndFilesTheResult(t *testing.T) {
	w := newShadowWorld(t, nil)
	spendBefore := w.r.spendUSD
	w.run(t)

	env := w.only(t)
	res := env.Result
	if env.LeaseID != 5 || res.Benchmark != "bench" || res.ServedProvider != "a" || res.ShadowProvider != "b" || res.JudgeProvider != "judge" {
		t.Errorf("envelope = %+v", env)
	}
	if res.DecisionID == "" || res.Stage == "" {
		t.Errorf("the result does not carry the served call's identity: %+v", res)
	}
	// Judge A both times, but the second ordering has the served answer as B, so
	// the orderings disagree: a tie, marked inconsistent.
	if res.Winner != winnerTie || !res.Inconsistent {
		t.Errorf("winner = %s inconsistent = %v, want an inconsistent tie", res.Winner, res.Inconsistent)
	}
	if w.judged.Load() != 2 {
		t.Errorf("judge called %d times, want 2 (position swap)", w.judged.Load())
	}
	if res.ShadowPromptTokens != 100 || res.ShadowCostUSD <= 0 || res.JudgeCostUSD <= 0 {
		t.Errorf("costs not recorded: %+v", res)
	}
	if res.Transcript != nil {
		t.Error("a transcript was sent although storeTranscripts is off")
	}
	// Benchmark spend is not the run's spend: the budget must not see it.
	if w.r.spendUSD != spendBefore+usageCost(TokenUsage{PromptTokens: 100, CompletionTokens: 20}, &w.r.cfg.Providers[0]) {
		t.Errorf("run spend = %v: the shadow or judge call was charged to the run", w.r.spendUSD)
	}
}

func TestShadow_ConsistentVerdictsWin(t *testing.T) {
	w := newShadowWorld(t, nil)
	// First ordering: served is A and wins. Second: served is B, so a "B" verdict
	// means served won again.
	w.judgeBody = func(call int) string {
		if call == 1 {
			return `{"winner":"A","confidence":0.9}`
		}
		return `{"winner":"B","confidence":0.7}`
	}
	w.run(t)
	res := w.only(t).Result
	if res.Winner != winnerServed || res.Inconsistent {
		t.Errorf("winner = %s inconsistent = %v, want served", res.Winner, res.Inconsistent)
	}
	if res.JudgeConfidence < 0.79 || res.JudgeConfidence > 0.81 {
		t.Errorf("confidence = %v, want the mean 0.8", res.JudgeConfidence)
	}
}

func TestShadow_InvalidJudgeReplyIsRecordedInvalid(t *testing.T) {
	w := newShadowWorld(t, nil)
	w.judgeBody = func(int) string { return "I prefer the first one." }
	w.run(t)
	if res := w.only(t).Result; res.Winner != winnerInvalid {
		t.Errorf("winner = %s, want invalid", res.Winner)
	}
}

func TestShadow_NeverDispatchesTheToolsTheShadowAsksFor(t *testing.T) {
	w := newShadowWorld(t, nil)
	w.shadowTC = true
	w.run(t)
	res := w.only(t).Result
	if res.ShadowToolCalls != 1 {
		t.Errorf("shadow tool calls = %d, want the one it asked for counted", res.ShadowToolCalls)
	}
	// A dispatched tool would be recorded on the run; none may be.
	for _, m := range w.r.messages {
		if m.Role == "tool" {
			t.Errorf("a tool result reached the conversation: %+v", m)
		}
	}
	if w.shadowed.Load() != 1 {
		t.Errorf("shadow provider called %d times, want 1", w.shadowed.Load())
	}
}

func TestShadow_DeniedLeaseMakesNoCall(t *testing.T) {
	w := newShadowWorld(t, nil)
	w.grant.Store(false)
	w.run(t)
	if w.shadowed.Load() != 0 || w.judged.Load() != 0 {
		t.Errorf("shadow=%d judge=%d calls were made without a lease", w.shadowed.Load(), w.judged.Load())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.results) != 0 {
		t.Errorf("results = %+v", w.results)
	}
}

func TestShadow_RateZeroAndToolTurnExclusion(t *testing.T) {
	w := newShadowWorld(t, func(c *ShadowConfig) { c.SampleRate = 0 })
	if w.r.shadow != nil {
		t.Error("a runner exists for sample rate 0")
	}
	w.run(t)
	if w.shadowed.Load() != 0 {
		t.Error("a call was shadowed at rate 0")
	}

	s := &shadowRunner{cfg: ShadowConfig{SampleRate: 1, IncludeToolTurns: false}, sem: make(chan struct{}, 1)}
	s.consider(servedCall{
		ev:     RoutingEvent{DecisionID: "d"},
		served: &ProviderConfig{Name: "a"},
		answer: Message{ToolCalls: []ToolCall{{ID: "1"}}},
	})
	if s.sampled != 0 {
		t.Error("a tool turn was sampled although includeToolTurns is false")
	}
}

func TestShadow_MaxPerRun(t *testing.T) {
	w := newShadowWorld(t, func(c *ShadowConfig) { c.MaxPerRun = 1; c.PositionSwap = false })
	w.r.shadow.sampled = 1
	w.run(t)
	if w.shadowed.Load() != 0 {
		t.Error("a call was shadowed past maxPerRun")
	}
}

// A deployment keeps running, so it has no per-run cap: the operator limits how
// many samples an Agent gets per hour.
func TestShadow_ZeroMaxPerRunMeansNoLocalCap(t *testing.T) {
	w := newShadowWorld(t, func(c *ShadowConfig) { c.MaxPerRun = 0; c.PositionSwap = false })
	w.r.shadow.sampled = 1000
	w.run(t)
	if w.shadowed.Load() == 0 {
		t.Error("a call was not shadowed although the router has no local cap")
	}
}

func TestShadow_StoreTranscriptsSendsThem(t *testing.T) {
	w := newShadowWorld(t, func(c *ShadowConfig) { c.StoreTranscripts = true; c.PositionSwap = false })
	w.run(t)
	res := w.only(t).Result
	var tr map[string]string
	if err := json.Unmarshal(res.Transcript, &tr); err != nil || tr["shadow"] != "shadow answer" || tr["served"] != "Done." {
		t.Errorf("transcript = %s (%v)", res.Transcript, err)
	}
}

func TestShadow_CounterfactualPicksTheOtherTier(t *testing.T) {
	r := newSelectionTestRouter(t, func(c *Config) {
		c.Providers = []ProviderConfig{
			{Name: "big", CostPerInputToken: 5}, {Name: "mid", CostPerInputToken: 2}, {Name: "small", CostPerInputToken: 1},
		}
		c.CapabilityRouting = map[string]string{"capable": "big", "efficient": "small"}
	})
	s := &shadowRunner{r: r}
	if got := s.counterfactual(&r.cfg.Providers[0]); got == nil || got.Name != "small" {
		t.Errorf("capable served -> %v, want small", got)
	}
	if got := s.counterfactual(&r.cfg.Providers[2]); got == nil || got.Name != "big" {
		t.Errorf("efficient served -> %v, want big", got)
	}
	// No roles: the cheapest other when the served one is not the cheapest.
	r.cfg.CapabilityRouting = nil
	if got := s.counterfactual(&r.cfg.Providers[0]); got == nil || got.Name != "small" {
		t.Errorf("no roles, big served -> %v, want small", got)
	}
	if got := s.counterfactual(&r.cfg.Providers[2]); got == nil || got.Name != "big" {
		t.Errorf("no roles, cheapest served -> %v, want the dearest", got)
	}
}

// The user's stream must not change because a benchmark samples the call.
func TestShadow_StreamIsByteIdenticalToAnUnbenchmarkedRun(t *testing.T) {
	with := newShadowWorld(t, nil)
	without := newShadowWorld(t, func(c *ShadowConfig) { c.SampleRate = 0 })
	a := serve(with.r, conversation(t, 2))
	with.r.shadow.wait(10 * time.Second)
	b := serve(without.r, conversation(t, 2))
	if with.shadowed.Load() != 1 || without.shadowed.Load() != 0 {
		t.Fatalf("shadowed calls: with=%d without=%d, want 1 and 0", with.shadowed.Load(), without.shadowed.Load())
	}
	if a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Errorf("benchmarked response differs:\n%q\n%q", a.Body.String(), b.Body.String())
	}
}

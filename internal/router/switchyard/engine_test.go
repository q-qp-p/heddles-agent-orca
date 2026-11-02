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

package switchyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sy "github.com/NVIDIA-NeMo/Switchyard/bindings/go"
)

// algStageRouter is the type and the reported name of the stage router.
const algStageRouter = "stage_router"

const (
	strong = "strong"
	weak   = "weak"
	judge  = "judge"

	stageSpec = `{"type":"stage_router","picker":"efficient_first","capable_target":"strong",` +
		`"efficient_target":"weak","confidence_threshold":0.5}`
	stageFields = `"capable_target":"strong","efficient_target":"weak","confidence_threshold":0.5`
	// A judge in front of the stage router (upstream's composite).
	compositeSpec = `{"type":"composite","classifier":{"target":"judge","base_threshold":0.5,` +
		`"classify_trigger":"user_turn","fail_open":false},"stage":{` + stageFields + `}}`
	compositeFailOpen = `{"type":"composite","classifier":{"target":"judge","base_threshold":0.5,` +
		`"classify_trigger":"user_turn","fail_open":true},"stage":{` + stageFields + `}}`
	// A typed decision service instead of an LLM judge: the router cannot serve it.
	decisionSpec = `{"type":"composite","classifier":{"target":"judge","classify_trigger":"user_turn","fail_open":true,` +
		`"decision":{"cutoff":0.5,"candidates":{"a":"strong","b":"weak"},"evidence":{}}},"stage":{` + stageFields + `}}`
)

var allTargets = []string{strong, weak, judge}

func groups() map[string][]string {
	return map[string][]string{"any": {strong, weak}, "capable": {strong}, "efficient": {weak}, "judge": {judge}}
}

func noModels(context.Context, string, []byte) ([]byte, error) {
	return nil, errors.New("no model calls expected")
}

func newEngine(t *testing.T, spec string, call ModelCaller) *Engine {
	t.Helper()
	readGuest(t)
	e, err := New(Config{Algorithm: json.RawMessage(spec), Targets: allTargets, Groups: groups()}, call)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(e.Close)
	return e
}

// chatHistory is an OpenAI chat request whose conversation holds `rounds` tool
// calls named tool, each answered with result.
func chatHistory(rounds int, tool, result string) []byte {
	msgs := make([]map[string]any, 1, 1+2*rounds)
	msgs[0] = map[string]any{"role": "user", "content": "Fix the failing test in parser.go"}
	for i := range rounds {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": tool, "arguments": `{"file_path":"parser.go"}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": result})
	}
	b, _ := json.Marshal(map[string]any{"model": "any", "messages": msgs})
	return b
}

const failingBash = "Traceback (most recent call last):\n  File x\nError: command failed with exit code 1\n" +
	"FAILED tests/test_parser.py::test_x"

func route(t *testing.T, e *Engine, body []byte, session string) Result {
	t.Helper()
	r, err := e.Route(context.Background(), body, Metadata{SessionID: session})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	return r
}

func TestStageRouter_RoutesByToolHistory(t *testing.T) {
	e := newEngine(t, stageSpec, noModels)
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"no history takes the picker default", chatHistory(0, "", ""), weak},
		{"clean production stays on the efficient model", chatHistory(5, "Edit", "The file parser.go has been updated."), weak},
		{"repeated failures escalate to the capable model", chatHistory(5, "Bash", failingBash), strong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := route(t, e, tt.body, "s-"+tt.name)
			if got.Selected[0] != tt.want {
				t.Errorf("selected %v, want %s first", got.Selected, tt.want)
			}
			if len(got.Selected) != 2 {
				t.Errorf("selected %v: the other model should follow as a fallback", got.Selected)
			}
		})
	}
}

func TestStageRouter_ResultCarriesTheAlgorithmsEvidence(t *testing.T) {
	e := newEngine(t, stageSpec, noModels)
	got := route(t, e, chatHistory(5, "Edit", "The file parser.go has been updated."), "s")
	var meta struct {
		Algorithm string `json:"algorithm"`
		Evidence  struct {
			Source     string  `json:"source"`
			Confidence float64 `json:"confidence"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(got.Metadata, &meta); err != nil {
		t.Fatalf("metadata %s: %v", got.Metadata, err)
	}
	if meta.Algorithm != algStageRouter || meta.Evidence.Source != "dimensions" || meta.Evidence.Confidence <= 0.5 {
		t.Errorf("metadata = %s", got.Metadata)
	}
}

// Switchyard keeps state per session: after an escalation the capable model is
// held for the next turns even on a clean history. Another session is unaffected,
// which is why sessions must never share an engine's state by accident.
func TestStageRouter_HoldsAreScopedToTheSession(t *testing.T) {
	e := newEngine(t, stageSpec, noModels)
	clean := chatHistory(5, "Edit", "The file parser.go has been updated.")

	if got := route(t, e, chatHistory(5, "Bash", failingBash), "alpha"); got.Selected[0] != strong {
		t.Fatalf("setup: failures selected %v", got.Selected)
	}
	if got := route(t, e, clean, "alpha"); got.Selected[0] != strong {
		t.Errorf("same session after escalation selected %v, want the capable model held", got.Selected)
	}
	if got := route(t, e, clean, "beta"); got.Selected[0] != weak {
		t.Errorf("a different session selected %v, want its own state (efficient)", got.Selected)
	}
}

func verdict(pSolve float64) []byte {
	text := fmt.Sprintf(`{"crux":"bounded task","primary_rule":"SUP-1","capability_boundary":"supported","p_solve":%v}`, pSolve)
	b, _ := json.Marshal(map[string]any{
		"id": "j", "object": "chat.completion", "model": "judge",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": text}}},
		"usage": map[string]any{"prompt_tokens": 40, "completion_tokens": 20},
	})
	return b
}

func TestComposite_JudgeIsServedAsAnOrdinaryModelCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pSolve float64
		want   string
	}{
		{"an easy task goes to the efficient model", 0.95, weak},
		{"a hard task goes to the capable model", 0.05, strong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var provider string
			var body []byte
			e := newEngine(t, compositeSpec, func(_ context.Context, p string, b []byte) ([]byte, error) {
				calls.Add(1)
				provider, body = p, b
				return verdict(tc.pSolve), nil
			})
			got := route(t, e, chatHistory(0, "", ""), "s")
			if got.Selected[0] != tc.want {
				t.Errorf("selected %v, want %s", got.Selected, tc.want)
			}
			if calls.Load() != 1 || provider != judge {
				t.Fatalf("judge calls = %d to %q, want one call to %q", calls.Load(), provider, judge)
			}
			var req struct {
				Model    string `json:"model"`
				Messages []any  `json:"messages"`
			}
			if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
				t.Errorf("judge request is not an OpenAI chat request: %s (%v)", body, err)
			}
		})
	}
}

func TestComposite_JudgeFailurePolicy(t *testing.T) {
	failing := func(context.Context, string, []byte) ([]byte, error) { return nil, errors.New("judge unavailable") }

	closed := newEngine(t, compositeSpec, failing)
	if _, err := closed.Route(context.Background(), chatHistory(0, "", ""), Metadata{SessionID: "s"}); err == nil {
		t.Error("fail_open=false: a failing judge must fail the decision so the caller routes another way")
	}

	open := newEngine(t, compositeFailOpen, failing)
	if got := route(t, open, chatHistory(0, "", ""), "s"); len(got.Selected) == 0 {
		t.Error("fail_open=true: the algorithm should still choose a model")
	}
}

func TestComposite_JudgeGarbageIsAnError(t *testing.T) {
	e := newEngine(t, compositeSpec, func(context.Context, string, []byte) ([]byte, error) { return []byte(`not json`), nil })
	if _, err := e.Route(context.Background(), chatHistory(0, "", ""), Metadata{SessionID: "s"}); err == nil {
		t.Error("an unparseable judge response was accepted")
	}
}

// A decision service is a different protocol the router does not serve. Asking
// for it aborts routing even though the spec says fail_open, so a misconfigured
// selector is loud rather than silently routing without its judge.
func TestComposite_DecisionServiceIsUnsupportedAndAborts(t *testing.T) {
	var calls atomic.Int32
	e := newEngine(t, decisionSpec, func(context.Context, string, []byte) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	})
	_, err := e.Route(context.Background(), chatHistory(0, "", ""), Metadata{SessionID: "s"})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
	if calls.Load() != 0 {
		t.Errorf("a decision call reached the model caller %d times", calls.Load())
	}
}

func TestRoute_DecisionTimeoutBoundsASlowJudge(t *testing.T) {
	readGuest(t)
	e, err := New(Config{
		Algorithm: json.RawMessage(compositeSpec), Targets: allTargets, Groups: groups(),
		DecisionTimeout: 150 * time.Millisecond,
	}, func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
		<-ctx.Done() // a judge that never answers
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	start := time.Now()
	_, err = e.Route(context.Background(), chatHistory(0, "", ""), Metadata{SessionID: "s"})
	if err == nil {
		t.Fatal("a judge that never answers should fail the decision")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v: the decision timeout did not bound the judge", elapsed)
	}
}

func TestRoute_CallerContextCancellationIsHonoured(t *testing.T) {
	e := newEngine(t, stageSpec, noModels)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Route(ctx, chatHistory(0, "", ""), Metadata{}); err == nil {
		t.Error("a cancelled context still routed")
	}
}

func TestRoute_RejectsAnUndecodableRequest(t *testing.T) {
	e := newEngine(t, stageSpec, noModels)
	if _, err := e.Route(context.Background(), []byte(`not json`), Metadata{}); err == nil {
		t.Error("an undecodable request routed")
	}
}

// An algorithm that answers the user itself cannot be relayed through the
// router's streaming path, so a response is an error, not a silent drop.
func TestRoute_AnAlgorithmThatAnswersIsUnsupported(t *testing.T) {
	readGuest(t)
	router, err := sy.New(engineConfig(json.RawMessage(`{"type":"noop"}`), allTargets))
	if err != nil {
		t.Skipf("this Switchyard build has no answering algorithm to test with: %v", err)
	}
	e := &Engine{router: router, cfg: Config{Groups: groups()}, call: noModels, timeout: DefaultDecisionTimeout}
	defer e.Close()
	if _, err := e.Route(context.Background(), chatHistory(0, "", ""), Metadata{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestRoute_ConcurrentRoutingIsSafe(t *testing.T) {
	e := newEngine(t, stageSpec, noModels)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			body := chatHistory(i%4, "Edit", "The file parser.go has been updated.")
			if _, err := e.Route(context.Background(), body, Metadata{SessionID: fmt.Sprintf("s%d", i%3)}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestNew_Requirements(t *testing.T) {
	readGuest(t)
	if _, err := New(Config{Algorithm: json.RawMessage(stageSpec), Targets: allTargets}, nil); err == nil {
		t.Error("New accepted a nil ModelCaller")
	}
	if _, err := New(Config{Algorithm: json.RawMessage(`{"type":"advisor"}`), Targets: allTargets}, noModels); !errors.Is(err, ErrUnsupported) {
		t.Errorf("New with an unsupported algorithm = %v, want ErrUnsupported", err)
	}
}

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

package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type sseResult struct {
	status       int
	content      string
	finish       string
	prompt, comp int
	toolName     string
	sawDone      bool
}

func post(t *testing.T, srv *httptest.Server, body string) sseResult {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := sseResult{status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		return out
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			out.sawDone = true
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt int `json:"prompt_tokens"`
				Comp   int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad frame %q: %v", data, err)
		}
		for _, ch := range c.Choices {
			out.content += ch.Delta.Content
			for _, tc := range ch.Delta.ToolCalls {
				out.toolName = tc.Function.Name
			}
			if ch.FinishReason != nil {
				out.finish = *ch.FinishReason
			}
		}
		if c.Usage != nil {
			out.prompt, out.comp = c.Usage.Prompt, c.Usage.Comp
		}
	}
	return out
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newServer().handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestFakeLLM_StreamsATextTurnWithUsage(t *testing.T) {
	srv := newTestServer(t)
	got := post(t, srv, `{"model":"strong","messages":[{"role":"user","content":"hello there, a long enough prompt"}]}`)
	if got.status != http.StatusOK || got.content != "ok from strong" || got.finish != "stop" || !got.sawDone {
		t.Errorf("result = %+v", got)
	}
	if got.prompt < 1 || got.comp < 1 {
		t.Errorf("usage = %d/%d, want non-zero so costs are non-zero", got.prompt, got.comp)
	}
}

func TestFakeLLM_UsageGrowsWithTheConversation(t *testing.T) {
	srv := newTestServer(t)
	short := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	long := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("word ", 400)+`"}]}`)
	if long.prompt <= short.prompt {
		t.Errorf("prompt tokens %d (long) <= %d (short)", long.prompt, short.prompt)
	}
}

func TestFakeLLM_ScriptedFailures(t *testing.T) {
	srv := newTestServer(t)
	failures := map[string]int{"weak-500": http.StatusInternalServerError, "weak-429": http.StatusTooManyRequests}
	for model, want := range failures {
		if got := post(t, srv, `{"model":"`+model+`","messages":[]}`); got.status != want {
			t.Errorf("%s: status = %d, want %d", model, got.status, want)
		}
	}
}

func TestFakeLLM_SlowModelDelaysTheFirstToken(t *testing.T) {
	srv := newTestServer(t)
	start := time.Now()
	post(t, srv, `{"model":"weak-slow","messages":[{"role":"user","content":"x"}]}`)
	if elapsed := time.Since(start); elapsed < slowFirstToken {
		t.Errorf("answered in %v, want at least %v", elapsed, slowFirstToken)
	}
}

func TestFakeLLM_ToolModelAsksForAToolUntilItHasAResult(t *testing.T) {
	srv := newTestServer(t)
	tools := `"tools":[{"type":"function","function":{"name":"_write_state"}}]`
	first := post(t, srv, `{"model":"agent-tool","messages":[{"role":"user","content":"go"}],`+tools+`}`)
	if first.toolName != "_write_state" || first.content != "" {
		t.Errorf("first turn = %+v, want a _write_state tool call", first)
	}
	withResult := `{"model":"agent-tool","messages":[{"role":"user","content":"go"},` +
		`{"role":"tool","content":"ok","tool_call_id":"call_fake"}],`
	second := post(t, srv, withResult+tools+`}`)
	if second.toolName != "" || second.content != "ok from agent-tool" {
		t.Errorf("second turn = %+v, want the final text", second)
	}
}

func TestFakeLLM_JudgeModelAnswersAVerdict(t *testing.T) {
	srv := newTestServer(t)
	got := post(t, srv, `{"model":"panel-judge","messages":[{"role":"user","content":"compare"}]}`)
	var verdict struct {
		Winner     string  `json:"winner"`
		Confidence float64 `json:"confidence"`
	}
	err := json.Unmarshal([]byte(got.content), &verdict)
	if err != nil || verdict.Winner != "A" || verdict.Confidence != 0.8 {
		t.Errorf("content %q does not parse as the scripted verdict: %v", got.content, err)
	}
}

func TestFakeLLM_StatsCountPerModelAndReset(t *testing.T) {
	srv := newTestServer(t)
	for _, m := range []string{"strong", "weak", "weak"} {
		post(t, srv, `{"model":"`+m+`","messages":[]}`)
	}
	resp, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	var stats map[string]int
	_ = json.NewDecoder(resp.Body).Decode(&stats)
	_ = resp.Body.Close()
	if stats["strong"] != 1 || stats["weak"] != 2 {
		t.Errorf("stats = %v", stats)
	}

	if r, err := http.Post(srv.URL+"/stats/reset", "", nil); err != nil || r.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %v, %v", r, err)
	}
	resp, _ = http.Get(srv.URL + "/stats")
	stats = nil
	_ = json.NewDecoder(resp.Body).Decode(&stats)
	_ = resp.Body.Close()
	if len(stats) != 0 {
		t.Errorf("stats after reset = %v", stats)
	}
}

func TestFakeLLM_RejectsBadRequests(t *testing.T) {
	srv := newTestServer(t)
	if got := post(t, srv, `not json`); got.status != http.StatusBadRequest {
		t.Errorf("bad json: status = %d", got.status)
	}
	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", resp.StatusCode)
	}
}

func TestFakeLLM_IgnoresAProviderPrefixOnTheModel(t *testing.T) {
	srv := newTestServer(t)
	if got := post(t, srv, `{"model":"openai/strong","messages":[]}`); got.content != "ok from strong" {
		t.Errorf("content = %q, want the prefix dropped", got.content)
	}
	if got := post(t, srv, `{"model":"openai/weak-500","messages":[]}`); got.status != http.StatusInternalServerError {
		t.Errorf("prefixed scripted failure: status = %d, want 500", got.status)
	}
}

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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const chatToolsRejected = `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-6-luna in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","type":"invalid_request_error"}}`

func TestToResponsesInput(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "be terse"},
		{Role: "developer", Content: "use tools"},
		{Role: "user", Content: "list files"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "Bash", Arguments: `{"command":"ls"}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "a.go\nb.go"},
		{Role: "assistant", Content: "two files"},
		{Role: "user", Content: []map[string]any{
			{"type": "text", "text": "and this?"},
			{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
		}},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_2", Function: FunctionCall{Name: "Noop"}}}},
	}
	instructions, input := toResponsesInput(msgs)
	if instructions != "be terse\n\nuse tools" {
		t.Errorf("instructions = %q", instructions)
	}
	got, _ := json.Marshal(input)
	want := `[{"content":"list files","role":"user"},` +
		`{"arguments":"{\"command\":\"ls\"}","call_id":"call_1","name":"Bash","type":"function_call"},` +
		`{"call_id":"call_1","output":"a.go\nb.go","type":"function_call_output"},` +
		`{"content":"two files","role":"assistant"},` +
		`{"content":[{"text":"and this?","type":"input_text"},{"image_url":"data:image/png;base64,AAAA","type":"input_image"}],"role":"user"},` +
		`{"arguments":"{}","call_id":"call_2","name":"Noop","type":"function_call"}]`
	if string(got) != want {
		t.Errorf("input =\n%s\nwant\n%s", got, want)
	}
}

func TestToResponsesTools(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "Bash", "description": "run", "parameters": map[string]any{"type": "object"}}},
		{"type": "web_search"},
	}
	got, _ := json.Marshal(toResponsesTools(tools))
	want := `[{"description":"run","name":"Bash","parameters":{"type":"object"},"strict":false,"type":"function"},{"type":"web_search"}]`
	if string(got) != want {
		t.Errorf("tools = %s\nwant %s", got, want)
	}
}

func TestResponsesEndpoint(t *testing.T) {
	cases := []struct {
		p    ProviderConfig
		want string
	}{
		{ProviderConfig{LiteLLMModel: "openai/gpt-6-luna"}, "https://api.openai.com/v1/responses"},
		{ProviderConfig{LiteLLMModel: "x", BaseURL: "http://p/v1"}, "http://p/v1/responses"},
		{ProviderConfig{LiteLLMModel: "x", BaseURL: "http://p/v1/chat/completions"}, "http://p/v1/responses"},
		{ProviderConfig{LiteLLMModel: "x", BaseURL: "http://p/"}, "http://p/v1/responses"},
		{ProviderConfig{LiteLLMModel: "gemini/gemini-3.8-flash"}, ""},
	}
	for _, c := range cases {
		if got := responsesEndpoint(&c.p); got != c.want {
			t.Errorf("%+v → %q, want %q", c.p, got, c.want)
		}
	}
}

// translated runs events through the translator and returns the decoded chunks.
func translated(t *testing.T, events ...string) (chunks []streamingChatCompletionChunk, done bool) {
	t.Helper()
	var in, out bytes.Buffer
	for _, e := range events {
		in.WriteString("data: " + e + "\n\n")
	}
	translateResponsesSSE(&in, &out)
	for line := range strings.SplitSeq(out.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == sseDoneData {
			done = true
			continue
		}
		var c streamingChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		chunks = append(chunks, c)
	}
	return chunks, done
}

func TestTranslateResponsesSSE_Text(t *testing.T) {
	chunks, done := translated(t,
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","delta":"po"}`,
		`{"type":"response.output_text.delta","delta":"ng"}`,
		`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":120,"output_tokens":4,"total_tokens":124}}}`,
	)
	if !done {
		t.Error("stream not terminated with [DONE]")
	}
	var text strings.Builder
	for _, c := range chunks {
		text.WriteString(c.Choices[0].Delta.Content)
	}
	last := chunks[len(chunks)-1]
	if text.String() != "pong" || chunks[0].ID != "resp_1" || chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Errorf("text = %q, first = %+v", text.String(), chunks[0])
	}
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != finishReasonStop {
		t.Errorf("finish = %v, want stop", last.Choices[0].FinishReason)
	}
	if last.Usage == nil || *last.Usage != (TokenUsage{PromptTokens: 120, CompletionTokens: 4, TotalTokens: 124}) {
		t.Errorf("usage = %+v", last.Usage)
	}
}

func TestTranslateResponsesSSE_ToolCalls(t *testing.T) {
	chunks, _ := translated(t,
		`{"type":"response.created","response":{"id":"resp_2"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_a","name":"Bash"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"command\""}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":":\"ls\"}"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_b","name":"Read"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{}"}`,
		`{"type":"response.completed","response":{"id":"resp_2"}}`,
	)
	var calls []ToolCall
	for _, c := range chunks {
		calls = mergeToolCallDeltas(calls, c.Choices[0].Delta.ToolCalls)
	}
	if len(calls) != 2 ||
		calls[0].ID != "call_a" || calls[0].Function.Name != "Bash" || calls[0].Function.Arguments != `{"command":"ls"}` ||
		calls[1].ID != "call_b" || calls[1].Function.Name != "Read" || calls[1].Function.Arguments != "{}" {
		t.Errorf("merged tool calls = %+v", calls)
	}
	if f := chunks[len(chunks)-1].Choices[0].FinishReason; f == nil || *f != finishReasonToolCalls {
		t.Errorf("finish = %v, want tool_calls", f)
	}
}

func TestTranslateResponsesSSE_IncompleteIsLength(t *testing.T) {
	chunks, done := translated(t,
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.output_text.delta","delta":"cut"}`,
		`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`,
	)
	if f := chunks[len(chunks)-1].Choices[0].FinishReason; !done || f == nil || *f != finishReasonLength {
		t.Errorf("done = %v, finish = %v, want length", done, f)
	}
}

func TestTranslateResponsesSSE_FailureEndsWithoutDone(t *testing.T) {
	_, done := translated(t,
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.failed","response":{"error":{"message":"boom"}}}`,
	)
	if done {
		t.Error("a failed response must not look complete")
	}
}

// fakeOpenAI serves chat completions that reject tools and a Responses endpoint.
type fakeOpenAI struct {
	*httptest.Server
	chat, responses atomic.Int32
	lastBody        atomic.Value // []byte of the last /v1/responses request
	chatStatus      int
	chatBody        string
}

func newFakeOpenAI(t *testing.T, chatStatus int, chatBody string) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{chatStatus: chatStatus, chatBody: chatBody}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		f.chat.Add(1)
		w.WriteHeader(f.chatStatus)
		_, _ = io.WriteString(w, f.chatBody)
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		f.responses.Add(1)
		b, _ := io.ReadAll(r.Body)
		f.lastBody.Store(b)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []string{
			`{"type":"response.created","response":{"id":"resp_9"}}`,
			`{"type":"response.output_text.delta","delta":"pong"}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":50,"output_tokens":2,"total_tokens":52}}}`,
		} {
			_, _ = io.WriteString(w, "data: "+e+"\n\n")
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func openAITestRouter(t *testing.T, baseURL string) (*Router, *ProviderConfig) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte("sk-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := ProviderConfig{Name: "openai-gpt-6-luna", LiteLLMModel: "gpt-6-luna", APIKeyFile: keyFile, BaseURL: baseURL + "/v1"}
	r := &Router{cfg: &Config{Providers: []ProviderConfig{p}, LLMRequestTimeout: time.Minute}, cancelCtx: ctx}
	return r, &r.cfg.Providers[0]
}

func toolRequest() ChatCompletionRequest {
	return ChatCompletionRequest{
		Messages: []Message{{Role: "system", Content: "be terse"}, {Role: "user", Content: "hi"}},
		Tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "Bash", "parameters": map[string]any{"type": "object"}}}},
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The first call hits chat completions, learns that the model needs Responses,
// and answers through it. Later calls skip chat completions entirely.
func TestForwardToProviderStream_SwitchesToResponses(t *testing.T) {
	f := newFakeOpenAI(t, http.StatusBadRequest, chatToolsRejected)
	r, p := openAITestRouter(t, f.URL)

	for call := 1; call <= 2; call++ {
		resp, err := r.forwardToProviderStream(context.Background(), p, toolRequest())
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, `"content":"pong"`) || !strings.Contains(body, sseDoneData) ||
			!strings.Contains(body, `"prompt_tokens":50`) {
			t.Errorf("call %d: not translated to chat chunks:\n%s", call, body)
		}
	}
	if f.chat.Load() != 1 || f.responses.Load() != 2 {
		t.Errorf("chat calls = %d, responses calls = %d, want 1 and 2", f.chat.Load(), f.responses.Load())
	}

	var sent map[string]any
	if err := json.Unmarshal(f.lastBody.Load().([]byte), &sent); err != nil {
		t.Fatal(err)
	}
	tools, _ := sent["tools"].([]any)
	first, _ := tools[0].(map[string]any)
	if sent["instructions"] != "be terse" || sent["store"] != false || sent["stream"] != true ||
		first["name"] != "Bash" || first["strict"] != false || first["function"] != nil {
		t.Errorf("responses request = %v", sent)
	}
}

// Only the "use /v1/responses" 400 switches; any other failure stays an error
// so the router's fallback chain still handles it.
func TestForwardToProviderStream_OtherErrorsDoNotSwitch(t *testing.T) {
	f := newFakeOpenAI(t, http.StatusUnauthorized, `{"error":{"message":"Incorrect API key"}}`)
	r, p := openAITestRouter(t, f.URL)

	if _, err := r.forwardToProviderStream(context.Background(), p, toolRequest()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the 401", err)
	}
	if _, switched := r.responsesOnly.Load(p.Name); switched || f.responses.Load() != 0 {
		t.Errorf("switched = %v, responses calls = %d, want neither", switched, f.responses.Load())
	}
}

func TestForwardToProviderStream_ResponsesErrorSurfaces(t *testing.T) {
	const statusNotRetried = 430
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, chatToolsRejected)
			return
		}
		w.WriteHeader(statusNotRetried)
		_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
	}))
	t.Cleanup(srv.Close)
	r, p := openAITestRouter(t, srv.URL)
	if _, err := r.forwardToProviderStream(context.Background(), p, toolRequest()); err == nil || !strings.Contains(err.Error(), "430") {
		t.Fatalf("err = %v, want the Responses status", err)
	}
}

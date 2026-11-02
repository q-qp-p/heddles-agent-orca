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
	"encoding/json"
	"strings"
	"testing"
)

// usageOf returns the usage the translated stream reports: the last chunk that carries one.
func usageOf(t *testing.T, sse string) TokenUsage {
	t.Helper()
	var out bytes.Buffer
	translateAnthropicSSE(strings.NewReader(sse), &out)
	var got *TokenUsage
	for line := range strings.SplitSeq(out.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == sseDoneData {
			continue
		}
		var chunk streamingChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("translated chunk is not JSON: %v: %s", err, data)
		}
		if chunk.Usage != nil {
			got = chunk.Usage
		}
	}
	if got == nil {
		t.Fatalf("no usage in the translated stream:\n%s", out.String())
	}
	return *got
}

func anthropicStream(start, delta string) string {
	return "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":" + start + "}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":" + delta + "}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
}

func TestAnthropicStreamUsage(t *testing.T) {
	cases := []struct {
		name         string
		start, delta string
		want         TokenUsage
	}{
		{
			name:  "input comes from message_start",
			start: `{"input_tokens":1500,"output_tokens":1}`,
			delta: `{"output_tokens":42}`,
			want:  TokenUsage{PromptTokens: 1500, CompletionTokens: 42, TotalTokens: 1542},
		},
		{
			name:  "cache writes and reads count toward the prompt",
			start: `{"input_tokens":20,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"output_tokens":1}`,
			delta: `{"output_tokens":10}`,
			want:  TokenUsage{PromptTokens: 4320, CompletionTokens: 10, TotalTokens: 4330},
		},
		{
			name:  "a repeated total in message_delta is not added twice",
			start: `{"input_tokens":100,"output_tokens":1}`,
			delta: `{"input_tokens":100,"output_tokens":7}`,
			want:  TokenUsage{PromptTokens: 100, CompletionTokens: 7, TotalTokens: 107},
		},
		{
			name:  "message_delta alone still reports input when message_start has none",
			start: `{}`,
			delta: `{"input_tokens":55,"output_tokens":3}`,
			want:  TokenUsage{PromptTokens: 55, CompletionTokens: 3, TotalTokens: 58},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageOf(t, anthropicStream(tc.start, tc.delta)); got != tc.want {
				t.Errorf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAnthropicToOpenAI_UsageCountsCache(t *testing.T) {
	body := `{"id":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"cache_creation_input_tokens":5,"cache_read_input_tokens":85,"output_tokens":4}}`
	out, err := anthropicToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var resp ChatCompletionResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	want := TokenUsage{PromptTokens: 100, CompletionTokens: 4, TotalTokens: 104}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

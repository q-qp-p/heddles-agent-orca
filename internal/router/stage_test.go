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

import "testing"

func TestClassifyStage(t *testing.T) {
	tests := []struct {
		name        string
		messages    []Message
		wantStage   string
		wantMinConf float64
	}{
		{
			name: "tool result present → tool-exec",
			messages: []Message{
				{Role: "user", Content: "run the tests"},
				{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{ID: "t1"}}},
				{Role: "tool", ToolCallID: "t1", Content: "PASS"},
			},
			wantStage:   stageToolExec,
			wantMinConf: stageConfToolExec,
		},
		{
			name: "assistant message with code fence → code",
			messages: []Message{
				{Role: "user", Content: "go"},
				{Role: "assistant", Content: "here: ```go\nfunc f() {}\n```"},
			},
			wantStage:   stageCode,
			wantMinConf: stageConfCode,
		},
		{
			name: "diff marker in assistant message → code",
			messages: []Message{
				{Role: "assistant", Content: "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n-x\n+y"},
			},
			wantStage:   stageCode,
			wantMinConf: stageConfCode,
		},
		{
			name: "code fence in system prompt ignored",
			messages: []Message{
				{Role: "system", Content: "always answer in ``` fenced blocks```"},
				{Role: "user", Content: "what is kubernetes?"},
			},
			wantStage:   stageChat,
			wantMinConf: stageConfChat,
		},
		{
			name: "latest user message asks for summary",
			messages: []Message{
				{Role: "user", Content: "long discussion..."},
				{Role: "assistant", Content: "long answer..."},
				{Role: "user", Content: "summarize this conversation"},
			},
			wantStage:   stageSummarize,
			wantMinConf: stageConfSummarize,
		},
		{
			name: "summary phrase in earlier turn ignored (latest user wins)",
			messages: []Message{
				{Role: "user", Content: "recap the plan"},
				{Role: "assistant", Content: "ok"},
				{Role: "user", Content: "now write the code"},
			},
			wantStage:   stageChat,
			wantMinConf: stageConfChat,
		},
		{
			name:        "plain question → chat",
			messages:    []Message{{Role: "user", Content: "what is kubernetes?"}},
			wantStage:   stageChat,
			wantMinConf: stageConfChat,
		},
		{
			name: "summary phrase only inside system prompt ignored",
			messages: []Message{
				{Role: "system", Content: "if asked to summarize, be concise"},
				{Role: "user", Content: "hello"},
			},
			wantStage:   stageChat,
			wantMinConf: stageConfChat,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyStage(tt.messages)
			if got.stage != tt.wantStage {
				t.Errorf("stage = %q, want %q", got.stage, tt.wantStage)
			}
			if got.confidence < tt.wantMinConf {
				t.Errorf("confidence = %v, want >= %v", got.confidence, tt.wantMinConf)
			}
		})
	}
}

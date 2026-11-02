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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/router"
)

// fakeReply is what the "strong" model answers.
const fakeReply = "ok from strong"

// The e2e suite trusts this server to look like a real provider to the real
// model-router. This test proves it in-process, so a drift in either side is
// caught here rather than in a cluster run.
func routerAgainstFakeLLM(t *testing.T, model string, tools []router.ToolDefinition) *router.Router {
	t.Helper()
	fake := httptest.NewServer(newServer().handler())
	t.Cleanup(fake.Close)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &router.Config{
		RunName: "compat", RunNamespace: "default", LLMRequestTimeout: time.Minute, SATokenFile: key,
		ToolDefinitions: tools,
		Providers: []router.ProviderConfig{{
			Name: "fake", LiteLLMModel: "openai/" + model, BaseURL: fake.URL, APIKeyFile: key, ContextWindow: 100000, Weight: 1,
		}},
	}
	r, err := router.New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return r
}

func chat(r *router.Router) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	body := `{"model":"default","stream":true,"messages":[{"role":"user","content":"say something"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.HandleChatCompletions(rec, req)
	return rec
}

// streamedContent joins the content deltas of an SSE body.
func streamedContent(t *testing.T, body string) string {
	t.Helper()
	var sb strings.Builder
	for line := range strings.SplitSeq(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad frame %q: %v", data, err)
		}
		for _, ch := range c.Choices {
			sb.WriteString(ch.Delta.Content)
		}
	}
	return sb.String()
}

func TestRouterConsumesTheFakeLLMTextTurn(t *testing.T) {
	r := routerAgainstFakeLLM(t, "strong", nil)
	rec := chat(r)
	body := rec.Body.String()
	got := streamedContent(t, body)
	if rec.Code != http.StatusOK || got != fakeReply || !strings.Contains(body, "[DONE]") {
		t.Fatalf("status %d content %q body %q", rec.Code, got, body)
	}
	if r.SpendUSD() != 0 {
		// No pricing is configured, so a non-zero spend would mean the usage chunk was misread.
		t.Errorf("spend = %v with no prices configured", r.SpendUSD())
	}
}

func TestRouterFailsOverFromAScriptedFailure(t *testing.T) {
	r := routerAgainstFakeLLM(t, "weak-500", nil)
	rec := chat(r)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when the only provider answers 500", rec.Code)
	}
}

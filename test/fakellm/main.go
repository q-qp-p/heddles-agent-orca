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

// Command fakellm is a deterministic, OpenAI-compatible chat completions
// server for tests. It never calls a real model: what it answers is scripted by
// the model name in the request, so a test chooses a behaviour by choosing the
// ModelProvider's model.
//
//	<anything>          streams "ok from <model>" with usage, finish_reason stop
//	<name>-500          answers HTTP 500
//	<name>-429          answers HTTP 429
//	<name>-slow         waits 300ms before the first token
//	<name>-tool         asks for the _write_state tool until a tool result arrives
//	<name>-judge        answers a pairwise-judge verdict as JSON text
//
// A provider prefix on the model ("openai/strong", which the router sends when a
// ModelProvider sets baseURL) is ignored: "strong" is the model.
//
// Usage is derived from the request and reply sizes (4 characters per token),
// so costs are non-zero and grow with the conversation. GET /stats returns how
// many requests each model served; POST /stats/reset clears it.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// slowFirstToken is how long a "-slow" model waits before answering.
const slowFirstToken = 300 * time.Millisecond

type server struct {
	mu    sync.Mutex
	stats map[string]int
}

func newServer() *server { return &server{stats: map[string]int{}} }

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/chat/completions", s.chat)
	mux.HandleFunc("/chat/completions", s.chat)
	mux.HandleFunc("/stats", s.getStats)
	mux.HandleFunc("/stats/reset", s.resetStats)
	return mux
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatRequest struct {
	Model    string           `json:"model"`
	Messages []chatMessage    `json:"messages"`
	Tools    []map[string]any `json:"tools"`
}

func (m chatMessage) text() string {
	if s, ok := m.Content.(string); ok {
		return s
	}
	b, _ := json.Marshal(m.Content)
	return string(b)
}

// tokens approximates a token count from text length.
func tokens(n int) int { return max(1, n/4) }

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.Model = req.Model[strings.LastIndex(req.Model, "/")+1:]
	s.mu.Lock()
	s.stats[req.Model]++
	s.mu.Unlock()

	switch {
	case strings.HasSuffix(req.Model, "-500"):
		http.Error(w, "scripted failure", http.StatusInternalServerError)
		return
	case strings.HasSuffix(req.Model, "-429"):
		http.Error(w, "scripted rate limit", http.StatusTooManyRequests)
		return
	}
	if strings.HasSuffix(req.Model, "-slow") {
		time.Sleep(slowFirstToken)
	}

	promptChars := 0
	sawToolResult := false
	for _, m := range req.Messages {
		promptChars += len(m.text())
		if m.Role == "tool" {
			sawToolResult = true
		}
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	frame := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(choice map[string]any) map[string]any {
		return map[string]any{"id": "fake", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{choice}}
	}

	if strings.HasSuffix(req.Model, "-tool") && !sawToolResult && len(req.Tools) > 0 {
		call := map[string]any{
			"index": 0, "id": "call_fake", "type": "function",
			"function": map[string]any{"name": "_write_state", "arguments": `{"scope":"run","key":"k","value":"v"}`},
		}
		frame(chunk(map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{call}}}))
		frame(map[string]any{"id": "fake", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{},
			"usage": map[string]any{"prompt_tokens": tokens(promptChars), "completion_tokens": 8}})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}

	reply := "ok from " + req.Model
	if strings.HasSuffix(req.Model, "-judge") {
		reply = `{"winner":"A","confidence":0.8,` +
			`"dimensions":{"correctness":"A","toolChoice":"tie","completeness":"A"},"reason":"scripted verdict"}`
	}
	frame(chunk(map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}))
	for _, part := range splitReply(reply) {
		frame(chunk(map[string]any{"index": 0, "delta": map[string]any{"content": part}}))
	}
	frame(chunk(map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}))
	frame(map[string]any{"id": "fake", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{},
		"usage": map[string]any{"prompt_tokens": tokens(promptChars), "completion_tokens": tokens(len(reply))}})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

// splitReply cuts a reply into a few chunks so streaming is observable.
func splitReply(s string) []string {
	const size = 12
	var out []string
	for len(s) > size {
		out = append(out, s[:size])
		s = s[size:]
	}
	return append(out, s)
}

func (s *server) getStats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.stats)
}

func (s *server) resetStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.stats = map[string]int{}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	addr := ":" + envOr("PORT", "8080")
	log.Printf("fakellm listening on %s", addr)
	srv := &http.Server{Addr: addr, Handler: newServer().handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

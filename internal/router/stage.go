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

import "strings"

// Task stages produced by classifyStage. Stage 0 uses them as a telemetry and
// category dimension on every routing decision; stage-keyed routing through
// CapabilityRouting ("stage:<name>") arrives with the recommendation engine.
const (
	stageToolExec  = "tool-exec"
	stageCode      = "code"
	stageSummarize = "summarize"
	stageChat      = "chat"
)

// Stage confidences. The classifier is deterministic; the confidence says how
// specific the signal was (a tool result is certain, plain chat is the default).
const (
	stageConfToolExec  = 0.95
	stageConfCode      = 0.85
	stageConfSummarize = 0.80
	stageConfChat      = 0.40
)

// stageResult is the outcome of stage classification.
type stageResult struct {
	// stage is one of the stage* constants.
	stage string
	// confidence is the classification confidence (0.0–1.0).
	confidence float64
}

// classifyStage classifies the current send into a task stage using
// structured message signals only — no LLM calls, no network (#60 F3).
// The stage is recorded on every routing decision so strategies can be compared
// per task category.
//
// Signals, in priority order:
//  1. tool-exec — the conversation contains tool-result messages (role
//     "tool"), meaning the agent is mid tool-execution loop.
//  2. code — recent non-system messages contain code fences or diffs,
//     a strong signal the task is writing/reviewing code.
//  3. summarize — the latest user message asks for a summary/recap.
//  4. chat — everything else (below the routing gate).
//
// System-prompt messages are excluded from every heuristic so agent
// instructions (which routinely contain code examples and words like
// "summarize") cannot trigger false positives.
func classifyStage(messages []Message) stageResult {
	// Signal 1: tool execution — any tool-result message in the send.
	for _, m := range messages {
		if m.Role == "tool" { //nolint:goconst
			return stageResult{stage: stageToolExec, confidence: stageConfToolExec}
		}
	}

	// Signals 2 and 3 examine non-system message text, latest first.
	var lastUserText string
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == "system" {
			continue
		}
		text, ok := m.Content.(string)
		if !ok {
			continue
		}
		if lastUserText == "" && m.Role == "user" { //nolint:goconst
			lastUserText = text
		}
		if containsCodeSignal(text) {
			return stageResult{stage: stageCode, confidence: stageConfCode}
		}
	}

	// Signal 3: summarization request in the latest user message.
	if asksForSummary(lastUserText) {
		return stageResult{stage: stageSummarize, confidence: stageConfSummarize}
	}

	return stageResult{stage: stageChat, confidence: stageConfChat}
}

// containsCodeSignal reports whether text contains a code fence or diff
// marker. Checked against a window of the text to keep the scan cheap on
// very large messages.
func containsCodeSignal(text string) bool {
	const maxScan = 8 * 1024
	if len(text) > maxScan {
		// Scan head and tail; a code fence or diff lives at a message
		// boundary in practice.
		text = text[:maxScan/2] + text[len(text)-maxScan/2:]
	}
	if strings.Contains(text, "```") {
		return true
	}
	return strings.Contains(text, "diff --git")
}

// asksForSummary reports whether text asks for a summary/recap of the
// conversation. Phrase list matches keyword-hint style so operators can
// predict what triggers the summarize stage.
func asksForSummary(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range []string{"summarize", "write a summary", "give me a summary", "summary of", "recap", "tl;dr"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

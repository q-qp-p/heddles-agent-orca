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

package controller

import (
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
)

// Labels that carry a run's conversation session. ACP-created runs use
// session-id; UI-created runs use session. Both name the same thing.
const (
	labelSessionID = "agentorca.io/session-id"
	labelSession   = "agentorca.io/session"
)

// runSessionID returns the conversation session a run belongs to, falling back
// to the run name so a standalone run is its own session.
func runSessionID(run *agentorcav1alpha1.AgentRun) string {
	if id := run.Labels[labelSessionID]; id != "" {
		return id
	}
	if id := run.Labels[labelSession]; id != "" {
		return id
	}
	return run.Name
}

// runDeadline returns when the run's timeout expires, or nil when the run sets
// none. The clock starts at the run's start time, or now for a run that has
// not started.
func runDeadline(run *agentorcav1alpha1.AgentRun, now time.Time) *time.Time {
	if run.Spec.Timeout == nil || run.Spec.Timeout.Duration <= 0 {
		return nil
	}
	start := now
	if run.Status.StartTime != nil {
		start = run.Status.StartTime.Time
	}
	deadline := start.Add(run.Spec.Timeout.Duration)
	return &deadline
}

// applyRunRoutingIdentity records on cfg who the run is, so every routing
// decision can be attributed to a selector, session and workflow step.
func applyRunRoutingIdentity(cfg *router.Config, run *agentorcav1alpha1.AgentRun, selectorName string, now time.Time) {
	cfg.SelectorName = selectorName
	cfg.SessionID = runSessionID(run)
	cfg.ParentRunName = run.Spec.ParentRunRef
	cfg.WorkflowStep = run.Labels[labelWorkflowStep]
	cfg.RunDeadline = runDeadline(run, now)
}

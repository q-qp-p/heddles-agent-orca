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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
)

func TestRunSessionID(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"acp label wins", map[string]string{labelSessionID: "acp-1", labelSession: "ui-1"}, "acp-1"},
		{"ui label", map[string]string{labelSession: "ui-1"}, "ui-1"},
		{"no session: the run is its own session", nil, "run-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Labels: tt.labels}}
			if got := runSessionID(run); got != tt.want {
				t.Errorf("runSessionID = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunDeadline(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	run := &agentorcav1alpha1.AgentRun{}

	if got := runDeadline(run, now); got != nil {
		t.Errorf("run without a timeout got deadline %v, want nil", got)
	}

	run.Spec.Timeout = &metav1.Duration{Duration: 10 * time.Minute}
	if got := runDeadline(run, now); got == nil || !got.Equal(now.Add(10*time.Minute)) {
		t.Errorf("unstarted run deadline = %v, want now+10m", got)
	}

	started := metav1.NewTime(now.Add(-4 * time.Minute))
	run.Status.StartTime = &started
	if got := runDeadline(run, now); got == nil || !got.Equal(now.Add(6*time.Minute)) {
		t.Errorf("started run deadline = %v, want start+10m", got)
	}

	run.Spec.Timeout = &metav1.Duration{}
	if got := runDeadline(run, now); got != nil {
		t.Errorf("zero timeout got deadline %v, want nil", got)
	}
}

func TestApplyRunRoutingIdentity(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "step-run",
			Labels: map[string]string{labelSessionID: "sess-9", labelWorkflowStep: "implement"},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{
			ParentRunRef: "parent-run",
			Timeout:      &metav1.Duration{Duration: time.Hour},
		},
	}
	cfg := &router.Config{}
	applyRunRoutingIdentity(cfg, run, "balanced", now)

	if cfg.SelectorName != "balanced" || cfg.SessionID != "sess-9" || cfg.ParentRunName != "parent-run" || cfg.WorkflowStep != "implement" {
		t.Errorf("identity = %+v", cfg)
	}
	if cfg.RunDeadline == nil || !cfg.RunDeadline.Equal(now.Add(time.Hour)) {
		t.Errorf("RunDeadline = %v, want now+1h", cfg.RunDeadline)
	}
}

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

// Package shellsandbox is the seam between the operator's internal API and the
// code that owns a run's shell sandbox (Agent.spec.shell). The API server
// speaks to the model-router; a Runner creates the sandbox on the OpenShell
// gateway and runs commands in it.
package shellsandbox

import (
	"context"
	"errors"
	"fmt"
)

// Job phases, as reported by Runner.Poll.
const (
	PhaseRunning = "running"
	PhaseExited  = "exited"
	PhaseLost    = "lost"
	PhaseMissing = "missing"
)

// Job is a snapshot of one command.
type Job struct {
	Phase    string
	ExitCode int
	// Output is the tail of the combined stdout and stderr.
	Output string
}

// Runner runs an Agent's commands in the sandbox of one run.
type Runner interface {
	// Start starts the command as job id, creating the run's sandbox on the
	// first call. It returns false, without starting anything, while the
	// sandbox is still provisioning; the caller asks again. Starting an id that
	// already started is a no-op.
	Start(ctx context.Context, namespace, run, id, command, workDir string) (ready bool, err error)
	// Poll reports the job.
	Poll(ctx context.Context, namespace, run, id string) (Job, error)
	// Stop ends the job if it is running.
	Stop(ctx context.Context, namespace, run, id string) error
}

// ErrUnavailable means the operator has no OpenShell gateway.
var ErrUnavailable = errors.New("the operator has no OpenShell gateway configured")

// RejectedError is a request the caller must not repeat unchanged: the run or
// Agent does not allow it. Its message is shown to the agent.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return e.Reason }

// Rejectedf returns a RejectedError.
func Rejectedf(format string, args ...any) error {
	return &RejectedError{Reason: fmt.Sprintf(format, args...)}
}

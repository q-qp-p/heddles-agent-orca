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

package apiserver

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/heddles/agent-orca/internal/shellsandbox"
)

// These handlers are the operator half of the sandbox_exec tool (see
// internal/router/shell_tool.go and internal/shellsandbox): the model-router
// asks to start a command, then long-polls it. A command is a detached job in
// the run's sandbox, so no request outlives the internal API's timeout.
//
// Auth is the run's model-router token, as for the budget endpoints: the
// caller must be a ServiceAccount of the run's namespace.

// shellJobIDPattern is the id the router makes for a command.
var shellJobIDPattern = regexp.MustCompile(`^sh-[0-9a-f]{16}$`)

const (
	// shellRoute is the path segment of the sandbox_exec endpoints.
	shellRoute = "shell"

	// maxShellCommandBytes bounds one command's script.
	maxShellCommandBytes = 64 << 10
	// shellPollInterval is how often a long poll re-reads a running job.
	shellPollInterval = 2 * time.Second
	// maxShellLongPoll keeps one poll well inside the request timeout.
	maxShellLongPoll = 25 * time.Second
)

// SetShellSandboxes wires the sandbox_exec tool. Without a runner the shell
// endpoints answer 501.
func (s *Server) SetShellSandboxes(r shellsandbox.Runner) {
	s.shell = r
}

type shellStartRequest struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	WorkDir string `json:"workDir,omitempty"`
}

type shellJobResponse struct {
	Phase    string `json:"phase"`
	ExitCode int    `json:"exitCode"`
	Output   string `json:"output"`
}

// startShellJob handles POST /agentrun/{ns}/{run}/shell. It answers 201 once
// the job has started and 202 while the sandbox is still provisioning.
func (s *Server) startShellJob(w http.ResponseWriter, r *http.Request, caller, namespace, runName string) {
	if !s.shellReady(w, caller, namespace) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxShellCommandBytes+4096))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var req shellStartRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !shellJobIDPattern.MatchString(req.ID) {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	if req.Command == "" || len(req.Command) > maxShellCommandBytes {
		http.Error(w, "command is required and must be at most 64 KiB", http.StatusBadRequest)
		return
	}
	ready, err := s.shell.Start(r.Context(), namespace, runName, req.ID, req.Command, req.WorkDir)
	if err != nil {
		s.shellFailure(w, "starting a shell command", namespace, runName, err)
		return
	}
	if !ready {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// pollShellJob handles GET /agentrun/{ns}/{run}/shell/{id}?wait=<seconds>. It
// answers 200 with the job once it is no longer running, or 204 when it still is.
func (s *Server) pollShellJob(w http.ResponseWriter, r *http.Request, caller, namespace, runName, id string) {
	if !s.shellReady(w, caller, namespace) {
		return
	}
	if !shellJobIDPattern.MatchString(id) {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	wait := time.Duration(0)
	if v := r.URL.Query().Get("wait"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			wait = min(time.Duration(secs*float64(time.Second)), maxShellLongPoll)
		}
	}
	deadline := time.Now().Add(wait)
	for {
		job, err := s.shell.Poll(r.Context(), namespace, runName, id)
		if err != nil {
			s.shellFailure(w, "polling a shell command", namespace, runName, err)
			return
		}
		if job.Phase != shellsandbox.PhaseRunning {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(shellJobResponse{Phase: job.Phase, ExitCode: job.ExitCode, Output: job.Output})
			return
		}
		if !time.Now().Before(deadline) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(min(shellPollInterval, time.Until(deadline))):
		}
	}
}

// stopShellJob handles POST /agentrun/{ns}/{run}/shell/{id}/stop.
func (s *Server) stopShellJob(w http.ResponseWriter, r *http.Request, caller, namespace, runName, id string) {
	if !s.shellReady(w, caller, namespace) {
		return
	}
	if !shellJobIDPattern.MatchString(id) {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	if err := s.shell.Stop(r.Context(), namespace, runName, id); err != nil {
		s.shellFailure(w, "stopping a shell command", namespace, runName, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// shellReady checks the caller and that the shell is wired. It writes the
// failure response itself.
func (s *Server) shellReady(w http.ResponseWriter, caller, namespace string) bool {
	if !callerInNamespace(caller, namespace) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if s.shell == nil {
		http.Error(w, shellsandbox.ErrUnavailable.Error(), http.StatusNotImplemented)
		return false
	}
	return true
}

func (s *Server) shellFailure(w http.ResponseWriter, what, namespace, runName string, err error) {
	var rejected *shellsandbox.RejectedError
	switch {
	case errors.Is(err, shellsandbox.ErrUnavailable):
		http.Error(w, err.Error(), http.StatusNotImplemented)
	case errors.As(err, &rejected):
		http.Error(w, rejected.Reason, http.StatusConflict)
	default:
		slog.Error(what, "run", runName, "ns", namespace, "err", err)
		http.Error(w, what+" failed: "+err.Error(), http.StatusBadGateway)
	}
}

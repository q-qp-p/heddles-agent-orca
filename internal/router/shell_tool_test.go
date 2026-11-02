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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shellStopSegment is the path segment of the stop endpoint.
const shellStopSegment = "stop"

// fakeShellAPI emulates the operator's /shell endpoints.
type fakeShellAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	started  []shellStartRequest
	stopped  []string
	auth     string
	startsAt int32 // POSTs answered 202 before one is accepted
	posts    atomic.Int32
	polls    atomic.Int32
	// pollsRunning is how many polls answer 204 before the job result.
	pollsRunning int32
	result       string // 200 body for a finished poll
	startStatus  int    // overrides the start answer when non-zero
	startBody    string
}

func newFakeShellAPI(t *testing.T) *fakeShellAPI {
	t.Helper()
	f := &fakeShellAPI{result: `{"phase":"exited","exitCode":0,"output":"hi\n"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("/agentrun/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/agentrun/"), "/")
		if len(parts) < 3 || parts[2] != "shell" {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.auth = r.Header.Get("Authorization")
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && len(parts) == 3:
			body, _ := io.ReadAll(r.Body)
			var req shellStartRequest
			_ = json.Unmarshal(body, &req)
			if f.startStatus != 0 {
				http.Error(w, f.startBody, f.startStatus)
				return
			}
			if f.posts.Add(1) <= f.startsAt {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			f.mu.Lock()
			f.started = append(f.started, req)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && len(parts) == 4:
			if f.polls.Add(1) <= f.pollsRunning {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = w.Write([]byte(f.result))
		case r.Method == http.MethodPost && len(parts) == 5 && parts[4] == shellStopSegment:
			f.mu.Lock()
			f.stopped = append(f.stopped, parts[3])
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newShellTestRouter(t *testing.T, sh *ShellConfig, api *fakeShellAPI) *Router {
	t.Helper()
	url := "http://127.0.0.1:1"
	if api != nil {
		url = api.srv.URL
	}
	r := newFSTestRouter(t, nil, url, fsSATokenFile(t))
	r.cfg.Shell = sh
	return r
}

func shortShellTimings(t *testing.T) {
	t.Helper()
	oldRetry, oldTimeout := shellProvisionRetry, shellProvisionTimeout
	shellProvisionRetry, shellProvisionTimeout = 5*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { shellProvisionRetry, shellProvisionTimeout = oldRetry, oldTimeout })
}

func TestShellToolDefinitions(t *testing.T) {
	if defs := (*ShellConfig)(nil).ToolDefinitions(); defs != nil {
		t.Fatalf("a nil config must define no tool, got %v", defs)
	}
	defs := (&ShellConfig{MaxTimeoutSeconds: 90}).ToolDefinitions()
	if len(defs) != 1 || defs[0].Name != sandboxExecToolName || defs[0].BackendType != backendTypeBuiltin {
		t.Fatalf("unexpected definitions: %+v", defs)
	}
	if !strings.Contains(string(defs[0].Parameters), "at most 90") {
		t.Errorf("the schema should state the configured cap: %s", defs[0].Parameters)
	}
	if !builtinToolAliases[sandboxExecToolName] {
		t.Error("agents must not be able to declare their own sandbox_exec")
	}
}

func TestSandboxExecRunsCommand(t *testing.T) {
	api := newFakeShellAPI(t)
	api.startsAt = 2 // the sandbox is provisioning for two attempts
	api.pollsRunning = 1
	shortShellTimings(t)
	r := newShellTestRouter(t, &ShellConfig{}, api)

	out := r.executeSandboxExec(context.Background(), `{"command":"echo hi","work_dir":"/sandbox/src"}`)

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not JSON: %v: %s", err, out)
	}
	if got["exit_code"] != float64(0) || got["output"] != "hi\n" || got["error"] != nil {
		t.Errorf("unexpected result: %s", out)
	}
	if len(api.started) != 1 {
		t.Fatalf("the command must start exactly once, got %d", len(api.started))
	}
	if s := api.started[0]; s.Command != "echo hi" || s.WorkDir != "/sandbox/src" || !strings.HasPrefix(s.ID, "sh-") {
		t.Errorf("unexpected start request: %+v", s)
	}
	if api.auth != "Bearer tok-123" {
		t.Errorf("the run's token must authenticate the call, got %q", api.auth)
	}
}

func TestSandboxExecReportsNonZeroExit(t *testing.T) {
	api := newFakeShellAPI(t)
	api.result = `{"phase":"exited","exitCode":2,"output":"boom"}`
	r := newShellTestRouter(t, &ShellConfig{}, api)

	out := r.executeSandboxExec(context.Background(), `{"command":"false"}`)
	if !strings.Contains(out, `"exit_code":2`) || !strings.Contains(out, "boom") || strings.Contains(out, `"error"`) {
		t.Errorf("a failing command is a result, not a tool error: %s", out)
	}
}

func TestSandboxExecLostJobIsAnError(t *testing.T) {
	api := newFakeShellAPI(t)
	api.result = `{"phase":"lost","exitCode":0,"output":"Killed"}`
	r := newShellTestRouter(t, &ShellConfig{}, api)

	out := r.executeSandboxExec(context.Background(), `{"command":"x"}`)
	if !strings.Contains(out, "killed from outside") {
		t.Errorf("a lost job must say so: %s", out)
	}
}

func TestSandboxExecTimeoutStopsTheJob(t *testing.T) {
	api := newFakeShellAPI(t)
	api.pollsRunning = 1 << 20 // never finishes
	r := newShellTestRouter(t, &ShellConfig{MaxTimeoutSeconds: 1}, api)

	start := time.Now()
	out := r.executeSandboxExec(context.Background(), `{"command":"sleep 100","timeout_seconds":1}`)

	if time.Since(start) > 5*time.Second {
		t.Errorf("the call must give up near its timeout, took %s", time.Since(start))
	}
	if !strings.Contains(out, `"timed_out":true`) {
		t.Errorf("expected a timeout result, got %s", out)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.stopped) != 1 || api.stopped[0] != api.started[0].ID {
		t.Errorf("the job must be stopped, got stopped=%v", api.stopped)
	}
}

func TestSandboxExecCapsTimeoutAtTheConfiguredMax(t *testing.T) {
	api := newFakeShellAPI(t)
	api.pollsRunning = 1 << 20
	r := newShellTestRouter(t, &ShellConfig{MaxTimeoutSeconds: 1}, api)

	start := time.Now()
	_ = r.executeSandboxExec(context.Background(), `{"command":"sleep 100","timeout_seconds":3600}`)
	if time.Since(start) > 5*time.Second {
		t.Errorf("a call cannot exceed spec.shell.maxTimeoutSeconds, took %s", time.Since(start))
	}
}

func TestSandboxExecErrors(t *testing.T) {
	tests := []struct {
		name    string
		sh      *ShellConfig
		args    string
		status  int
		body    string
		wantErr string
	}{
		{"not enabled", nil, `{"command":"ls"}`, 0, "", "not enabled"},
		{"bad arguments", &ShellConfig{}, `{`, 0, "", "invalid arguments"},
		{"empty command", &ShellConfig{}, `{"command":"  "}`, 0, "", "command is required"},
		{"rejected by the operator", &ShellConfig{}, `{"command":"ls"}`, http.StatusConflict, "the run has ended", "the run has ended"},
		{"no gateway", &ShellConfig{}, `{"command":"ls"}`, http.StatusNotImplemented, "no OpenShell gateway", "HTTP 501"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeShellAPI(t)
			api.startStatus, api.startBody = tc.status, tc.body
			r := newShellTestRouter(t, tc.sh, api)

			out := r.executeSandboxExec(context.Background(), tc.args)
			if !strings.Contains(out, `"error"`) || !strings.Contains(out, tc.wantErr) {
				t.Errorf("want an error containing %q, got %s", tc.wantErr, out)
			}
		})
	}
}

func TestSandboxExecGivesUpOnASandboxThatNeverStarts(t *testing.T) {
	api := newFakeShellAPI(t)
	api.startsAt = 1 << 20
	shortShellTimings(t)
	r := newShellTestRouter(t, &ShellConfig{}, api)

	out := r.executeSandboxExec(context.Background(), `{"command":"ls"}`)
	if !strings.Contains(out, "did not become ready") {
		t.Errorf("expected a provisioning timeout, got %s", out)
	}
}

func TestSandboxExecUnreachableOperator(t *testing.T) {
	r := newShellTestRouter(t, &ShellConfig{}, nil)
	out := r.executeSandboxExec(context.Background(), `{"command":"ls"}`)
	if !strings.Contains(out, "unreachable") {
		t.Errorf("expected an unreachable error, got %s", out)
	}
}

func TestDispatchRunsSandboxExecWithAndWithoutPrefix(t *testing.T) {
	for _, name := range []string{"sandbox_exec", "_sandbox_exec"} {
		t.Run(name, func(t *testing.T) {
			api := newFakeShellAPI(t)
			r := newShellTestRouter(t, &ShellConfig{}, api)
			out := r.dispatchToolCall(context.Background(), ToolCall{
				Function: FunctionCall{Name: name, Arguments: `{"command":"echo hi"}`},
			})
			if !strings.Contains(out, `"exit_code":0`) {
				t.Errorf("the dispatcher must route %q to the shell tool: %s", name, out)
			}
		})
	}
}

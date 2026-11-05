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
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// The sandbox_exec built-in tool runs a shell command in a sandbox the operator
// keeps for the run (Agent.spec.shell). The router never touches the sandbox: it
// asks the operator's internal API, which owns the gateway credentials, creates
// the sandbox on the first call and deletes it with the run. A command is a
// detached job in the sandbox, so the router can poll it across the internal
// API's request timeout; see docs/sandbox-shell.md.
const (
	sandboxExecToolName = "sandbox_exec"

	defaultShellTimeoutSeconds    = 120
	defaultShellMaxTimeoutSeconds = 600

	// shellStopGrace bounds the best-effort stop of a job the router gave up on.
	shellStopGrace = 10 * time.Second
	// shellResultSlack is added to a command's timeout to let the sandbox report
	// the exit of a job that ran right up to its limit.
	shellResultSlack = 15 * time.Second
)

// Timings the tests shorten.
var (
	// shellProvisionTimeout bounds the wait for the sandbox to become ready.
	shellProvisionTimeout = 5 * time.Minute
	// shellProvisionRetry is the pause between start attempts while it is not.
	shellProvisionRetry = 2 * time.Second
)

// ShellConfig, when non-nil on Config, enables the sandbox_exec tool.
// Populated by the operator from Agent.spec.shell.
type ShellConfig struct {
	// MaxTimeoutSeconds caps one command. 0 = defaultShellMaxTimeoutSeconds.
	MaxTimeoutSeconds int `json:"maxTimeoutSeconds,omitempty"`
}

func (c *ShellConfig) maxTimeout() int {
	if c != nil && c.MaxTimeoutSeconds > 0 {
		return c.MaxTimeoutSeconds
	}
	return defaultShellMaxTimeoutSeconds
}

// ToolDefinitions returns the sandbox_exec definition (none when c is nil).
func (c *ShellConfig) ToolDefinitions() []ToolDefinition {
	if c == nil {
		return nil
	}
	return []ToolDefinition{{
		Name: sandboxExecToolName,
		Description: "Run a shell command (sh) in your sandbox: an isolated Linux environment that is created on " +
			"your first call and kept until this run ends, so files you write and tools you install are still there on " +
			"the next call. Use it to build, test, run scripts or inspect things you cannot do with your other tools. " +
			"Network access is limited to hosts the sandbox's policy allows; a blocked host fails the command. " +
			"The result is the exit code and the tail of the combined stdout and stderr. " +
			"A command that outlives its timeout is stopped.",
		Parameters: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The shell command or script to run with sh -c"},
    "work_dir": {"type": "string", "description": "Optional directory to run in; defaults to the sandbox workspace"},
    "timeout_seconds": {"type": "integer", "description": "Optional limit in seconds (default %d, at most %d)"}
  },
  "required": ["command"]
}`, defaultShellTimeoutSeconds, c.maxTimeout())),
		BackendType: backendTypeBuiltin,
	}}
}

// shellStartRequest is the body of POST /agentrun/{ns}/{run}/shell.
type shellStartRequest struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	WorkDir string `json:"workDir,omitempty"`
}

// shellJobResult is the body of a finished GET /agentrun/{ns}/{run}/shell/{id}.
type shellJobResult struct {
	Phase    string `json:"phase"`
	ExitCode int    `json:"exitCode"`
	Output   string `json:"output"`
}

// executeSandboxExec handles the sandbox_exec built-in tool.
func (r *Router) executeSandboxExec(ctx context.Context, args string) string {
	if r.cfg.Shell == nil {
		return `{"error": "this pod was not given a sandbox shell (declared by Agent.spec.shell)"}`
	}
	var p struct {
		Command        string `json:"command"`
		WorkDir        string `json:"work_dir"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf(`{"error": "invalid arguments: %v"}`, err)
	}
	if strings.TrimSpace(p.Command) == "" {
		return `{"error": "command is required"}`
	}
	timeout := p.TimeoutSeconds
	if timeout <= 0 {
		timeout = min(defaultShellTimeoutSeconds, r.cfg.Shell.maxTimeout())
	}
	timeout = min(timeout, r.cfg.Shell.maxTimeout())

	token, err := os.ReadFile(r.cfg.SATokenFile)
	if err != nil {
		return fmt.Sprintf(`{"error": "reading SA token: %v"}`, err)
	}
	c := &shellClient{
		base:  fmt.Sprintf("%s/agentrun/%s/%s/shell", strings.TrimRight(r.cfg.OperatorAPIURL, "/"), r.cfg.RunNamespace, r.cfg.RunName),
		token: strings.TrimSpace(string(token)),
	}
	id, err := newShellJobID()
	if err != nil {
		return fmt.Sprintf(`{"error": "creating a job id: %v"}`, err)
	}

	if msg := c.start(ctx, shellStartRequest{ID: id, Command: p.Command, WorkDir: p.WorkDir}); msg != "" {
		return msg
	}
	res, msg := c.await(ctx, id, time.Duration(timeout)*time.Second)
	if msg != "" {
		return msg
	}
	return res
}

// newShellJobID returns a job id the sandbox accepts (lowercase alphanumerics).
func newShellJobID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sh-" + hex.EncodeToString(b), nil
}

// shellClient talks to the operator's shell endpoints with the run's SA token.
type shellClient struct {
	base  string
	token string
}

func (c *shellClient) do(ctx context.Context, method, url string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, err
}

// start asks the operator to start the job. The operator answers 202 while the
// sandbox is still provisioning, so start repeats until it is accepted, which
// is safe: the job id makes a second start a no-op. It returns a tool error
// result, or "" once the job has started.
func (c *shellClient) start(ctx context.Context, req shellStartRequest) string {
	deadline := time.Now().Add(shellProvisionTimeout)
	for {
		status, body, err := c.do(ctx, http.MethodPost, c.base, req)
		if err != nil {
			return fmt.Sprintf(`{"error": "sandbox service unreachable: %v"}`, err)
		}
		switch status {
		case http.StatusCreated, http.StatusOK:
			return ""
		case http.StatusAccepted:
			// Sandbox provisioning; ask again.
		default:
			return shellError(status, body)
		}
		if time.Now().After(deadline) {
			return `{"error": "the sandbox did not become ready in time"}`
		}
		select {
		case <-ctx.Done():
			return `{"error": "cancelled while waiting for the sandbox"}`
		case <-time.After(shellProvisionRetry):
		}
	}
}

// await long-polls the job until it ends or limit passes, then stops it.
func (c *shellClient) await(ctx context.Context, id string, limit time.Duration) (result, errMsg string) {
	started := time.Now()
	for {
		if time.Since(started) >= limit {
			return c.timedOut(ctx, id, limit)
		}
		status, body, err := c.do(ctx, http.MethodGet, c.base+"/"+id+"?wait=20", nil)
		if err != nil {
			if ctx.Err() != nil {
				c.stop(id)
				return "", `{"error": "cancelled while the command was running; it was stopped"}`
			}
			return "", fmt.Sprintf(`{"error": "sandbox service unreachable: %v"}`, err)
		}
		switch status {
		case http.StatusNoContent:
			continue
		case http.StatusOK:
			var res shellJobResult
			if err := json.Unmarshal(body, &res); err != nil {
				return "", fmt.Sprintf(`{"error": "decoding the command result: %v"}`, err)
			}
			return formatShellResult(res), ""
		default:
			return "", shellError(status, body)
		}
	}
}

// timedOut stops the job and returns what it printed.
func (c *shellClient) timedOut(ctx context.Context, id string, limit time.Duration) (string, string) {
	c.stop(id)
	// The stopped job's last output is still on the sandbox; give the final
	// poll a moment to see it.
	pollCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shellResultSlack)
	defer cancel()
	out := ""
	if status, body, err := c.do(pollCtx, http.MethodGet, c.base+"/"+id, nil); err == nil && status == http.StatusOK {
		var res shellJobResult
		if json.Unmarshal(body, &res) == nil {
			out = res.Output
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"timed_out": true,
		"error":     fmt.Sprintf("the command was stopped after %ds", int(limit.Seconds())),
		"output":    out,
	})
	return string(raw), ""
}

// stop asks the operator to stop the job, best effort and not tied to ctx,
// which may be the cancelled one.
func (c *shellClient) stop(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), shellStopGrace)
	defer cancel()
	_, _, _ = c.do(ctx, http.MethodPost, c.base+"/"+id+"/stop", nil)
}

func formatShellResult(res shellJobResult) string {
	out := map[string]any{"exit_code": res.ExitCode, "output": res.Output}
	switch res.Phase {
	case "exited":
	case "lost":
		out["error"] = "the command was killed from outside (for example an out-of-memory kill)"
	default:
		out["error"] = "the command did not run: " + res.Phase
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func shellError(status int, body []byte) string {
	raw, _ := json.Marshal(map[string]string{
		"error": fmt.Sprintf("sandbox service returned HTTP %d: %s", status, strings.TrimSpace(string(body))),
	})
	return string(raw)
}

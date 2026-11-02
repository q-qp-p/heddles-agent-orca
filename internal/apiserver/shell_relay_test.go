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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/heddles/agent-orca/internal/shellsandbox"
)

type fakeRunner struct {
	ready   bool
	err     error
	job     shellsandbox.Job
	polls   int
	runJobs int // polls that answer running before job
	started []string
	stopped []string
	gotCmd  string
	gotDir  string
}

func (f *fakeRunner) Start(_ context.Context, _, _, id, command, workDir string) (bool, error) {
	if f.err != nil || !f.ready {
		return false, f.err
	}
	f.started = append(f.started, id)
	f.gotCmd, f.gotDir = command, workDir
	return true, nil
}

func (f *fakeRunner) Poll(context.Context, string, string, string) (shellsandbox.Job, error) {
	f.polls++
	if f.polls <= f.runJobs {
		return shellsandbox.Job{Phase: shellsandbox.PhaseRunning}, nil
	}
	return f.job, f.err
}

func (f *fakeRunner) Stop(_ context.Context, _, _, id string) error {
	f.stopped = append(f.stopped, id)
	return f.err
}

const shellJobID = "sh-0123456789abcdef"

// shellCall goes through handleAgentRun, so it also proves the routes exist
// behind the token review.
func shellCall(t *testing.T, s *Server, user, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	s.k8s.(*k8sfake.Clientset).PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true, User: authv1.UserInfo{Username: user},
		}}, nil
	})
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.handleAgentRun(rec, req)
	return rec
}

func TestShellStartRoutesAndAnswers(t *testing.T) {
	s, _ := budgetServer(t, budgetRun(nil))
	runner := &fakeRunner{ready: false}
	s.SetShellSandboxes(runner)
	body := `{"id":"` + shellJobID + `","command":"make test","workDir":"/sandbox/x"}`

	if rec := shellCall(t, s, routerSA, http.MethodPost, "/agentrun/team-a/run-1/shell", body); rec.Code != http.StatusAccepted {
		t.Fatalf("a provisioning sandbox answers 202, got %d", rec.Code)
	}
	runner.ready = true
	if rec := shellCall(t, s, routerSA, http.MethodPost, "/agentrun/team-a/run-1/shell", body); rec.Code != http.StatusCreated {
		t.Fatalf("a started job answers 201, got %d: %s", rec.Code, rec.Body)
	}
	if len(runner.started) != 1 || runner.gotCmd != "make test" || runner.gotDir != "/sandbox/x" {
		t.Errorf("unexpected start: %+v", runner)
	}
}

func TestShellStartValidation(t *testing.T) {
	s, _ := budgetServer(t, budgetRun(nil))
	s.SetShellSandboxes(&fakeRunner{ready: true})
	for name, body := range map[string]string{
		"bad json":   `{`,
		"bad id":     `{"id":"../../etc","command":"ls"}`,
		"no command": `{"id":"` + shellJobID + `","command":""}`,
		"too long":   `{"id":"` + shellJobID + `","command":"` + strings.Repeat("a", maxShellCommandBytes+1) + `"}`,
	} {
		if rec := shellCall(t, s, routerSA, http.MethodPost, "/agentrun/team-a/run-1/shell", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, rec.Code)
		}
	}
}

func TestShellRejectsOtherNamespaces(t *testing.T) {
	s, _ := budgetServer(t, budgetRun(nil))
	s.SetShellSandboxes(&fakeRunner{ready: true})
	other := "system:serviceaccount:team-b:agentorca-agent-coder"
	body := `{"id":"` + shellJobID + `","command":"ls"}`
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/agentrun/team-a/run-1/shell"},
		{http.MethodGet, "/agentrun/team-a/run-1/shell/" + shellJobID},
		{http.MethodPost, "/agentrun/team-a/run-1/shell/" + shellJobID + "/stop"},
	} {
		if rec := shellCall(t, s, other, c.method, c.path, body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s from another namespace: %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

func TestShellWithoutRunnerIs501(t *testing.T) {
	s, _ := budgetServer(t, budgetRun(nil))
	rec := shellCall(t, s, routerSA, http.MethodPost, "/agentrun/team-a/run-1/shell", `{"id":"`+shellJobID+`","command":"ls"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("got %d, want 501", rec.Code)
	}
}

func TestShellPoll(t *testing.T) {
	s, _ := budgetServer(t, budgetRun(nil))
	runner := &fakeRunner{job: shellsandbox.Job{Phase: shellsandbox.PhaseExited, ExitCode: 3, Output: "out"}, runJobs: 1}
	s.SetShellSandboxes(runner)
	path := "/agentrun/team-a/run-1/shell/" + shellJobID

	if rec := shellCall(t, s, routerSA, http.MethodGet, path, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("a running job answers 204 without a wait, got %d", rec.Code)
	}
	rec := shellCall(t, s, routerSA, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a finished job answers 200, got %d", rec.Code)
	}
	var got shellJobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Phase != "exited" || got.ExitCode != 3 || got.Output != "out" {
		t.Errorf("unexpected body: %s", rec.Body)
	}
}

func TestShellStop(t *testing.T) {
	s, _ := budgetServer(t, budgetRun(nil))
	runner := &fakeRunner{}
	s.SetShellSandboxes(runner)
	rec := shellCall(t, s, routerSA, http.MethodPost, "/agentrun/team-a/run-1/shell/"+shellJobID+"/stop", "")
	if rec.Code != http.StatusNoContent || len(runner.stopped) != 1 || runner.stopped[0] != shellJobID {
		t.Errorf("stop: code %d, stopped %v", rec.Code, runner.stopped)
	}
}

func TestShellFailureMapping(t *testing.T) {
	s, _ := budgetServer(t, budgetRun(nil))
	body := `{"id":"` + shellJobID + `","command":"ls"}`
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"rejected":    {shellsandbox.Rejectedf("Agent has no spec.shell"), http.StatusConflict},
		"unavailable": {shellsandbox.ErrUnavailable, http.StatusNotImplemented},
		"other":       {context.DeadlineExceeded, http.StatusBadGateway},
	} {
		s.SetShellSandboxes(&fakeRunner{err: c.err})
		rec := shellCall(t, s, routerSA, http.MethodPost, "/agentrun/team-a/run-1/shell", body)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", name, rec.Code, c.want)
		}
		if name == "rejected" && !strings.Contains(rec.Body.String(), "no spec.shell") {
			t.Errorf("the reason must reach the agent: %s", rec.Body)
		}
	}
}

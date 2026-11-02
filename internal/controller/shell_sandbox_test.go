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
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/shellsandbox"
)

func shellAgent() *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentSpec{
			Shell: &agentorcav1alpha1.ShellAccessSpec{ClassRef: "coder", Image: "ghcr.io/acme/sandbox:1", WorkDir: "/sandbox/work"},
		},
	}
}

func shellRun() *agentorcav1alpha1.AgentRun {
	return &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "dev", Input: "go"},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning},
	}
}

func shellNamespace(optedIn bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	if optedIn {
		ns.Labels = map[string]string{security.LabelEnableSandboxes: annotationTrue}
	}
	return ns
}

func shellClass() *agentorcav1alpha1.SandboxClass {
	return &agentorcav1alpha1.SandboxClass{ObjectMeta: metav1.ObjectMeta{Name: "coder"}}
}

// startedScript returns the job script a start command carried to the sandbox:
// the sixth argument of the start command is its base64.
func startedScript(t *testing.T, command []string) string {
	t.Helper()
	if len(command) < 7 || command[0] != "sh" {
		t.Fatalf("not a job start command: %v", command)
	}
	raw, err := base64.StdEncoding.DecodeString(command[5])
	if err != nil {
		t.Fatalf("decoding the job script: %v", err)
	}
	return string(raw)
}

func newShellService(t *testing.T, gw openshell.Gateway, objs ...client.Object) (*ShellSandboxService, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(sandboxScheme(t)).WithObjects(objs...).Build()
	return &ShellSandboxService{Client: cl, Sandboxes: gw}, cl
}

func TestShellStartWithoutGateway(t *testing.T) {
	s := &ShellSandboxService{}
	if _, err := s.Start(context.Background(), "default", "run-1", "sh-1", "ls", ""); !errors.Is(err, shellsandbox.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
}

func TestShellStartRejections(t *testing.T) {
	noShell := shellAgent()
	noShell.Spec.Shell = nil
	ended := shellRun()
	ended.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	badImage := shellAgent()
	badImage.Spec.Shell.Image = "evil.example/sandbox:1"

	tests := []struct {
		name    string
		objs    []client.Object
		allowed []string
		want    string
	}{
		{"unknown run", []client.Object{shellAgent(), shellNamespace(true), shellClass()}, nil, "not found"},
		{"agent without spec.shell", []client.Object{noShell, shellRun(), shellNamespace(true), shellClass()}, nil, "no spec.shell"},
		{"run ended", []client.Object{shellAgent(), ended, shellNamespace(true), shellClass()}, nil, "has ended"},
		{"namespace not opted in", []client.Object{shellAgent(), shellRun(), shellNamespace(false), shellClass()}, nil, "not opted in"},
		{"class missing", []client.Object{shellAgent(), shellRun(), shellNamespace(true)}, nil, "SandboxClass"},
		{"image not allowed", []client.Object{badImage, shellRun(), shellNamespace(true), shellClass()}, []string{"ghcr.io/acme/"}, "not allowed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := newStubGateway()
			s, _ := newShellService(t, gw, tc.objs...)
			s.AllowedRegistries = tc.allowed
			_, err := s.Start(context.Background(), "default", "run-1", "sh-1", "ls", "")
			var rejected *shellsandbox.RejectedError
			if !errors.As(err, &rejected) || !strings.Contains(rejected.Reason, tc.want) {
				t.Fatalf("got %v, want a rejection containing %q", err, tc.want)
			}
			if len(gw.sandboxes) != 0 {
				t.Errorf("a rejected request must not create a sandbox, got %d", len(gw.sandboxes))
			}
		})
	}
}

func TestShellStartCreatesTheRunsSandboxOnceAndRunsTheJob(t *testing.T) {
	gw := newStubGateway()
	var scripts []string
	gw.execFn = func(_ context.Context, _ string, command []string, _ openshell.ExecOptions) (*openshell.ExecResult, error) {
		scripts = append(scripts, startedScript(t, command))
		return &openshell.ExecResult{}, nil
	}
	s, cl := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), shellClass())
	ctx := context.Background()
	name := openshell.ShellSandboxName("default", "run-1")

	ready, err := s.Start(ctx, "default", "run-1", "sh-1", "make test", "")
	if err != nil || ready {
		t.Fatalf("first call: ready=%v err=%v, want provisioning", ready, err)
	}
	sb := gw.sandboxes[name]
	if sb == nil {
		t.Fatalf("the sandbox %s was not created", name)
	}
	spec := gw.specs[name]
	if spec.Template.Image != "ghcr.io/acme/sandbox:1" {
		t.Errorf("image = %q", spec.Template.Image)
	}
	if strings.Join(spec.Command, " ") != "sh -c while :; do sleep 3600; done" {
		t.Errorf("the main process must be idle, got %v", spec.Command)
	}
	if sb.Labels[openshell.LabelShell] != annotationTrue || sb.Labels[openshell.LabelRunName] != "run-1" {
		t.Errorf("labels = %v", sb.Labels)
	}
	var run agentorcav1alpha1.AgentRun
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "run-1"}, &run); err != nil {
		t.Fatal(err)
	}
	if run.Annotations[AnnotationShellSandbox] != name {
		t.Errorf("the run must record the sandbox for cleanup, annotations = %v", run.Annotations)
	}
	if len(scripts) != 0 {
		t.Errorf("nothing may run in a sandbox that is not ready")
	}

	sb.Status.Phase = osv1.SandboxReady
	ready, err = s.Start(ctx, "default", "run-1", "sh-1", "make test", "")
	if err != nil || !ready {
		t.Fatalf("second call: ready=%v err=%v, want started", ready, err)
	}
	if len(gw.sandboxes) != 1 {
		t.Errorf("one sandbox per run, got %d", len(gw.sandboxes))
	}
	if len(scripts) != 1 || !strings.Contains(scripts[0], "make test") {
		t.Errorf("the command must reach the sandbox once, got %q", scripts)
	}
}

func TestShellStartUsesTheWorkDirOrTheAgents(t *testing.T) {
	for _, tc := range []struct{ name, workDir, want string }{
		{"agent default", "", "cd '/sandbox/work'"},
		{"call overrides", "/tmp/x", "cd '/tmp/x'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := newStubGateway()
			var scripts []string
			gw.execFn = func(_ context.Context, _ string, command []string, _ openshell.ExecOptions) (*openshell.ExecResult, error) {
				scripts = append(scripts, startedScript(t, command))
				return &openshell.ExecResult{}, nil
			}
			s, _ := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), shellClass())
			if _, err := s.Start(context.Background(), "default", "run-1", "sh-1", "ls", tc.workDir); err != nil {
				t.Fatal(err)
			}
			gw.sandboxes[openshell.ShellSandboxName("default", "run-1")].Status.Phase = osv1.SandboxReady
			if ready, err := s.Start(context.Background(), "default", "run-1", "sh-1", "ls", tc.workDir); err != nil || !ready {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
			if len(scripts) != 1 || !strings.Contains(scripts[0], tc.want) {
				t.Errorf("want the job to contain %q, got %q", tc.want, scripts)
			}
		})
	}
}

func TestShellFailedSandboxIsDeletedSoTheNextCallRecreatesIt(t *testing.T) {
	gw := newStubGateway()
	s, _ := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), shellClass())
	name := openshell.ShellSandboxName("default", "run-1")

	if _, err := s.Start(context.Background(), "default", "run-1", "sh-1", "ls", ""); err != nil {
		t.Fatal(err)
	}
	gw.sandboxes[name].Status.Phase = osv1.SandboxError

	ready, err := s.Start(context.Background(), "default", "run-1", "sh-1", "ls", "")
	if err == nil || ready || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("ready=%v err=%v, want a failure", ready, err)
	}
	if len(gw.deleted) != 1 || gw.deleted[0] != name {
		t.Errorf("the failed sandbox must be deleted, deleted = %v", gw.deleted)
	}
}

func TestShellPollMapsAJob(t *testing.T) {
	gw := newStubGateway()
	gw.execFn = func(context.Context, string, []string, openshell.ExecOptions) (*openshell.ExecResult, error) {
		return &openshell.ExecResult{Stdout: []byte("STATE exited 4\n---\nbad\n")}, nil
	}
	s, _ := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), shellClass())

	job, err := s.Poll(context.Background(), "default", "run-1", "sh-1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Phase != shellsandbox.PhaseExited || job.ExitCode != 4 || !strings.Contains(job.Output, "bad") {
		t.Errorf("job = %+v", job)
	}
}

func TestDeleteRunSandboxAlsoDeletesTheShellSandbox(t *testing.T) {
	gw := newStubGateway()
	run := shellRun()
	run.Annotations = map[string]string{AnnotationShellSandbox: "ao-shell"}
	r, _ := sandboxReconciler(t, gw, run)

	r.deleteRunSandbox(context.Background(), run)

	if len(gw.deleted) != 1 || gw.deleted[0] != "ao-shell" {
		t.Errorf("deleted = %v, want [ao-shell]", gw.deleted)
	}
}

func TestDeleteRunSandboxLeavesRunsWithoutAShellAlone(t *testing.T) {
	gw := newStubGateway()
	run := shellRun()
	r, _ := sandboxReconciler(t, gw, run)

	r.deleteRunSandbox(context.Background(), run)

	if len(gw.deleted) != 0 {
		t.Errorf("no gateway call expected for a run without a shell sandbox, got %v", gw.deleted)
	}
}

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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/githubapp"
	"github.com/heddles/agent-orca/internal/openshell"
	"github.com/heddles/agent-orca/internal/shellsandbox"
)

const (
	gitTestRepo      = "heddles/agent-orca"
	gitTestNamespace = "default"
	gitTestRun       = "run-1"
)

// gitClass is a class whose githubApp credential gives git access to gitTestRepo.
func gitClass(push bool) *agentorcav1alpha1.SandboxClass {
	app := &agentorcav1alpha1.SandboxCredentialGitHubApp{Repositories: []string{gitTestRepo}}
	if push {
		app.Permissions = map[string]agentorcav1alpha1.SandboxCredentialPermission{"contents": agentorcav1alpha1.SandboxCredentialPermissionWrite}
	}
	return &agentorcav1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "coder"},
		Spec: agentorcav1alpha1.SandboxClassSpec{
			Enforcement:   agentorcav1alpha1.SandboxEnforcementEnforce,
			EgressPresets: []agentorcav1alpha1.SandboxEgressPreset{agentorcav1alpha1.SandboxEgressPresetGitHubRead},
			Credentials: []agentorcav1alpha1.SandboxCredential{{
				Name:   "github",
				Source: agentorcav1alpha1.SandboxCredentialSource{GitHubApp: app},
				Bind:   agentorcav1alpha1.SandboxCredentialBind{Host: "github.com"},
			}},
		},
	}
}

func TestClassGitHubAndMerge(t *testing.T) {
	if got := openshell.ClassGitHub(nil); got != nil {
		t.Errorf("nil class: %+v", got)
	}
	if got := openshell.ClassGitHub(credClass("")); got != nil {
		t.Errorf("a class with only secretRef credentials has no git access, got %+v", got)
	}
	if got := openshell.ClassGitHub(gitClass(false)); got == nil || got.Push || len(got.Repositories) != 1 {
		t.Errorf("read class: %+v", got)
	}
	if got := openshell.ClassGitHub(gitClass(true)); got == nil || !got.Push {
		t.Errorf("write class: %+v", got)
	}

	a := &agentorcav1alpha1.SandboxGitHub{Repositories: []string{"o/a"}}
	b := &agentorcav1alpha1.SandboxGitHub{Repositories: []string{"o/a", "o/b"}, Push: true}
	got := openshell.MergeGitHub(a, b)
	if len(got.Repositories) != 2 || !got.Push {
		t.Errorf("merge = %+v, want the union with push", got)
	}
	if len(a.Repositories) != 1 || a.Push {
		t.Errorf("MergeGitHub modified its input: %+v", a)
	}
	if openshell.MergeGitHub(nil, nil) != nil {
		t.Error("merging nothing must give nothing")
	}
}

func TestApplyClassFoldsGitHubIntoTheAgent(t *testing.T) {
	agent := sandboxedAgent()
	agent.Spec.Runtime.Sandbox.GitHub = &agentorcav1alpha1.SandboxGitHub{Repositories: []string{"heddles/other"}}
	got, _ := openshell.ApplyClass(agent, gitClass(true), openshell.Placement{})
	gh := got.Spec.Runtime.Sandbox.GitHub
	if gh == nil || len(gh.Repositories) != 2 || !gh.Push {
		t.Errorf("github = %+v, want the Agent's and the class's repositories with push", gh)
	}
	if agent.Spec.Runtime.Sandbox.GitHub.Push {
		t.Error("the Agent passed in was modified")
	}
}

func assertGitBound(t *testing.T, gw *stubGateway, spec *osv1.SandboxSpec, run string, push bool) {
	t.Helper()
	rule, ok := spec.Policy.NetworkPolicies["agentorca-git-github"]
	if !ok || rule.Endpoints[0].CredentialBinding == nil {
		t.Fatalf("no bound git rule in %v", spec.Policy.NetworkPolicies)
	}
	provider := rule.Endpoints[0].CredentialBinding.Provider
	p := gw.providers[provider]
	if p == nil || !strings.HasPrefix(p.Spec.Credentials[openshell.GitTokenEnvVar], "ghs_token") {
		t.Fatalf("provider %s = %+v, want the minted token", provider, p)
	}
	if p.Labels[openshell.LabelRunName] != run {
		t.Errorf("git provider labels = %v, want the run's so cleanup revokes it", p.Labels)
	}
	gotPush := false
	for _, r := range rule.Endpoints[0].Rules {
		gotPush = gotPush || strings.HasSuffix(r.Allow.Path, "/git-receive-pack")
	}
	if gotPush != push {
		t.Errorf("push allowed = %v, want %v", gotPush, push)
	}
	if raw, err := json.Marshal(spec); err != nil || strings.Contains(string(raw), "ghs_token") {
		t.Errorf("the token is in the sandbox spec (err %v)", err)
	}
	// Plain github.com egress gives way: the destination carries one binding.
	for name, r := range spec.Policy.NetworkPolicies {
		for _, e := range r.Endpoints {
			if e.Host == "github.com" && name != "agentorca-git-github" {
				t.Errorf("rule %s still reaches github.com without the credential", name)
			}
		}
	}
}

func TestShellSandboxGetsGitHubAccessFromTheClass(t *testing.T) {
	gw := newStubGateway()
	s, cl := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), gitClass(true))
	minter := &stubMinter{ttl: time.Hour}
	s.GitHubApp = minter

	if _, err := s.Start(context.Background(), gitTestNamespace, gitTestRun, "sh-1", "git push", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(minter.reqs) != 1 {
		t.Fatalf("minted %d tokens, want 1", len(minter.reqs))
	}
	want := githubapp.TokenRequest{Repositories: []string{"agent-orca"}, Permissions: map[string]string{"contents": "write", "metadata": "read"}}
	if got := minter.reqs[0]; len(got.Repositories) != 1 || got.Repositories[0] != "agent-orca" || got.Permissions["contents"] != "write" || got.Permissions["metadata"] != want.Permissions["metadata"] {
		t.Errorf("token request = %+v, want %+v", got, want)
	}
	spec := gw.specs[openshell.ShellSandboxName(gitTestNamespace, gitTestRun)]
	assertGitBound(t, gw, spec, gitTestRun, true)
	if spec.Environment["GIT_CONFIG_COUNT"] == "" {
		t.Errorf("git is not configured to use the placeholder, env = %v", spec.Environment)
	}

	var run agentorcav1alpha1.AgentRun
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: gitTestNamespace, Name: gitTestRun}, &run); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, run.Annotations[AnnotationShellGitTokenExpiry]); err != nil {
		t.Errorf("expiry annotation = %q: %v", run.Annotations[AnnotationShellGitTokenExpiry], err)
	}
}

func TestShellSandboxReadOnlyClassCannotPush(t *testing.T) {
	gw := newStubGateway()
	s, _ := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), gitClass(false))
	s.GitHubApp = &stubMinter{ttl: time.Hour}
	if _, err := s.Start(context.Background(), gitTestNamespace, gitTestRun, "sh-1", "git fetch", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertGitBound(t, gw, gw.specs[openshell.ShellSandboxName(gitTestNamespace, gitTestRun)], gitTestRun, false)
}

func TestShellSandboxRejectsAGitHubClassWithoutTheApp(t *testing.T) {
	gw := newStubGateway()
	s, _ := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), gitClass(true))

	_, err := s.Start(context.Background(), gitTestNamespace, gitTestRun, "sh-1", "git push", "")
	var rejected *shellsandbox.RejectedError
	if !errors.As(err, &rejected) || !strings.Contains(rejected.Reason, "no GitHub App") {
		t.Fatalf("err = %v, want a rejection naming the missing GitHub App", err)
	}
	if len(gw.sandboxes) != 0 {
		t.Error("a sandbox was created without the access its class promises")
	}
}

func TestShellGitTokenRefresh(t *testing.T) {
	gw := newStubGateway()
	s, cl := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), gitClass(true))
	minter := &stubMinter{ttl: time.Hour}
	s.GitHubApp = minter
	ctx := context.Background()
	if _, err := s.Start(ctx, gitTestNamespace, gitTestRun, "sh-1", "true", ""); err != nil {
		t.Fatal(err)
	}
	provider := openshell.GitProviderName(gitTestNamespace, gitTestRun+"-shell")
	first := gw.providers[provider].Spec.Credentials[openshell.GitTokenEnvVar]

	// A long command polls; a token with time left is kept.
	_, _ = s.Poll(ctx, gitTestNamespace, gitTestRun, "sh-1")
	if len(minter.reqs) != 1 {
		t.Fatalf("minted %d tokens while the first was fresh, want 1", len(minter.reqs))
	}

	// Close to expiry, the next poll replaces the token in the same provider.
	var run agentorcav1alpha1.AgentRun
	if err := cl.Get(ctx, client.ObjectKey{Namespace: gitTestNamespace, Name: gitTestRun}, &run); err != nil {
		t.Fatal(err)
	}
	run.Annotations[AnnotationShellGitTokenExpiry] = time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	if err := cl.Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Poll(ctx, gitTestNamespace, gitTestRun, "sh-1")
	if len(minter.reqs) != 2 {
		t.Fatalf("minted %d tokens, want a renewal", len(minter.reqs))
	}
	if second := gw.providers[provider].Spec.Credentials[openshell.GitTokenEnvVar]; second == first || second == "" {
		t.Errorf("provider token = %q after renewal, was %q", second, first)
	}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: gitTestNamespace, Name: gitTestRun}, &run); err != nil {
		t.Fatal(err)
	}
	exp, err := time.Parse(time.RFC3339, run.Annotations[AnnotationShellGitTokenExpiry])
	if err != nil || time.Until(exp) < 30*time.Minute {
		t.Errorf("expiry after renewal = %v (%v), want about an hour out", exp, err)
	}
}

func TestRuntimeSandboxGetsGitHubAccessFromTheClass(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	r, cl := sandboxReconciler(t, gw, run, token, shellNamespace(true), gitClass(true))
	r.GitHubApp = &stubMinter{ttl: time.Hour}

	if _, err := r.ensureSandbox(context.Background(), run, classedAgent(), "tok", "http://router:8080"); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	assertGitBound(t, gw, gw.specs[openshell.SandboxName(gitTestNamespace, gitTestRun, 0)], gitTestRun, true)
	if got := fetchSandboxRun(t, cl, run); got.Status.SandboxGitTokenExpiry == nil {
		t.Error("the token expiry was not recorded in the run status")
	}

	// The monitor loop renews the class's token too, though the Agent has none of its own.
	minter := r.GitHubApp.(*stubMinter)
	got := fetchSandboxRun(t, cl, run)
	expired := metav1.NewTime(time.Now().Add(time.Minute))
	got.Status.SandboxGitTokenExpiry = &expired
	r.refreshSandboxGit(context.Background(), got, classedAgent())
	if len(minter.reqs) != 2 {
		t.Errorf("minted %d tokens, want a renewal from the class's credential", len(minter.reqs))
	}
}

func TestRuntimeSandboxFailsWithoutTheAppForAGitHubClass(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	r, _ := sandboxReconciler(t, gw, run, token, shellNamespace(true), gitClass(false))
	_, err := r.ensureSandbox(context.Background(), run, classedAgent(), "tok", "http://router:8080")
	if !errors.Is(err, errNoGitHubApp) {
		t.Fatalf("err = %v, want errNoGitHubApp", err)
	}
}

func TestWorkflowGitHubUnionsTheClass(t *testing.T) {
	wf := &agentorcav1alpha1.AgentWorkflow{Spec: agentorcav1alpha1.AgentWorkflowSpec{
		Sandbox: &agentorcav1alpha1.WorkflowSandboxSpec{Repo: "heddles/agent-orca"}}}
	if got := workflowGitHub(wf, nil); len(got.Repositories) != 1 || got.Push {
		t.Errorf("no class: %+v", got)
	}
	got := workflowGitHub(wf, &agentorcav1alpha1.SandboxGitHub{Repositories: []string{"heddles/docs"}, Push: true})
	if len(got.Repositories) != 2 || !got.Push {
		t.Errorf("with class: %+v, want both repositories with push", got)
	}
}

func TestEnsureWorkflowGitNeedsTheAppForAGitHubClass(t *testing.T) {
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: gitTestNamespace},
		Spec:       agentorcav1alpha1.AgentWorkflowSpec{Sandbox: &agentorcav1alpha1.WorkflowSandboxSpec{Repo: gitTestRepo}},
	}
	r := &AgentWorkflowReconciler{}
	if git, err := r.ensureWorkflowGit(context.Background(), wf, nil); git != nil || err != nil {
		t.Errorf("no App and no class access: git=%v err=%v, want nothing (public clone)", git, err)
	}
	_, err := r.ensureWorkflowGit(context.Background(), wf, &agentorcav1alpha1.SandboxGitHub{Repositories: []string{gitTestRepo}})
	if !errors.Is(err, errNoGitHubApp) {
		t.Errorf("err = %v, want errNoGitHubApp", err)
	}
}

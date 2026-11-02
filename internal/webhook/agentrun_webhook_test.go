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

package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/security"
)

func TestCheckRegistryAllowlist(t *testing.T) {
	tests := []struct {
		name              string
		ociRef            string
		allowedRegistries []string
		wantErr           bool
	}{
		{name: "empty allowlist allows anything", ociRef: "evil.io/malware:latest", allowedRegistries: nil, wantErr: false},
		{name: "exact match", ociRef: "gcr.io/my-project", allowedRegistries: []string{"gcr.io/my-project"}, wantErr: false},
		{name: "prefix with sub-path", ociRef: "gcr.io/my-project/image:v1", allowedRegistries: []string{"gcr.io/my-project"}, wantErr: false},
		{name: "bypass: prefix must not match across path boundary", ociRef: "gcr.io/my-evil-org/image:v1", allowedRegistries: []string{"gcr.io/my"}, wantErr: true},
		{name: "bypass: similar project name", ociRef: "gcr.io/my-project-evil/image:v1", allowedRegistries: []string{"gcr.io/my-project"}, wantErr: true},
		{name: "allowed prefix with trailing slash", ociRef: "gcr.io/my-project/image:v1", allowedRegistries: []string{"gcr.io/my-project/"}, wantErr: false},
		{name: "trailing slash does not match different prefix", ociRef: "gcr.io/my-project-evil/image:v1", allowedRegistries: []string{"gcr.io/my-project/"}, wantErr: true},
		{name: "registry-only prefix", ociRef: "docker.io/library/nginx:latest", allowedRegistries: []string{"docker.io"}, wantErr: false},
		{name: "no match among multiple registries", ociRef: "evil.io/malware:latest", allowedRegistries: []string{"gcr.io/my-project", "docker.io/library"}, wantErr: true},
		{name: "match second entry in list", ociRef: "docker.io/library/nginx:latest", allowedRegistries: []string{"gcr.io/my-project", "docker.io/library"}, wantErr: false},
		{name: "deeply nested image under allowed prefix", ociRef: "gcr.io/my-project/team/service/image:v2", allowedRegistries: []string{"gcr.io/my-project"}, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRegistryAllowlist(tt.ociRef, tt.allowedRegistries)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkRegistryAllowlist(%q, %v) error = %v, wantErr %v",
					tt.ociRef, tt.allowedRegistries, err, tt.wantErr)
			}
		})
	}
}

// --- SecurityContextOverride gating tests ---

// baseAgent returns an Agent that passes the baseline (non-override) validation.
func baseAgent(ns string, withGuardrail bool) agentorcav1alpha1.Agent {
	a := agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "red-pwnbox", Namespace: ns},
		Spec: agentorcav1alpha1.AgentSpec{
			Runtime:          agentorcav1alpha1.AgentRuntime{OCIRef: "python:3.12-slim"},
			ModelSelectorRef: "default",
		},
	}
	if withGuardrail {
		a.Spec.GuardrailPolicyRef = "arena-bounds"
	}
	return a
}

func newValidator(nsName string, labelled bool) *AgentValidator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if labelled {
		ns.Labels = map[string]string{
			security.LabelEnablePrivilegedPods: security.PrivilegedPodsAllowedValue,
		}
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	return &AgentValidator{Client: cl}
}

//go:fix inline
func ptrTo[T any](v T) *T { return new(v) }

func TestAgentValidatorSecurityContextOverride(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		labelled  bool
		guardrail bool
		override  *agentorcav1alpha1.PodSecurityOverride
		wantErr   bool
		errSubstr string
	}{
		{name: "no override: allowed even without label/guardrail", labelled: false, guardrail: false, override: nil, wantErr: false},
		{name: "override without namespace label is denied", labelled: false, guardrail: true, override: &agentorcav1alpha1.PodSecurityOverride{Privileged: true}, wantErr: true, errSubstr: "not opted in"},
		{name: "override without guardrail policy is denied", labelled: true, guardrail: false, override: &agentorcav1alpha1.PodSecurityOverride{Privileged: true}, wantErr: true, errSubstr: "guardrailPolicyRef is empty"},
		{name: "privileged + addCapabilities both set is denied", labelled: true, guardrail: true, override: &agentorcav1alpha1.PodSecurityOverride{Privileged: true, AddCapabilities: []string{"NET_ADMIN"}}, wantErr: true, errSubstr: "mutually exclusive"},
		{name: "runAsUser=0 without privileged is denied", labelled: true, guardrail: true, override: &agentorcav1alpha1.PodSecurityOverride{AddCapabilities: []string{"NET_ADMIN"}, RunAsUser: ptrTo[int64](0)}, wantErr: true, errSubstr: "requires privileged"},
		{name: "labelled + guardrail + privileged is allowed", labelled: true, guardrail: true, override: &agentorcav1alpha1.PodSecurityOverride{Privileged: true}, wantErr: false},
		{name: "labelled + guardrail + NET_ADMIN cap is allowed", labelled: true, guardrail: true, override: &agentorcav1alpha1.PodSecurityOverride{AddCapabilities: []string{"NET_ADMIN"}}, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator("red-team", tt.labelled)
			agent := baseAgent("red-team", tt.guardrail)
			agent.Spec.Runtime.SecurityContextOverride = tt.override

			err := v.validate(ctx, &agent)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
				t.Fatalf("expected error containing %q, got %q", tt.errSubstr, err)
			}
		})
	}
}

// namespace lookup failure path: pointing an override at a namespace the fake
// client does not know about returns an error (never silently allowed).
func TestAgentValidatorOverrideMissingNamespace(t *testing.T) {
	v := newValidator("red-team", true)
	agent := baseAgent("does-not-exist", true)
	agent.Spec.Runtime.SecurityContextOverride = &agentorcav1alpha1.PodSecurityOverride{Privileged: true}

	err := v.validate(context.Background(), &agent)
	if err == nil {
		t.Fatal("expected an error when namespace lookup fails, got nil")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("expected a namespace error, got %q", err)
	}
}

// --- Sandbox gating tests ---

func newLabelledValidator(nsName string, labels map[string]string) *AgentValidator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName, Labels: labels}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	return &AgentValidator{Client: cl}
}

// sandboxAgent returns an Agent that passes every sandbox rule.
func sandboxAgent(ns string) agentorcav1alpha1.Agent {
	a := baseAgent(ns, true)
	a.Spec.NetworkIsolation = &agentorcav1alpha1.NetworkIsolationSpec{SplitPod: true}
	a.Spec.Runtime.Command = []string{"/usr/local/bin/agent"}
	a.Spec.Runtime.Sandbox = &agentorcav1alpha1.SandboxSpec{
		Provider: agentorcav1alpha1.SandboxProviderOpenShell,
		Egress: []agentorcav1alpha1.SandboxEgressRule{
			{Host: "pypi.org", Binaries: []string{"/usr/bin/python3"}},
		},
		Filesystem: &agentorcav1alpha1.SandboxFilesystem{ReadOnly: []string{"/opt/venv"}},
	}
	return a
}

func TestAgentValidatorSandbox(t *testing.T) {
	sandboxes := map[string]string{security.LabelEnableSandboxes: "true"}
	tests := []struct {
		name      string
		nsLabels  map[string]string // nil = the sandbox opt-in only
		mutate    func(*agentorcav1alpha1.Agent)
		errSubstr string // empty = allowed
	}{
		{name: "labelled namespace + guardrail + splitPod is allowed"},
		{name: "namespace not opted in is denied", nsLabels: map[string]string{}, errSubstr: "not opted in"},
		{name: "privileged-pods label does not opt in to sandboxes",
			nsLabels:  map[string]string{security.LabelEnablePrivilegedPods: security.PrivilegedPodsAllowedValue},
			errSubstr: "enable-sandboxes"},
		{name: "missing guardrail policy is denied",
			mutate: func(a *agentorcav1alpha1.Agent) { a.Spec.GuardrailPolicyRef = "" }, errSubstr: "guardrailPolicyRef is empty"},
		{name: "combined pod is denied",
			mutate: func(a *agentorcav1alpha1.Agent) { a.Spec.NetworkIsolation = nil }, errSubstr: "splitPod=true"},
		{name: "securityContextOverride is denied even with both opt-ins",
			nsLabels: map[string]string{
				security.LabelEnableSandboxes:      "true",
				security.LabelEnablePrivilegedPods: security.PrivilegedPodsAllowedValue,
			},
			mutate: func(a *agentorcav1alpha1.Agent) {
				a.Spec.Runtime.SecurityContextOverride = &agentorcav1alpha1.PodSecurityOverride{Privileged: true}
			}, errSubstr: "mutually exclusive"},
		{name: "agent secretRefs are denied",
			mutate: func(a *agentorcav1alpha1.Agent) {
				a.Spec.Runtime.SecretRefs = []agentorcav1alpha1.SecretMount{{Name: "vpn"}}
			}, errSubstr: "secretRefs"},
		{name: "http input mode is denied",
			mutate: func(a *agentorcav1alpha1.Agent) { a.Spec.Runtime.InputMode = "http" }, errSubstr: "inputMode=env"},
		{name: "missing command is denied: OpenShell ignores the image ENTRYPOINT",
			mutate: func(a *agentorcav1alpha1.Agent) { a.Spec.Runtime.Command = nil }, errSubstr: "requires spec.runtime.command"},
		{name: "init-container framework is denied",
			mutate: func(a *agentorcav1alpha1.Agent) { a.Spec.Runtime.Framework = "autogen" }, errSubstr: "init container"},
		{name: "shim framework is denied",
			mutate: func(a *agentorcav1alpha1.Agent) { a.Spec.Runtime.Framework = "shim" }, errSubstr: "shim"},
		{name: "langgraph framework is allowed",
			mutate: func(a *agentorcav1alpha1.Agent) { a.Spec.Runtime.Framework = "langgraph" }},
		{name: "relative egress binary is denied",
			mutate:    func(a *agentorcav1alpha1.Agent) { a.Spec.Runtime.Sandbox.Egress[0].Binaries = []string{"python3"} },
			errSubstr: "not an absolute path"},
		{name: "relative filesystem path is denied",
			mutate:    func(a *agentorcav1alpha1.Agent) { a.Spec.Runtime.Sandbox.Filesystem.ReadWrite = []string{"work"} },
			errSubstr: "not an absolute path"},
		{name: "excluding a listed tool is allowed",
			mutate: func(a *agentorcav1alpha1.Agent) {
				a.Spec.Tools = []string{"github"}
				a.Spec.Runtime.Sandbox.Tools = &agentorcav1alpha1.SandboxTools{ExcludeTools: []string{"github"}}
			}},
		{name: "excluding an unknown tool is denied",
			mutate: func(a *agentorcav1alpha1.Agent) {
				a.Spec.Tools = []string{"github"}
				a.Spec.Runtime.Sandbox.Tools = &agentorcav1alpha1.SandboxTools{ExcludeTools: []string{"gihtub"}}
			}, errSubstr: "not in spec.tools"},
		{name: "github access to one owner is allowed",
			mutate: func(a *agentorcav1alpha1.Agent) {
				a.Spec.Runtime.Sandbox.GitHub = &agentorcav1alpha1.SandboxGitHub{Repositories: []string{"heddles/agent-orca", "heddles/docs"}, Push: true}
			}},
		{name: "github repositories of two owners are denied",
			mutate: func(a *agentorcav1alpha1.Agent) {
				a.Spec.Runtime.Sandbox.GitHub = &agentorcav1alpha1.SandboxGitHub{Repositories: []string{"heddles/agent-orca", "other/repo"}}
			}, errSubstr: "share one owner"},
		{name: "github repository without owner is denied",
			mutate: func(a *agentorcav1alpha1.Agent) {
				a.Spec.Runtime.Sandbox.GitHub = &agentorcav1alpha1.SandboxGitHub{Repositories: []string{"agent-orca"}}
			}, errSubstr: "not an owner/name"},
		{name: "read-write root is denied",
			mutate:    func(a *agentorcav1alpha1.Agent) { a.Spec.Runtime.Sandbox.Filesystem.ReadWrite = []string{"/"} },
			errSubstr: "must not include /"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := tt.nsLabels
			if labels == nil {
				labels = sandboxes
			}
			v := newLabelledValidator("agents", labels)
			agent := sandboxAgent("agents")
			if tt.mutate != nil {
				tt.mutate(&agent)
			}

			err := v.validate(context.Background(), &agent)
			if tt.errSubstr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want allowed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.errSubstr) {
				t.Fatalf("validate() = %v, want error containing %q", err, tt.errSubstr)
			}
		})
	}
}

func TestAgentValidatorSandboxClass(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentorcav1alpha1.AddToScheme(scheme)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agents", Labels: map[string]string{
		security.LabelEnableSandboxes: "true", "team": "a",
	}}}
	open := &agentorcav1alpha1.SandboxClass{ObjectMeta: metav1.ObjectMeta{Name: "open"}}
	teamB := &agentorcav1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: agentorcav1alpha1.SandboxClassSpec{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}},
		},
	}
	v := &AgentValidator{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, open, teamB).Build()}

	for class, errSubstr := range map[string]string{
		"open":    "",
		"team-b":  "not available to namespace",
		"missing": "not found",
	} {
		agent := sandboxAgent("agents")
		agent.Spec.Runtime.Sandbox.ClassRef = class
		err := v.validate(context.Background(), &agent)
		switch {
		case errSubstr == "" && err != nil:
			t.Errorf("class %s: validate() = %v, want allowed", class, err)
		case errSubstr != "" && (err == nil || !strings.Contains(err.Error(), errSubstr)):
			t.Errorf("class %s: validate() = %v, want %q", class, err, errSubstr)
		}
	}
}

func TestAgentValidatorShell(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentorcav1alpha1.AddToScheme(scheme)
	optedIn := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agents", Labels: map[string]string{security.LabelEnableSandboxes: "true"}}}
	plain := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}
	class := &agentorcav1alpha1.SandboxClass{ObjectMeta: metav1.ObjectMeta{Name: "coder"}}

	shellAgent := func(ns, classRef, image string) agentorcav1alpha1.Agent {
		a := baseAgent(ns, true)
		a.Spec.Runtime.OCIRef = "ghcr.io/acme/agent:1"
		a.Spec.Shell = &agentorcav1alpha1.ShellAccessSpec{ClassRef: classRef, Image: image}
		return a
	}
	tests := []struct {
		name      string
		agent     agentorcav1alpha1.Agent
		allowed   []string
		errSubstr string // empty = allowed
	}{
		{name: "opted-in namespace with a usable class", agent: shellAgent("agents", "coder", "ghcr.io/acme/box:1")},
		{name: "namespace not opted in", agent: shellAgent("plain", "coder", "ghcr.io/acme/box:1"), errSubstr: "not opted in"},
		{name: "unknown class", agent: shellAgent("agents", "nope", "ghcr.io/acme/box:1"), errSubstr: "spec.shell.classRef: SandboxClass \"nope\" not found"},
		{name: "image outside the allowed registries", agent: shellAgent("agents", "coder", "evil.example/box:1"),
			allowed: []string{"ghcr.io/acme/"}, errSubstr: "spec.shell.image rejected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &AgentValidator{
				Client:            fake.NewClientBuilder().WithScheme(scheme).WithObjects(optedIn, plain, class).Build(),
				AllowedRegistries: tc.allowed,
			}
			err := v.validate(context.Background(), &tc.agent)
			switch {
			case tc.errSubstr == "" && err != nil:
				t.Errorf("validate() = %v, want allowed", err)
			case tc.errSubstr != "" && (err == nil || !strings.Contains(err.Error(), tc.errSubstr)):
				t.Errorf("validate() = %v, want %q", err, tc.errSubstr)
			}
		})
	}
}

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

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
	"github.com/heddles/agent-orca/internal/shellsandbox"
)

const (
	credTestNamespace = "default"
	credTestRun       = "run-1"
)

func credSecret(namespace, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "warehouse", Namespace: namespace},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

// credClass is a class that reaches api.example.com read-write and holds a
// credential for it, read from Secret warehouse/token. secretNamespace "" reads
// the workload's own namespace.
func credClass(secretNamespace string) *agentorcav1alpha1.SandboxClass {
	return &agentorcav1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "coder"},
		Spec: agentorcav1alpha1.SandboxClassSpec{
			Enforcement: agentorcav1alpha1.SandboxEnforcementEnforce,
			Egress:      []agentorcav1alpha1.SandboxEgressRule{{Host: "api.example.com", Access: agentorcav1alpha1.SandboxAccessReadWrite}},
			Credentials: []agentorcav1alpha1.SandboxCredential{{
				Name: "warehouse",
				Source: agentorcav1alpha1.SandboxCredentialSource{SecretRef: &agentorcav1alpha1.SandboxCredentialSecretRef{
					Name: "warehouse", Key: "token", Namespace: secretNamespace}},
				Bind: agentorcav1alpha1.SandboxCredentialBind{Host: "api.example.com", Binaries: []string{"/usr/bin/curl"}},
			}},
		},
	}
}

func tenant(name string, namespaces ...string) *agentorcav1alpha1.TenantConfig {
	return &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-orca-system"},
		Spec:       agentorcav1alpha1.TenantConfigSpec{AllowedNamespaces: namespaces},
	}
}

func TestResolveClassCredentials(t *testing.T) {
	githubClass := credClass("")
	githubClass.Spec.Credentials[0].Source = agentorcav1alpha1.SandboxCredentialSource{
		GitHubApp: &agentorcav1alpha1.SandboxCredentialGitHubApp{Repositories: []string{"heddles/agent-orca"}}}

	tests := []struct {
		name    string
		class   *agentorcav1alpha1.SandboxClass
		objs    []client.Object
		want    string // value, when it resolves
		wantErr string
	}{
		{"own namespace by default", credClass(""), []client.Object{credSecret(credTestNamespace, "token", "  tok-default\n")}, "tok-default", ""},
		{"another namespace of the tenant", credClass("data"),
			[]client.Object{credSecret("data", "token", "tok-data"), tenant("t", credTestNamespace, "data")}, "tok-data", ""},
		{"namespace outside the tenant", credClass("other"),
			[]client.Object{credSecret("other", "token", "x"), tenant("t", credTestNamespace, "data")}, "", "not in the tenant"},
		{"namespace in a tenant the workload is not in", credClass("other"),
			[]client.Object{credSecret("other", "token", "x"), tenant("t", "other", "data")}, "", "not in the tenant"},
		{"another namespace with no tenant at all", credClass("data"),
			[]client.Object{credSecret("data", "token", "x")}, "", "not in the tenant"},
		{"the workload's own namespace named explicitly needs no tenant", credClass(credTestNamespace),
			[]client.Object{credSecret(credTestNamespace, "token", "tok")}, "tok", ""},
		{"missing secret", credClass(""), nil, "", "not found"},
		{"missing key", credClass(""), []client.Object{credSecret(credTestNamespace, "other", "x")}, "", `no key "token"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, cl := newShellService(t, newStubGateway(), tt.objs...)
			creds, err := resolveClassCredentials(context.Background(), cl, credTestNamespace, tt.class)
			if tt.wantErr != "" {
				if !errors.Is(err, errSandboxClassUnusable) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want errSandboxClassUnusable containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(creds) != 1 || creds[0].Value != tt.want || creds[0].Host != "api.example.com" || creds[0].Class != "coder" {
				t.Errorf("creds = %+v, want one with value %q", creds, tt.want)
			}
		})
	}

	t.Run("a githubApp credential is minted as git access, not read as a Secret", func(t *testing.T) {
		_, cl := newShellService(t, newStubGateway())
		creds, err := resolveClassCredentials(context.Background(), cl, credTestNamespace, githubClass)
		if err != nil || len(creds) != 0 {
			t.Errorf("creds = %v err = %v, want none", creds, err)
		}
	})

	t.Run("a class without credentials reads nothing", func(t *testing.T) {
		_, cl := newShellService(t, newStubGateway())
		creds, err := resolveClassCredentials(context.Background(), cl, credTestNamespace, &agentorcav1alpha1.SandboxClass{})
		if err != nil || creds != nil {
			t.Errorf("creds = %v err = %v", creds, err)
		}
		if creds, err := resolveClassCredentials(context.Background(), cl, credTestNamespace, nil); err != nil || creds != nil {
			t.Errorf("nil class: creds = %v err = %v", creds, err)
		}
	})
}

// assertCredentialBound checks that spec binds the credential and holds no secret.
func assertCredentialBound(t *testing.T, gw *stubGateway, spec *osv1.SandboxSpec, value string) {
	t.Helper()
	rule, ok := spec.Policy.NetworkPolicies["agentorca-cred-warehouse"]
	if !ok || len(rule.Endpoints) != 1 || rule.Endpoints[0].CredentialBinding == nil {
		t.Fatalf("no bound credential rule in %v", spec.Policy.NetworkPolicies)
	}
	provider := rule.Endpoints[0].CredentialBinding.Provider
	if len(spec.Providers) != 1 || spec.Providers[0] != provider {
		t.Errorf("sandbox providers = %v, want [%s]", spec.Providers, provider)
	}
	p := gw.providers[provider]
	if p == nil || p.Spec.Credentials["AGENTORCA_CRED_WAREHOUSE"] != value {
		t.Fatalf("provider %s = %+v, want the secret under AGENTORCA_CRED_WAREHOUSE", provider, p)
	}
	if gw.profiles[p.Type] == nil {
		t.Errorf("profile %s was not ensured", p.Type)
	}
	if raw, err := json.Marshal(spec); err != nil || strings.Contains(string(raw), value) {
		t.Errorf("the secret value is in the sandbox spec (err %v)", err)
	}
}

func TestShellSandboxGetsClassCredentials(t *testing.T) {
	gw := newStubGateway()
	s, _ := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), credClass(""),
		credSecret(credTestNamespace, "token", "sh-secret"))

	if _, err := s.Start(context.Background(), credTestNamespace, credTestRun, "sh-1", "curl x", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	spec := gw.specs[openshell.ShellSandboxName(credTestNamespace, credTestRun)]
	assertCredentialBound(t, gw, spec, "sh-secret")

	// The providers carry the run's labels, so the run's cleanup revokes them.
	for _, p := range gw.providers {
		if p.Labels[openshell.LabelRunName] != credTestRun || p.Labels[openshell.LabelRunNamespace] != credTestNamespace {
			t.Errorf("provider labels = %v, want the run's", p.Labels)
		}
	}
}

func TestShellSandboxRejectsWhenACredentialCannotBeResolved(t *testing.T) {
	gw := newStubGateway()
	s, _ := newShellService(t, gw, shellAgent(), shellRun(), shellNamespace(true), credClass(""))

	_, err := s.Start(context.Background(), credTestNamespace, credTestRun, "sh-1", "curl x", "")
	var rejected *shellsandbox.RejectedError
	if !errors.As(err, &rejected) || !strings.Contains(rejected.Reason, "not found") {
		t.Fatalf("err = %v, want a rejection naming the missing Secret", err)
	}
	if len(gw.sandboxes) != 0 || len(gw.providers) != 0 {
		t.Errorf("nothing may be created without the promised credentials: %d sandboxes, %d providers", len(gw.sandboxes), len(gw.providers))
	}
}

func TestRuntimeSandboxGetsClassCredentials(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	r, _ := sandboxReconciler(t, gw, run, token, shellNamespace(true), credClass(""),
		credSecret(credTestNamespace, "token", "rt-secret"))

	if _, err := r.ensureSandbox(context.Background(), run, classedAgent(), "tok", "http://router:8080"); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	assertCredentialBound(t, gw, gw.specs[openshell.SandboxName(credTestNamespace, credTestRun, 0)], "rt-secret")
}

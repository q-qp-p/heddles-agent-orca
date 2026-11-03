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

package openshell

import (
	"encoding/json"
	"strings"
	"testing"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const testAPIHost = "api.example.com"

func testCredential(name, host string, binaries, paths []string, inject *agentorcav1alpha1.SandboxCredentialInject) *ClassCredential {
	c := NewClassCredential("general", &agentorcav1alpha1.SandboxCredential{
		Name:   name,
		Bind:   agentorcav1alpha1.SandboxCredentialBind{Host: host, Binaries: binaries, Paths: paths},
		Inject: inject,
	})
	c.Value = "s3cret"
	c.Provider = "provider-" + name
	return c
}

func policyWithEgress(rules ...agentorcav1alpha1.SandboxEgressRule) (*osv1.SandboxPolicy, []agentorcav1alpha1.SandboxEgressRule) {
	sb := &agentorcav1alpha1.SandboxSpec{Enforcement: agentorcav1alpha1.SandboxEnforcementEnforce, Egress: rules}
	return CompilePolicy("router", sb, nil), rules
}

func TestNewClassCredentialDefaults(t *testing.T) {
	c := testCredential("api", "API.Example.com", nil, nil, nil)
	if c.Host != testAPIHost || c.Port != 443 || c.Style != agentorcav1alpha1.SandboxCredentialInjectBearer {
		t.Errorf("defaults = host %q port %d style %q", c.Host, c.Port, c.Style)
	}
	if got := c.EnvVar(); got != "AGENTORCA_CRED_API" {
		t.Errorf("EnvVar = %q", got)
	}
	if got := ClassCredentialEnvVar("my-db"); got != "AGENTORCA_CRED_MY_DB" {
		t.Errorf("EnvVar(my-db) = %q", got)
	}
}

func TestClassCredentialProfileAndProvider(t *testing.T) {
	tests := []struct {
		name       string
		inject     *agentorcav1alpha1.SandboxCredentialInject
		wantStyle  string
		wantHeader string
	}{
		{"default is bearer", nil, "bearer", "authorization"},
		{"basic", &agentorcav1alpha1.SandboxCredentialInject{Style: agentorcav1alpha1.SandboxCredentialInjectBasic}, "basic", "authorization"},
		{"named header", &agentorcav1alpha1.SandboxCredentialInject{Style: agentorcav1alpha1.SandboxCredentialInjectHeader, Name: "X-Api-Key"}, "header", "x-api-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testCredential("api", testAPIHost, nil, nil, tt.inject)
			p := ClassCredentialProfile("ns", c)
			if len(p.Credentials) != 1 {
				t.Fatalf("credentials = %+v", p.Credentials)
			}
			cr := p.Credentials[0]
			if cr.AuthStyle != tt.wantStyle || cr.HeaderName != tt.wantHeader || !cr.Secret || cr.EnvVars[0] != "AGENTORCA_CRED_API" {
				t.Errorf("credential = %+v", cr)
			}
			if len(p.Endpoints) != 0 {
				t.Errorf("a profile must have no endpoints of its own, got %+v", p.Endpoints)
			}

			prov := ClassCredentialProvider("ns", "run-1", c, map[string]string{"k": "v"})
			if prov.Type != p.ID || prov.Name != ProviderName("ns", "run-1", ClassCredentialSource("general", "api")) {
				t.Errorf("provider = %s of type %s, want profile %s", prov.Name, prov.Type, p.ID)
			}
			if prov.Spec.Credentials["AGENTORCA_CRED_API"] != "s3cret" || prov.Labels["k"] != "v" {
				t.Errorf("provider spec = %+v labels %v", prov.Spec, prov.Labels)
			}
		})
	}
}

func TestApplyClassCredentialsBindsTheEndpointAndRemovesTheOtherRules(t *testing.T) {
	policy, egress := policyWithEgress(
		agentorcav1alpha1.SandboxEgressRule{Host: testAPIHost, Access: agentorcav1alpha1.SandboxAccessReadWrite},
		agentorcav1alpha1.SandboxEgressRule{Host: "pypi.org"},
	)
	c := testCredential("api", testAPIHost, []string{"/usr/bin/curl"}, nil, nil)

	ApplyClassCredentials(policy, []*ClassCredential{c}, egress)

	rule, ok := policy.NetworkPolicies["agentorca-cred-api"]
	if !ok || len(rule.Endpoints) != 1 {
		t.Fatalf("no credential rule in %v", policy.NetworkPolicies)
	}
	ep := rule.Endpoints[0]
	if ep.Host != testAPIHost || len(ep.Ports) != 1 || ep.Ports[0] != 443 {
		t.Errorf("endpoint = %+v", ep)
	}
	if ep.CredentialBinding == nil || ep.CredentialBinding.Provider != "provider-api" {
		t.Errorf("binding = %+v", ep.CredentialBinding)
	}
	if ep.Access != accessPreset(agentorcav1alpha1.SandboxAccessReadWrite) {
		t.Errorf("the credential must get the access the class's egress gives the host, got %q", ep.Access)
	}
	if ep.Enforcement != osv1.NetworkEnforcementModeEnforce {
		t.Errorf("enforcement = %v, want the policy's", ep.Enforcement)
	}
	if len(rule.Binaries) != 1 || rule.Binaries[0].Path != "/usr/bin/curl" {
		t.Errorf("binaries = %+v", rule.Binaries)
	}
	// The plain rule for the same destination gives way; others stay.
	for name, r := range policy.NetworkPolicies {
		for _, e := range r.Endpoints {
			if e.Host == testAPIHost && name != "agentorca-cred-api" {
				t.Errorf("rule %s still reaches the bound endpoint without the credential", name)
			}
		}
	}
	found := false
	for _, r := range policy.NetworkPolicies {
		for _, e := range r.Endpoints {
			found = found || e.Host == "pypi.org"
		}
	}
	if !found {
		t.Error("an unrelated egress rule was removed")
	}
}

func TestApplyClassCredentialsKeepsOtherPortsOfTheHost(t *testing.T) {
	policy, egress := policyWithEgress(agentorcav1alpha1.SandboxEgressRule{Host: testAPIHost, Ports: []int32{443, 8443}})
	ApplyClassCredentials(policy, []*ClassCredential{testCredential("api", testAPIHost, nil, nil, nil)}, egress)

	var ports []uint32
	for name, r := range policy.NetworkPolicies {
		if strings.HasPrefix(name, "agentorca-egress-") {
			for _, e := range r.Endpoints {
				ports = append(ports, e.Ports...)
			}
		}
	}
	if len(ports) != 1 || ports[0] != 8443 {
		t.Errorf("remaining plain ports = %v, want [8443]", ports)
	}
}

func TestApplyClassCredentialsLimitsToPaths(t *testing.T) {
	tests := []struct {
		name        string
		access      agentorcav1alpha1.SandboxAccess
		wantMethods []string
		notMethods  []string
	}{
		{"read-only egress", agentorcav1alpha1.SandboxAccessReadOnly, []string{"GET", "HEAD", "OPTIONS"}, []string{"POST", "PUT", "PATCH", "DELETE", "*"}},
		{"read-write egress", agentorcav1alpha1.SandboxAccessReadWrite, []string{"GET", "POST", "PUT", "PATCH"}, []string{"DELETE", "*"}},
		{"full egress", agentorcav1alpha1.SandboxAccessFull, []string{"*"}, []string{"GET"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, egress := policyWithEgress(agentorcav1alpha1.SandboxEgressRule{Host: testAPIHost, Access: tt.access})
			c := testCredential("api", testAPIHost, nil, []string{"/v1/**", "/health"}, nil)
			ApplyClassCredentials(policy, []*ClassCredential{c}, egress)

			rule := policy.NetworkPolicies["agentorca-cred-api"]
			ep := rule.Endpoints[0]
			if ep.Access != 0 {
				t.Errorf("an endpoint with path rules must not also set an access preset, got %v", ep.Access)
			}
			methods := map[string]map[string]bool{}
			for _, r := range ep.Rules {
				if methods[r.Allow.Path] == nil {
					methods[r.Allow.Path] = map[string]bool{}
				}
				methods[r.Allow.Path][r.Allow.Method] = true
			}
			if len(methods) != 2 {
				t.Fatalf("rules cover paths %v, want /v1/** and /health", methods)
			}
			for path, got := range methods {
				for _, m := range tt.wantMethods {
					if !got[m] {
						t.Errorf("%s: method %s missing", path, m)
					}
				}
				for _, m := range tt.notMethods {
					if got[m] {
						t.Errorf("%s: method %s allowed, but the class's egress is %s: a credential must not widen it", path, m, tt.access)
					}
				}
			}
			if len(rule.Binaries) != 1 || rule.Binaries[0].Path != anyBinary {
				t.Errorf("no binaries means any binary, got %+v", rule.Binaries)
			}
		})
	}
}

func TestApplyClassCredentialsLeavesAPolicyWithoutCredentialsAlone(t *testing.T) {
	policy, egress := policyWithEgress(agentorcav1alpha1.SandboxEgressRule{Host: "pypi.org"})
	before := len(policy.NetworkPolicies)
	ApplyClassCredentials(policy, nil, egress)
	ApplyClassCredentials(nil, []*ClassCredential{testCredential("a", "pypi.org", nil, nil, nil)}, egress)
	if len(policy.NetworkPolicies) != before {
		t.Errorf("policy changed: %v", policy.NetworkPolicies)
	}
}

func TestEgressMatching(t *testing.T) {
	rules := []agentorcav1alpha1.SandboxEgressRule{
		{Host: "*.corp.example.com", Ports: []int32{8443}, Access: agentorcav1alpha1.SandboxAccessReadWrite},
		{Host: "db.example.com"},
		{Host: "db.example.com", Access: agentorcav1alpha1.SandboxAccessFull},
	}
	tests := []struct {
		host   string
		port   int32
		allows bool
		access agentorcav1alpha1.SandboxAccess
	}{
		{"a.corp.example.com", 8443, true, agentorcav1alpha1.SandboxAccessReadWrite},
		{"a.corp.example.com", 443, false, agentorcav1alpha1.SandboxAccessReadOnly},
		{"corp.example.com", 8443, false, agentorcav1alpha1.SandboxAccessReadOnly},
		{"DB.example.com", 443, true, agentorcav1alpha1.SandboxAccessFull},
		{"other.example.com", 443, false, agentorcav1alpha1.SandboxAccessReadOnly},
	}
	for _, tt := range tests {
		if got := EgressAllows(rules, tt.host, tt.port); got != tt.allows {
			t.Errorf("EgressAllows(%s:%d) = %v, want %v", tt.host, tt.port, got, tt.allows)
		}
		if got := EgressAccess(rules, tt.host, tt.port); got != tt.access {
			t.Errorf("EgressAccess(%s:%d) = %q, want %q", tt.host, tt.port, got, tt.access)
		}
	}
}

func TestBuildSpecBindsClassCredentials(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{Runtime: agentorcav1alpha1.AgentRuntime{
		OCIRef: "img", Command: []string{"sleep"},
		Sandbox: &agentorcav1alpha1.SandboxSpec{
			Enforcement: agentorcav1alpha1.SandboxEnforcementEnforce,
			Egress:      []agentorcav1alpha1.SandboxEgressRule{{Host: testAPIHost}},
		},
	}}}
	c := testCredential("api", testAPIHost, nil, nil, nil)
	spec := BuildSpec(SpecInput{Agent: agent, Providers: []string{c.Provider}, ClassCredentials: []*ClassCredential{c}})

	if _, ok := spec.Policy.NetworkPolicies["agentorca-cred-api"]; !ok {
		t.Fatalf("no credential rule in %v", spec.Policy.NetworkPolicies)
	}
	if len(spec.Providers) != 1 || spec.Providers[0] != c.Provider {
		t.Errorf("providers = %v", spec.Providers)
	}
	if raw, err := json.Marshal(spec); err != nil || strings.Contains(string(raw), "s3cret") {
		t.Error("the secret value must never be in the sandbox spec")
	}
}

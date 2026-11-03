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
	"strings"
	"testing"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func secretCred(name, host string, port int32, binaries ...string) agentorcav1alpha1.SandboxCredential {
	return agentorcav1alpha1.SandboxCredential{
		Name:   name,
		Source: agentorcav1alpha1.SandboxCredentialSource{SecretRef: &agentorcav1alpha1.SandboxCredentialSecretRef{Name: "s", Key: "k"}},
		Bind:   agentorcav1alpha1.SandboxCredentialBind{Host: host, Port: port, Binaries: binaries},
	}
}

func githubCred(host string, repos ...string) agentorcav1alpha1.SandboxCredential {
	return agentorcav1alpha1.SandboxCredential{
		Name:   "gh",
		Source: agentorcav1alpha1.SandboxCredentialSource{GitHubApp: &agentorcav1alpha1.SandboxCredentialGitHubApp{Repositories: repos}},
		Bind:   agentorcav1alpha1.SandboxCredentialBind{Host: host},
	}
}

func withGitHubTweak(c agentorcav1alpha1.SandboxCredential, tweak func(*agentorcav1alpha1.SandboxCredential)) agentorcav1alpha1.SandboxCredential {
	tweak(&c)
	return c
}

func TestValidateClassCredentials(t *testing.T) {
	egress := []agentorcav1alpha1.SandboxEgressRule{
		{Host: "db.example.com"},
		{Host: "*.corp.example.com", Ports: []int32{8443}},
		{Host: "github.com"},
	}
	tests := []struct {
		name     string
		spec     agentorcav1alpha1.SandboxClassSpec
		wantErr  string // substring; empty means valid
		wantWarn int
	}{
		{"no credentials", agentorcav1alpha1.SandboxClassSpec{}, "", 0},
		{"secret on allowed host, default port", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{secretCred("db", "db.example.com", 0, "/usr/bin/psql")}}, "", 0},
		{"wildcard egress with its own port", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{secretCred("api", "a.corp.example.com", 8443, "/usr/bin/curl")}}, "", 0},
		{"host outside egress", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{secretCred("x", "evil.example.org", 0, "/usr/bin/curl")}}, "not reachable", 0},
		{"port outside egress", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{secretCred("x", "db.example.com", 5432, "/usr/bin/psql")}}, "not reachable", 0},
		{"host from a preset", agentorcav1alpha1.SandboxClassSpec{
			EgressPresets: []agentorcav1alpha1.SandboxEgressPreset{agentorcav1alpha1.SandboxEgressPresetNPM},
			Credentials:   []agentorcav1alpha1.SandboxCredential{secretCred("npm", "registry.npmjs.org", 0, "/usr/bin/npm")}}, "", 0},
		{"two credentials on one endpoint", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{
				secretCred("a", "db.example.com", 0, "/usr/bin/psql"), secretCred("b", "DB.example.com", 443, "/usr/bin/psql")}}, "one credential", 0},
		{"no binaries warns", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{secretCred("db", "db.example.com", 0)}}, "", 1},
		{"relative binary", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{secretCred("db", "db.example.com", 0, "psql")}}, "absolute path", 0},
		{"relative path", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{func() agentorcav1alpha1.SandboxCredential {
				c := secretCred("db", "db.example.com", 0, "/usr/bin/psql")
				c.Bind.Paths = []string{"api/**"}
				return c
			}()}}, "must start with /", 0},
		{"githubApp on github.com", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{githubCred("github.com", "heddles/agent-orca")}}, "", 0},
		{"githubApp with no binaries does not warn", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{githubCred("github.com", "heddles/agent-orca")}}, "", 0},
		{"githubApp with binaries", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{withGitHubTweak(githubCred("github.com", "heddles/agent-orca"),
				func(c *agentorcav1alpha1.SandboxCredential) { c.Bind.Binaries = []string{"/usr/bin/git"} })}}, "do not apply", 0},
		{"githubApp with paths", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{withGitHubTweak(githubCred("github.com", "heddles/agent-orca"),
				func(c *agentorcav1alpha1.SandboxCredential) { c.Bind.Paths = []string{"/x/**"} })}}, "do not apply", 0},
		{"githubApp with inject", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{withGitHubTweak(githubCred("github.com", "heddles/agent-orca"),
				func(c *agentorcav1alpha1.SandboxCredential) {
					c.Inject = &agentorcav1alpha1.SandboxCredentialInject{Style: agentorcav1alpha1.SandboxCredentialInjectBasic}
				})}}, "inject does not apply", 0},
		{"githubApp contents write", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{withGitHubTweak(githubCred("github.com", "heddles/agent-orca"),
				func(c *agentorcav1alpha1.SandboxCredential) {
					c.Source.GitHubApp.Permissions = map[string]agentorcav1alpha1.SandboxCredentialPermission{"contents": "write"}
				})}}, "", 0},
		{"githubApp other permission", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{withGitHubTweak(githubCred("github.com", "heddles/agent-orca"),
				func(c *agentorcav1alpha1.SandboxCredential) {
					c.Source.GitHubApp.Permissions = map[string]agentorcav1alpha1.SandboxCredentialPermission{"pull_requests": "write"}
				})}}, "only contents applies", 0},
		{"githubApp needs github.com in egress", agentorcav1alpha1.SandboxClassSpec{
			Credentials: []agentorcav1alpha1.SandboxCredential{githubCred("github.com", "heddles/agent-orca")}}, "not reachable", 0},
		{"githubApp on another host", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{githubCred("db.example.com", "heddles/agent-orca")}}, "github.com:443", 0},
		{"githubApp repositories under two owners", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{githubCred("github.com", "heddles/a", "other/b")}}, "one owner", 0},
		{"githubApp bad slug", agentorcav1alpha1.SandboxClassSpec{
			Egress: egress, Credentials: []agentorcav1alpha1.SandboxCredential{githubCred("github.com", "heddles/a.git")}}, "owner/name", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warns, err := ValidateClassCredentials(&tt.spec)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
			if err == nil && len(warns) != tt.wantWarn {
				t.Errorf("warnings = %v, want %d", warns, tt.wantWarn)
			}
		})
	}
}

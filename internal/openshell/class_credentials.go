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
	"slices"
	"strings"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// Class credentials (SandboxClass.spec.credentials) reach a sandbox the way
// tool credentials do: an endpointless profile declares the env var, a
// per-owner provider holds the value, and the sandbox policy binds the provider
// to one endpoint. The workload's environment holds only a placeholder, which
// the supervisor replaces with the value on requests to the bound endpoint.

// ClassCredentialEnvPrefix starts the placeholder env var of every class credential.
const ClassCredentialEnvPrefix = "AGENTORCA_CRED_"

// DefaultCredentialPort is the port a class credential binds when it names none.
const DefaultCredentialPort int32 = 443

// ClassCredential is a class credential with its value resolved.
type ClassCredential struct {
	// Class and Name identify the credential.
	Class string
	Name  string
	// Host and Port are the endpoint the value is bound to.
	Host string
	Port uint32
	// Binaries and Paths restrict the binding (empty means any).
	Binaries []string
	Paths    []string
	// Style and Header say where the placeholder is replaced.
	Style  agentorcav1alpha1.SandboxCredentialInjectStyle
	Header string
	// Value is the secret. It must never be logged or put in the sandbox spec.
	Value string
	// Provider is the per-owner provider instance that holds Value.
	Provider string
}

// ClassCredentialEnvVar is the placeholder env var of a credential name.
func ClassCredentialEnvVar(name string) string {
	return ClassCredentialEnvPrefix + envName(name)
}

// ClassCredentialSource is the credential's source key for ProfileID and ProviderName.
func ClassCredentialSource(class, name string) string {
	return "class-" + class + "-" + name
}

// NewClassCredential returns spec's credential as the sandbox sees it, with
// the default port and injection style filled in. It does not set Value or
// Provider.
func NewClassCredential(class string, spec *agentorcav1alpha1.SandboxCredential) *ClassCredential {
	c := &ClassCredential{
		Class:    class,
		Name:     spec.Name,
		Host:     strings.ToLower(spec.Bind.Host),
		Port:     uint32(DefaultCredentialPort),
		Binaries: slices.Clone(spec.Bind.Binaries),
		Paths:    slices.Clone(spec.Bind.Paths),
		Style:    agentorcav1alpha1.SandboxCredentialInjectBearer,
	}
	if spec.Bind.Port != 0 {
		c.Port = uint32(spec.Bind.Port)
	}
	if in := spec.Inject; in != nil {
		c.Style = in.Style
		c.Header = in.Name
	}
	return c
}

// EnvVar is the credential's placeholder env var.
func (c *ClassCredential) EnvVar() string { return ClassCredentialEnvVar(c.Name) }

// ClassCredentialProfile declares the credential's env var and how the value is
// presented. Like the tool profiles it has no endpoints: the sandbox policy's
// credential binding is the only place the value may go.
func ClassCredentialProfile(namespace string, c *ClassCredential) *osv1.ProviderProfile {
	source := ClassCredentialSource(c.Class, c.Name)
	style, header := string(c.Style), "authorization"
	if c.Style == agentorcav1alpha1.SandboxCredentialInjectHeader {
		header = strings.ToLower(c.Header)
	}
	return &osv1.ProviderProfile{
		ID:          ProfileID(namespace, source),
		DisplayName: "agent-orca class credential " + c.Class + "/" + c.Name,
		Description: "Credential " + c.Name + " of SandboxClass " + c.Class + ", managed by the agent-orca operator",
		Category:    types.ProfileCategoryOther,
		Annotations: map[string]string{
			"agentorca.io/namespace": namespace,
			"agentorca.io/source":    source,
		},
		Credentials: []osv1.ProfileCredential{{
			Name:        strings.ToLower(c.EnvVar()),
			Description: "Credential " + c.Name + " of SandboxClass " + c.Class,
			EnvVars:     []string{c.EnvVar()},
			Required:    true,
			Secret:      true,
			AuthStyle:   style,
			HeaderName:  header,
		}},
	}
}

// ClassCredentialProvider is the owner's provider holding the credential's
// value. labels identify the owner, for cleanup.
func ClassCredentialProvider(namespace, owner string, c *ClassCredential, labels map[string]string) *osv1.Provider {
	source := ClassCredentialSource(c.Class, c.Name)
	return &osv1.Provider{
		Name:   ProviderName(namespace, owner, source),
		Type:   ProfileID(namespace, source),
		Labels: labels,
		Spec:   osv1.ProviderSpec{Credentials: map[string]string{c.EnvVar(): c.Value}},
	}
}

// ApplyClassCredentials binds each credential to its endpoint in policy. A
// destination carries one binding, and OpenShell rejects endpoints that match
// the same request but disagree on credentials, so the endpoint gives way:
// the bound host:port is removed from every other egress rule and reachable
// only through the credential's own rule. The rule allows what the class's
// egress allows for the host (egress is the class's, so a credential never
// widens it) and, when the credential names paths, only those paths.
// Credentials must already have Provider set.
func ApplyClassCredentials(policy *osv1.SandboxPolicy, creds []*ClassCredential, egress []agentorcav1alpha1.SandboxEgressRule) {
	if policy == nil {
		return
	}
	for _, c := range creds {
		stripEndpoint(policy, c.Host, c.Port)

		endpoint := osv1.PolicyNetworkEndpoint{
			Host:              c.Host,
			Ports:             []uint32{c.Port},
			Protocol:          "rest",
			Enforcement:       policyEnforcement(policy),
			CredentialBinding: &types.NetworkCredentialBinding{Provider: c.Provider},
		}
		access := EgressAccess(egress, c.Host, int32(c.Port))
		if len(c.Paths) == 0 {
			endpoint.Access = accessPreset(access)
		} else {
			// Path rules replace the access preset, so spell the preset's methods out
			// per path: paths narrow what the class's egress allows, never widen it.
			for _, p := range c.Paths {
				for _, m := range accessMethods(access) {
					endpoint.Rules = append(endpoint.Rules, types.L7Rule{Allow: &types.L7Allow{Method: m, Path: p}})
				}
			}
		}
		name := "agentorca-cred-" + c.Name
		policy.NetworkPolicies[name] = osv1.NetworkPolicyRule{
			Name:      name,
			Endpoints: []osv1.PolicyNetworkEndpoint{endpoint},
			Binaries:  egressBinaries(c.Binaries),
		}
	}
}

// accessMethods are the HTTP methods an access level permits: read-only is
// GET, HEAD and OPTIONS; read-write adds the methods that change data; full is
// every method.
func accessMethods(a agentorcav1alpha1.SandboxAccess) []string {
	switch a {
	case agentorcav1alpha1.SandboxAccessFull:
		return []string{"*"}
	case agentorcav1alpha1.SandboxAccessReadWrite:
		return []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH"}
	default:
		return []string{"GET", "HEAD", "OPTIONS"}
	}
}

// stripEndpoint removes host:port from every egress and tool rule of policy.
func stripEndpoint(policy *osv1.SandboxPolicy, host string, port uint32) {
	for name, rule := range policy.NetworkPolicies {
		if !strings.HasPrefix(name, "agentorca-egress-") && !strings.HasPrefix(name, "agentorca-tool-") {
			continue
		}
		kept := rule.Endpoints[:0]
		for _, ep := range rule.Endpoints {
			if strings.EqualFold(ep.Host, host) {
				ep.Ports = deletePort(ep.Ports, port)
				if len(ep.Ports) == 0 {
					continue
				}
			}
			kept = append(kept, ep)
		}
		if len(kept) == 0 {
			delete(policy.NetworkPolicies, name)
			continue
		}
		rule.Endpoints = kept
		policy.NetworkPolicies[name] = rule
	}
}

// policyEnforcement is the enforcement mode CompilePolicy gave the policy's rules.
func policyEnforcement(policy *osv1.SandboxPolicy) osv1.NetworkEnforcementMode {
	for _, rule := range policy.NetworkPolicies {
		if len(rule.Endpoints) > 0 {
			return rule.Endpoints[0].Enforcement
		}
	}
	return osv1.NetworkEnforcementModeAudit
}

// HostMatches reports whether an egress rule's host (possibly a wildcard such
// as *.example.com) covers host.
func HostMatches(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+rest)
	}
	return pattern == host
}

// EgressAllows reports whether any rule reaches host:port. A rule with no
// ports means 443.
func EgressAllows(rules []agentorcav1alpha1.SandboxEgressRule, host string, port int32) bool {
	for _, r := range rules {
		if HostMatches(r.Host, host) && ruleHasPort(r, port) {
			return true
		}
	}
	return false
}

// EgressAccess is the widest access any rule grants host:port, read-only
// when none matches.
func EgressAccess(rules []agentorcav1alpha1.SandboxEgressRule, host string, port int32) agentorcav1alpha1.SandboxAccess {
	best := agentorcav1alpha1.SandboxAccessReadOnly
	for _, r := range rules {
		if HostMatches(r.Host, host) && ruleHasPort(r, port) && accessRank(r.Access) > accessRank(best) {
			best = r.Access
		}
	}
	return best
}

func ruleHasPort(r agentorcav1alpha1.SandboxEgressRule, port int32) bool {
	if len(r.Ports) == 0 {
		return port == DefaultCredentialPort
	}
	return slices.Contains(r.Ports, port)
}

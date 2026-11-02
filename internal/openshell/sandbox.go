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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// Labels set on every sandbox the operator creates, so a sandbox found on the
// gateway can be traced back to its AgentRun.
const (
	LabelRunName      = "agentorca.io/run"
	LabelRunNamespace = "agentorca.io/run-namespace"
	// LabelShell marks the sandbox behind a run's sandbox_exec tool.
	LabelShell             = "agentorca.io/shell"
	LabelWorkflowName      = "agentorca.io/workflow"
	LabelWorkflowNamespace = "agentorca.io/workflow-namespace"
)

// Supervisor pod labels set by the OpenShell Kubernetes driver. The supervisor
// opens the workload's outbound connections, so it is the peer the router
// NetworkPolicy admits.
const (
	SupervisorComponentLabel = "openshell.ai/component"
	SupervisorComponentValue = "supervisor"
	SandboxIDLabel           = "openshell.ai/sandbox-id"
)

// RouterPorts are the model-router ports the workload may reach: the
// OpenAI-compatible API and the Gemini-compatible API.
var RouterPorts = []uint32{8080, 8082}

// anyBinary matches every executable in the sandbox. An empty binary list
// matches none, so rules that apply to the whole agent must say so explicitly.
const anyBinary = "/**"

// SandboxName returns the gateway name for one attempt of a run's sandbox. The
// gateway caps sandbox names at 19 characters and names are unique per
// workspace, while AgentRun names are only unique per namespace, so the name is
// a short hash. It is stable within an attempt, which makes Create idempotent
// across reconciles, and changes per retry so a retry never collides with the
// previous attempt's sandbox while the gateway is still deleting it.
func SandboxName(namespace, runName string, attempt int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%d", namespace, runName, attempt))
	return "ao-" + hex.EncodeToString(sum[:])[:16]
}

// WorkflowSandboxName returns the stable gateway name of a workflow's shared
// sandbox. Its finalizer removes the sandbox before the workflow can be
// recreated with the same namespace and name.
func WorkflowSandboxName(namespace, workflowName string) string {
	return SandboxName(namespace, "workflow-"+workflowName, 0)
}

// WorkflowStepSandboxName returns the gateway name of a step's fresh sandbox.
// It differs from both the shared sandbox and every other step's sandbox.
func WorkflowStepSandboxName(namespace, workflowName, stepName string) string {
	return SandboxName(namespace, "workflow-"+workflowName+"-step-"+stepName, 0)
}

// ShellSandboxName returns the gateway name of the sandbox behind a run's
// sandbox_exec tool (Agent.spec.shell). It is stable for the run, which makes
// creating it idempotent, and differs from the run's own sandbox name.
func ShellSandboxName(namespace, runName string) string {
	return SandboxName(namespace, "shell-"+runName, 0)
}

// WorkflowJobName returns a detached job ID for an AgentRun. Continuations have
// different run names and therefore different jobs in the same fresh sandbox.
func WorkflowJobName(namespace, runName string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s", namespace, runName))
	return "job-" + hex.EncodeToString(sum[:])[:16]
}

// SpecInput is everything BuildSpec needs to describe a run's sandbox.
type SpecInput struct {
	Agent *agentorcav1alpha1.Agent
	// RouterHost is the router Service hostname the workload's base URL names.
	RouterHost string
	// ExtraRouterHosts are further router Service hostnames the sandbox may
	// reach: a workflow's shared sandbox serves one router per agent step. Each
	// must be a literal name, since OpenShell resolves policy hosts ahead of time
	// and refuses names it has no mapping for.
	ExtraRouterHosts []string
	// Env is the agent process environment, including the router base URL and
	// the per-run router credential.
	Env map[string]string
	// Tools are the tool endpoints the agent may reach directly, already
	// filtered by spec.runtime.sandbox.tools and bound to credential providers.
	Tools []ToolEndpoint
	// Providers are the provider instances to attach to the sandbox.
	Providers []string
	// Placement chooses the sandbox pod's nodes (operator-wide).
	Placement Placement
	// Git, when set, grants git access to GitHub through a credential-bound
	// endpoint (see ApplyGitAccess).
	Git *GitAccess
}

// ToolEndpoint is one network destination of one of the Agent's tools.
type ToolEndpoint struct {
	Tool string
	Host string
	Port uint32
	// CredentialProvider, when set, binds the named provider's static
	// credentials to this endpoint (see BindCredentials).
	CredentialProvider string
}

// BuildSpec returns the sandbox spec for a run: the agent image as the
// workload with the Agent's resources and the operator's placement, the agent
// command as the sandbox's main process, and the compiled policy.
func BuildSpec(in SpecInput) *osv1.SandboxSpec {
	rt := in.Agent.Spec.Runtime
	policy := CompilePolicy(in.RouterHost, rt.Sandbox, in.Tools)
	addRouterHosts(policy, rt.Sandbox, in.ExtraRouterHosts)
	ApplyGitAccess(policy, in.Git)
	return &osv1.SandboxSpec{
		Template: &osv1.SandboxTemplate{
			Image:        rt.OCIRef,
			Resources:    templateResources(in.Agent.Spec.Resources),
			DriverConfig: in.Placement.driverConfig(),
		},
		Environment:   sandboxEnv(in.Env),
		Command:       slices.Concat(rt.Command, rt.Args),
		RestartPolicy: osv1.SandboxRestartNever,
		Providers:     in.Providers,
		Policy:        policy,
	}
}

// addRouterHosts adds an endpoint to the router rule for each further host.
func addRouterHosts(policy *osv1.SandboxPolicy, sb *agentorcav1alpha1.SandboxSpec, hosts []string) {
	rule, ok := policy.NetworkPolicies["agentorca-router"]
	if len(hosts) == 0 {
		return
	}
	enforcement := osv1.NetworkEnforcementModeAudit
	if sb != nil && sb.Enforcement == agentorcav1alpha1.SandboxEnforcementEnforce {
		enforcement = osv1.NetworkEnforcementModeEnforce
	}
	if !ok {
		rule = osv1.NetworkPolicyRule{Name: "agentorca-router", Binaries: []osv1.PolicyNetworkBinary{{Path: anyBinary}}}
	}
	for _, host := range hosts {
		rule.Endpoints = append(rule.Endpoints, osv1.PolicyNetworkEndpoint{
			Host:        host,
			Ports:       slices.Clone(RouterPorts),
			Protocol:    "rest",
			Enforcement: enforcement,
			Access:      osv1.NetworkAccessPresetFull,
		})
	}
	policy.NetworkPolicies["agentorca-router"] = rule
}

// CompilePolicy builds the sandbox policy for an agent. The model-router is
// always reachable; every other destination must be listed in sb.Egress or
// come from tools. The policy is deterministic for a given input.
func CompilePolicy(routerHost string, sb *agentorcav1alpha1.SandboxSpec, tools []ToolEndpoint) *osv1.SandboxPolicy {
	enforcement := osv1.NetworkEnforcementModeAudit
	if sb != nil && sb.Enforcement == agentorcav1alpha1.SandboxEnforcementEnforce {
		enforcement = osv1.NetworkEnforcementModeEnforce
	}

	policy := &osv1.SandboxPolicy{
		Version: 1,
		// The workdir is read-write. Because the policy always has a network rule,
		// the sandbox adds its baseline paths at startup (read-only /usr, /lib,
		// /etc, /app, /proc, /var/log; read-write /tmp) where the image has them.
		Filesystem: &osv1.FilesystemPolicy{IncludeWorkdir: true},
		// The kernel must support Landlock: a silent best_effort fallback would
		// drop the filesystem rules without telling anyone.
		Landlock:        &osv1.LandlockPolicy{Compatibility: "hard_requirement"},
		NetworkPolicies: map[string]osv1.NetworkPolicyRule{},
	}
	// A sandbox with no router (a workflow's idle sandbox, before an agent step
	// brings one) has no router rule.
	if routerHost != "" {
		policy.NetworkPolicies["agentorca-router"] = osv1.NetworkPolicyRule{
			Name: "agentorca-router",
			Endpoints: []osv1.PolicyNetworkEndpoint{{
				Host:        routerHost,
				Ports:       slices.Clone(RouterPorts),
				Protocol:    "rest",
				Enforcement: enforcement,
				Access:      osv1.NetworkAccessPresetFull,
			}},
			Binaries: []osv1.PolicyNetworkBinary{{Path: anyBinary}},
		}
	}
	// OpenShell rejects a policy in which endpoints matching the same request
	// disagree on credential settings, so a host:port that carries a credential
	// binding is reachable only through its bound endpoint.
	bound := map[string]bool{}
	for _, ep := range tools {
		if ep.CredentialProvider != "" {
			bound[hostPort(ep.Host, ep.Port)] = true
		}
	}

	// One rule per tool, so the policy (and its audit log) names the tool a
	// connection was allowed for. Tool APIs take writes, so access is full.
	for _, ep := range tools {
		if ep.CredentialProvider == "" && bound[hostPort(ep.Host, ep.Port)] {
			continue
		}
		name := "agentorca-tool-" + ep.Tool
		rule := policy.NetworkPolicies[name]
		rule.Name = name
		rule.Binaries = []osv1.PolicyNetworkBinary{{Path: anyBinary}}
		endpoint := osv1.PolicyNetworkEndpoint{
			Host:        ep.Host,
			Ports:       []uint32{ep.Port},
			Protocol:    "rest",
			Enforcement: enforcement,
			Access:      osv1.NetworkAccessPresetFull,
		}
		if ep.CredentialProvider != "" {
			endpoint.CredentialBinding = &types.NetworkCredentialBinding{Provider: ep.CredentialProvider}
		}
		rule.Endpoints = append(rule.Endpoints, endpoint)
		policy.NetworkPolicies[name] = rule
	}

	if sb == nil {
		return policy
	}

	if sb.Filesystem != nil {
		policy.Filesystem.ReadOnly = slices.Clone(sb.Filesystem.ReadOnly)
		policy.Filesystem.ReadWrite = slices.Clone(sb.Filesystem.ReadWrite)
	}

	for i, rule := range sb.Egress {
		ports := slices.DeleteFunc(egressPorts(rule.Ports), func(p uint32) bool {
			return bound[hostPort(strings.ToLower(rule.Host), p)]
		})
		if len(ports) == 0 {
			continue // the tool's credentialed endpoint already allows it
		}
		name := fmt.Sprintf("agentorca-egress-%d", i)
		policy.NetworkPolicies[name] = osv1.NetworkPolicyRule{
			Name: name,
			Endpoints: []osv1.PolicyNetworkEndpoint{{
				Host:        rule.Host,
				Ports:       ports,
				Protocol:    "rest",
				Enforcement: enforcement,
				Access:      accessPreset(rule.Access),
			}},
			Binaries: egressBinaries(rule.Binaries),
		}
	}
	return policy
}

func hostPort(host string, port uint32) string {
	return host + ":" + strconv.FormatUint(uint64(port), 10)
}

func egressPorts(ports []int32) []uint32 {
	if len(ports) == 0 {
		return []uint32{443}
	}
	out := make([]uint32, 0, len(ports))
	for _, p := range ports {
		out = append(out, uint32(p)) //nolint:gosec // ports are validated 1-65535 at admission
	}
	return out
}

func egressBinaries(paths []string) []osv1.PolicyNetworkBinary {
	if len(paths) == 0 {
		return []osv1.PolicyNetworkBinary{{Path: anyBinary}}
	}
	out := make([]osv1.PolicyNetworkBinary, 0, len(paths))
	for _, p := range paths {
		out = append(out, osv1.PolicyNetworkBinary{Path: p})
	}
	return out
}

func accessPreset(a agentorcav1alpha1.SandboxAccess) osv1.NetworkAccessPreset {
	switch a {
	case agentorcav1alpha1.SandboxAccessReadWrite:
		return osv1.NetworkAccessPresetReadWrite
	case agentorcav1alpha1.SandboxAccessFull:
		return osv1.NetworkAccessPresetFull
	default:
		return osv1.NetworkAccessPresetReadOnly
	}
}

// Outcome is a sandbox's run state as the AgentRun controller sees it.
type Outcome int

const (
	// OutcomeRunning means the sandbox is provisioning or its main process is running.
	OutcomeRunning Outcome = iota
	// OutcomeSucceeded means the main process exited 0.
	OutcomeSucceeded
	// OutcomeFailed means the main process exited nonzero or the sandbox errored.
	OutcomeFailed
)

// Observe maps a sandbox's status to an Outcome, with a failure reason for
// OutcomeFailed.
func Observe(sb *osv1.Sandbox) (Outcome, string) {
	switch sb.Status.Phase {
	case osv1.SandboxCompleted:
		if sb.Status.ExitCode != nil && *sb.Status.ExitCode != 0 {
			return OutcomeFailed, fmt.Sprintf("sandbox main process exited with code %d", *sb.Status.ExitCode)
		}
		return OutcomeSucceeded, ""
	case osv1.SandboxError:
		return OutcomeFailed, "sandbox error: " + failureDetail(sb)
	case osv1.SandboxStopped, osv1.SandboxDeleting:
		return OutcomeFailed, fmt.Sprintf("sandbox %s before the agent finished", sb.Status.Phase)
	}
	if adm := sb.Status.ConfigurationAdmission; adm != nil && adm.State == types.ConfigurationAdmissionRejected {
		return OutcomeFailed, "sandbox policy rejected: " + adm.Error
	}
	return OutcomeRunning, ""
}

// failureDetail summarises why a sandbox is in the Error phase: its failure
// conditions (e.g. MainProcessFailed), why it is not Ready, its exit code and
// any configuration admission error. Conditions that are merely true, like
// PodScheduled, say nothing about the failure and are skipped.
func failureDetail(sb *osv1.Sandbox) string {
	var parts []string
	describe := func(c types.SandboxCondition) string {
		d := c.Type
		if c.Reason != "" && c.Reason != c.Type {
			d += " (" + c.Reason + ")"
		}
		if c.Message != "" {
			d += ": " + c.Message
		}
		return d
	}
	for _, c := range sb.Status.Conditions {
		if c.Status == "True" && strings.Contains(c.Type, "Fail") {
			parts = append(parts, describe(c))
		}
	}
	for _, c := range sb.Status.Conditions {
		if c.Type == "Ready" && c.Status == "False" && (c.Reason != "" || c.Message != "") {
			parts = append(parts, describe(c))
		}
	}
	if sb.Status.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("main process exited with code %d", *sb.Status.ExitCode))
	}
	if adm := sb.Status.ConfigurationAdmission; adm != nil && adm.Error != "" {
		parts = append(parts, "configuration: "+adm.Error)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "; ")
}

// ToolEndpoints returns the network destinations of one Tool that a sandboxed
// agent may reach: its networkEgress rules that name a host (plus
// extraEgress, e.g. its MCPServer's networkEgress), and the URL of a remote
// MCP server. Port-only egress rules are skipped because sandbox policy is per
// host, and loopback URLs because they name the router pod's own sidecars and
// the sandbox can never reach loopback destinations.
func ToolEndpoints(tool *agentorcav1alpha1.Tool, extraEgress ...agentorcav1alpha1.EgressRule) []ToolEndpoint {
	var out []ToolEndpoint
	add := func(host string, port uint32) {
		ep := ToolEndpoint{Tool: tool.Name, Host: strings.ToLower(host), Port: port}
		if ep.Host == "" || port == 0 || isLoopback(ep.Host) || slices.Contains(out, ep) {
			return
		}
		out = append(out, ep)
	}
	for _, rule := range slices.Concat(tool.Spec.NetworkEgress, extraEgress) {
		if rule.Protocol == "" || strings.EqualFold(rule.Protocol, "TCP") {
			add(rule.Host, uint32(rule.Port)) //nolint:gosec // CRD ports are 1-65535
		}
	}
	if tool.Spec.Type == agentorcav1alpha1.ToolTypeMCP && tool.Spec.MCPConfig != nil && tool.Spec.MCPConfig.URL != "" {
		if u, err := url.Parse(tool.Spec.MCPConfig.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			port := uint32(443)
			if u.Scheme == "http" {
				port = 80
			}
			if p, err := strconv.ParseUint(u.Port(), 10, 16); err == nil {
				port = uint32(p)
			}
			add(u.Hostname(), port)
		}
	}
	return out
}

// FilterToolEndpoints applies spec.runtime.sandbox.tools to the Agent's tool
// endpoints.
func FilterToolEndpoints(sb *agentorcav1alpha1.SandboxSpec, eps []ToolEndpoint) []ToolEndpoint {
	if sb == nil || !sb.InheritsToolEgress() {
		return nil
	}
	if sb.Tools == nil {
		return eps
	}
	return slices.DeleteFunc(slices.Clone(eps), func(ep ToolEndpoint) bool {
		return slices.Contains(sb.Tools.ExcludeTools, ep.Tool) ||
			slices.ContainsFunc(sb.Tools.ExcludeHosts, func(h string) bool { return strings.EqualFold(h, ep.Host) })
	})
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// EnvB64Suffix marks the base64 copy of an environment value that had line
// breaks (see sandboxEnv).
const EnvB64Suffix = "_B64"

// sandboxEnv makes env acceptable to the gateway, which rejects values with
// line breaks. Such a value (typically a multi-line AGENTORC_INPUT) is passed
// exactly, base64-encoded, as <NAME>_B64, and <NAME> keeps a one-line version
// with each line break replaced by a space for agents that do not know the
// encoding.
func sandboxEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if !strings.ContainsAny(v, "\r\n") {
			out[k] = v
			continue
		}
		out[k+EnvB64Suffix] = base64.StdEncoding.EncodeToString([]byte(v))
		out[k] = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(v)
	}
	return out
}

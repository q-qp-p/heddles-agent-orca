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

// Package webhook provides the validating admission webhook for agent-orca CRDs.
// The webhook is registered at /validate-agentorca-io-v1alpha1-agentrun and
// /validate-agentorca-io-v1alpha1-agent. It enforces:
//
//   - Image registry allowlist: all ociRef fields must use an approved registry prefix.
//   - Required field validation beyond what kubebuilder markers express.
//   - OCI signature check hook point (actual Cosign verification deferred to the
//     AgentRun controller to avoid blocking admission for long).
//
// The webhook uses failurePolicy: Fail for AgentRun (if we can't validate, don't run)
// and failurePolicy: Ignore for other resources (avoid blocking unrelated workloads).
package webhook

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/security"
)

// AgentRunValidator validates AgentRun resources on admission.
type AgentRunValidator struct {
	Client            client.Client
	Decoder           admission.Decoder
	AllowedRegistries []string // empty = allow all
}

// +kubebuilder:webhook:path=/validate-agentorca-agentorca-io-v1alpha1-agentrun,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorca.agentorca.io,resources=agentruns,verbs=create;update,versions=v1alpha1,name=vagentrun.kb.io,admissionReviewVersions=v1

// Handle implements admission.Handler.
func (v *AgentRunValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var run agentorcav1alpha1.AgentRun
	if err := v.Decoder.DecodeRaw(req.Object, &run); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Skip validation for objects being deleted (e.g. finalizer removal updates).
	if run.DeletionTimestamp != nil {
		return admission.Allowed("ok")
	}

	if err := v.validate(ctx, &run); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("ok")
}

func (v *AgentRunValidator) validate(ctx context.Context, run *agentorcav1alpha1.AgentRun) error {
	if run.Spec.AgentRef == "" {
		return fmt.Errorf("%s", "spec.agentRef is required")
	}
	if run.Spec.Input == "" {
		return fmt.Errorf("%s", "spec.input is required")
	}

	if err := validateRunBudget(run.Spec.Budget); err != nil {
		return err
	}

	// Verify the referenced Agent exists.
	var agent agentorcav1alpha1.Agent
	if err := v.Client.Get(ctx, client.ObjectKey{
		Name:      run.Spec.AgentRef,
		Namespace: run.Namespace,
	}, &agent); err != nil {
		return fmt.Errorf("%s", fmt.Sprintf("agent %q not found in namespace %q: %v", run.Spec.AgentRef, run.Namespace, err))
	}

	// Check the agent's OCI image against the registry allowlist.
	if err := v.checkRegistry(agent.Spec.Runtime.OCIRef); err != nil {
		return fmt.Errorf("%s", fmt.Sprintf("agent runtime image rejected: %v", err))
	}

	return nil
}

// AgentValidator validates Agent resources on admission.
type AgentValidator struct {
	Client            client.Client
	Decoder           admission.Decoder
	AllowedRegistries []string // empty = allow all
}

// +kubebuilder:webhook:path=/validate-agentorca-agentorca-io-v1alpha1-agent,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorca.agentorca.io,resources=agents,verbs=create;update,versions=v1alpha1,name=vagent.kb.io,admissionReviewVersions=v1

// Handle implements admission.Handler.
func (v *AgentValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var agent agentorcav1alpha1.Agent
	if err := v.Decoder.DecodeRaw(req.Object, &agent); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if err := v.validate(ctx, &agent); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("ok")
}

func (v *AgentValidator) validate(ctx context.Context, agent *agentorcav1alpha1.Agent) error {
	if agent.Spec.Runtime.OCIRef == "" {
		return fmt.Errorf("%s", "spec.runtime.ociRef is required")
	}
	if agent.Spec.ModelSelectorRef == "" {
		return fmt.Errorf("%s", "spec.runtime.modelSelectorRef is required")
	}
	if err := v.checkRegistry(agent.Spec.Runtime.OCIRef); err != nil {
		return fmt.Errorf("%s", fmt.Sprintf("agent runtime image rejected: %v", err))
	}

	// Enforce the PodSecurityOverride safety contract before the pod is ever
	// scheduled. An override (e.g. for a VPN pwnbox) grants the agent container
	// capabilities the restricted baseline forbids, so it is gated on:
	//  1. the namespace carrying the opt-in label agentorca.io/enable-privileged-pods=true
	//  2. the Agent referencing a GuardrailPolicyRef (defense in depth), and
	//  3. a small set of internal consistency rules on the override itself.
	// Admission uses failurePolicy=Fail, so these check the role at admission time.
	if agent.Spec.Runtime.SecurityContextOverride != nil {
		if err := v.validateSecurityContextOverride(ctx, agent); err != nil {
			return err
		}
	}
	if agent.Spec.Runtime.Sandbox != nil {
		if err := v.validateSandbox(ctx, agent); err != nil {
			return err
		}
	}
	if agent.Spec.Shell != nil {
		if err := v.validateShell(ctx, agent); err != nil {
			return err
		}
	}
	return nil
}

// validateShell checks Agent.spec.shell (the sandbox_exec tool). A shell can run
// arbitrary binaries, so the namespace must opt in to sandboxes; what the
// commands can reach is then decided by the SandboxClass alone, which must be
// usable from the namespace, and the image must come from an allowed registry.
func (v *AgentValidator) validateShell(ctx context.Context, agent *agentorcav1alpha1.Agent) error {
	sh := agent.Spec.Shell
	var ns corev1.Namespace
	if err := v.Client.Get(ctx, client.ObjectKey{Name: agent.Namespace}, &ns); err != nil {
		return fmt.Errorf("looking up namespace %q for shell validation: %w", agent.Namespace, err)
	}
	if ns.Labels[security.LabelEnableSandboxes] != "true" {
		return fmt.Errorf("spec.shell is set, but namespace %q is not opted in: "+
			"add label %s=true to the namespace", agent.Namespace, security.LabelEnableSandboxes)
	}
	if err := v.checkRegistry(sh.Image); err != nil {
		return fmt.Errorf("spec.shell.image rejected: %w", err)
	}
	return v.validateSandboxClass(ctx, "spec.shell.classRef", sh.ClassRef, &ns)
}

// validateSandbox enforces the gating + consistency rules for running an Agent
// in a sandbox. A sandboxed agent has a shell and may exec and install
// arbitrary binaries, so, like a privileged pod, it needs a namespace opt-in
// and a GuardrailPolicyRef. The remaining rules reject configurations the
// sandbox path cannot honour, rather than silently dropping them.
func (v *AgentValidator) validateSandbox(ctx context.Context, agent *agentorcav1alpha1.Agent) error {
	rt := agent.Spec.Runtime
	if agent.Spec.GuardrailPolicyRef == "" {
		return fmt.Errorf("spec.runtime.sandbox is set but spec.guardrailPolicyRef is empty: " +
			"a GuardrailPolicyRef is required for sandboxed agents")
	}
	var ns corev1.Namespace
	if err := v.Client.Get(ctx, client.ObjectKey{Name: agent.Namespace}, &ns); err != nil {
		return fmt.Errorf("looking up namespace %q for sandbox validation: %w", agent.Namespace, err)
	}
	if ns.Labels[security.LabelEnableSandboxes] != "true" {
		return fmt.Errorf("spec.runtime.sandbox is set, but namespace %q is not opted in: "+
			"add label %s=true to the namespace", agent.Namespace, security.LabelEnableSandboxes)
	}

	// The model-router must stay in its own pod: the sandbox only runs the agent.
	if agent.Spec.NetworkIsolation == nil || !agent.Spec.NetworkIsolation.SplitPod {
		return fmt.Errorf("spec.runtime.sandbox requires spec.networkIsolation.splitPod=true")
	}
	if rt.SecurityContextOverride != nil {
		return fmt.Errorf("spec.runtime.sandbox and spec.runtime.securityContextOverride are mutually exclusive")
	}
	if len(rt.SecretRefs) > 0 {
		return fmt.Errorf("spec.runtime.secretRefs cannot be mounted into a sandbox; " +
			"attach the credential to a Tool so the model-router holds it")
	}
	if rt.InputMode == "http" {
		return fmt.Errorf("spec.runtime.sandbox supports inputMode=env only: the sandbox admits no inbound connections")
	}
	// OpenShell does not run the image's ENTRYPOINT: an empty command creates
	// an idle scratch sandbox, and the run would wait for its timeout.
	if len(rt.Command) == 0 {
		return fmt.Errorf("spec.runtime.sandbox requires spec.runtime.command: " +
			"OpenShell runs the given command, not the image's ENTRYPOINT")
	}
	switch rt.Framework {
	case "autogen", "semantic-kernel":
		return fmt.Errorf("framework %q configures the agent with an init container, "+
			"which sandboxes do not run; use openai-compatible or langgraph", rt.Framework)
	case "shim":
		return fmt.Errorf("framework \"shim\" rewrites vendor hostnames to the router, " +
			"which the sandbox network policy does not allow; use openai-compatible")
	}

	if name := rt.Sandbox.ClassRef; name != "" {
		if err := v.validateSandboxClass(ctx, "spec.runtime.sandbox.classRef", name, &ns); err != nil {
			return err
		}
	}
	if gh := rt.Sandbox.GitHub; gh != nil {
		if err := validateSandboxGitHub(gh); err != nil {
			return err
		}
	}

	for i, rule := range rt.Sandbox.Egress {
		for _, b := range rule.Binaries {
			if !strings.HasPrefix(b, "/") {
				return fmt.Errorf("spec.runtime.sandbox.egress[%d].binaries: %q is not an absolute path", i, b)
			}
		}
	}
	// A misspelled exclusion would silently leave the tool reachable.
	if t := rt.Sandbox.Tools; t != nil {
		for _, name := range t.ExcludeTools {
			if !slices.Contains(agent.Spec.Tools, name) {
				return fmt.Errorf("spec.runtime.sandbox.tools.excludeTools: %q is not in spec.tools", name)
			}
		}
	}
	if fs := rt.Sandbox.Filesystem; fs != nil {
		for _, p := range slices.Concat(fs.ReadOnly, fs.ReadWrite) {
			if !strings.HasPrefix(p, "/") {
				return fmt.Errorf("spec.runtime.sandbox.filesystem: %q is not an absolute path", p)
			}
		}
		if slices.Contains(fs.ReadWrite, "/") {
			return fmt.Errorf("spec.runtime.sandbox.filesystem.readWrite must not include /")
		}
	}
	return nil
}

// validateSecurityContextOverride enforces the gating + consistency rules for a
// PodSecurityOverride on an Agent. See validate() for the full rationale.
func (v *AgentValidator) validateSecurityContextOverride(ctx context.Context, agent *agentorcav1alpha1.Agent) error {
	override := agent.Spec.Runtime.SecurityContextOverride

	// Require a GuardrailPolicyRef so privileged/elevated agents always run with
	// content filtering (redact/block real endpoints & secret material) applied
	// by the model-router sidecar on every LLM call.
	if agent.Spec.GuardrailPolicyRef == "" {
		return fmt.Errorf(
			"spec.runtime.securityContextOverride is set but spec.guardrailPolicyRef is empty: " +
				"a GuardrailPolicyRef is required when escalating pod privileges")
	}

	// Require the namespace opt-in label.
	var ns corev1.Namespace
	if err := v.Client.Get(ctx, client.ObjectKey{Name: agent.Namespace}, &ns); err != nil {
		return fmt.Errorf("looking up namespace %q for securityContextOverride validation: %w", agent.Namespace, err)
	}
	if ns.Labels[security.LabelEnablePrivilegedPods] != security.PrivilegedPodsAllowedValue {
		return fmt.Errorf(
			"spec.runtime.securityContextOverride is set, but namespace %q is not opted in: "+
				"add label %s=%s to the namespace",
			agent.Namespace, security.LabelEnablePrivilegedPods, security.PrivilegedPodsAllowedValue)
	}

	// Consistency: privileged and addCapabilities are mutually exclusive.
	if override.Privileged && len(override.AddCapabilities) > 0 {
		return fmt.Errorf(
			"spec.runtime.securityContextOverride.privileged and addCapabilities are mutually exclusive")
	}

	// Root (uid 0) is only permitted when fully privileged.
	if override.RunAsUser != nil && *override.RunAsUser == 0 && !override.Privileged {
		return fmt.Errorf(
			"spec.runtime.securityContextOverride.runAsUser=0 requires privileged=true " +
				"(root is not permitted with a granular capability add)")
	}

	return nil
}

// ToolValidator validates Tool resources on admission.
type ToolValidator struct {
	Client            client.Client
	Decoder           admission.Decoder
	AllowedRegistries []string
}

// +kubebuilder:webhook:path=/validate-agentorca-agentorca-io-v1alpha1-tool,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorca.agentorca.io,resources=tools,verbs=create;update,versions=v1alpha1,name=vtool.kb.io,admissionReviewVersions=v1

// Handle implements admission.Handler.
func (v *ToolValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	var tool agentorcav1alpha1.Tool
	if err := v.Decoder.DecodeRaw(req.Object, &tool); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if tool.Spec.OCIRef != "" {
		if err := v.checkRegistry(tool.Spec.OCIRef); err != nil {
			return admission.Denied(fmt.Sprintf("tool OCI image rejected: %v", err))
		}
	}
	return admission.Allowed("ok")
}

// checkRegistry verifies an OCI reference against the configured allowlist.
// If AllowedRegistries is empty, all registries are allowed.
func (v *AgentRunValidator) checkRegistry(ociRef string) error {
	return checkRegistryAllowlist(ociRef, v.AllowedRegistries)
}

func (v *AgentValidator) checkRegistry(ociRef string) error {
	return checkRegistryAllowlist(ociRef, v.AllowedRegistries)
}

func (v *ToolValidator) checkRegistry(ociRef string) error {
	return checkRegistryAllowlist(ociRef, v.AllowedRegistries)
}

// CheckRegistryAllowlist verifies an OCI image reference against an allowlist of
// registry prefixes; an empty allowlist allows everything. The workflow
// controller uses it for container steps, which no admission webhook sees.
func CheckRegistryAllowlist(ociRef string, allowedRegistries []string) error {
	return checkRegistryAllowlist(ociRef, allowedRegistries)
}

func checkRegistryAllowlist(ociRef string, allowedRegistries []string) error {
	if len(allowedRegistries) == 0 {
		return nil // no restriction
	}
	for _, allowed := range allowedRegistries {
		// Ensure we match on a path boundary to prevent "gcr.io/my" from
		// matching "gcr.io/my-evil-org/image". Append "/" if the allowed
		// prefix doesn't already end with one, then check for exact match
		// or prefix+"/".
		if ociRef == allowed || strings.HasPrefix(ociRef, allowed+"/") ||
			(strings.HasSuffix(allowed, "/") && strings.HasPrefix(ociRef, allowed)) {
			return nil
		}
	}
	return fmt.Errorf("image %q does not match any allowed registry prefix: %v", ociRef, allowedRegistries)
}

// SetupAgentRunWebhook registers the AgentRun validating webhook with the manager.
func SetupAgentRunWebhook(mgr ctrl.Manager, allowedRegistries []string) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	mgr.GetWebhookServer().Register("/validate-agentorca-agentorca-io-v1alpha1-agentrun",
		&admission.Webhook{
			Handler: &AgentRunValidator{
				Client:            mgr.GetClient(),
				Decoder:           decoder,
				AllowedRegistries: allowedRegistries,
			},
		},
	)
	return nil
}

// SetupAgentWebhook registers the Agent validating webhook with the manager.
func SetupAgentWebhook(mgr ctrl.Manager, allowedRegistries []string) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	mgr.GetWebhookServer().Register("/validate-agentorca-agentorca-io-v1alpha1-agent",
		&admission.Webhook{
			Handler: &AgentValidator{
				Client:            mgr.GetClient(),
				Decoder:           decoder,
				AllowedRegistries: allowedRegistries,
			},
		},
	)
	return nil
}

// SetupToolWebhook registers the Tool validating webhook with the manager.
func SetupToolWebhook(mgr ctrl.Manager, allowedRegistries []string) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	mgr.GetWebhookServer().Register("/validate-agentorca-agentorca-io-v1alpha1-tool",
		&admission.Webhook{
			Handler: &ToolValidator{
				Client:            mgr.GetClient(),
				Decoder:           decoder,
				AllowedRegistries: allowedRegistries,
			},
		},
	)
	return nil
}

// Ensure the runtime import is referenced to satisfy the Go import checker.
var _ runtime.Object

// githubRepoPattern is an "owner/name" GitHub repository slug.
var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)

// validateSandboxGitHub checks spec.runtime.sandbox.github: the operator's
// GitHub App token is scoped to one installation, so every repository must be
// a slug under one owner.
func validateSandboxGitHub(gh *agentorcav1alpha1.SandboxGitHub) error {
	owner := ""
	for i, repo := range gh.Repositories {
		if !githubRepoPattern.MatchString(repo) || strings.HasSuffix(repo, ".git") {
			return fmt.Errorf("spec.runtime.sandbox.github.repositories[%d] %q is not an owner/name repository", i, repo)
		}
		o, _, _ := strings.Cut(repo, "/")
		if owner == "" {
			owner = o
		} else if !strings.EqualFold(o, owner) {
			return fmt.Errorf("spec.runtime.sandbox.github.repositories must share one owner (the GitHub App installation's account): "+
				"%q and %q differ", owner, o)
		}
	}
	return nil
}

// validateSandboxClass checks that the SandboxClass exists and is available
// to the Agent's namespace.
func (v *AgentValidator) validateSandboxClass(ctx context.Context, field, name string, ns *corev1.Namespace) error {
	var class agentorcav1alpha1.SandboxClass
	if err := v.Client.Get(ctx, client.ObjectKey{Name: name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%s: SandboxClass %q not found", field, name)
		}
		return fmt.Errorf("looking up SandboxClass %q: %w", name, err)
	}
	if class.Spec.NamespaceSelector == nil {
		return nil
	}
	selector, err := metav1.LabelSelectorAsSelector(class.Spec.NamespaceSelector)
	if err != nil {
		return fmt.Errorf("SandboxClass %q has an invalid namespaceSelector: %w", name, err)
	}
	if !selector.Matches(labels.Set(ns.Labels)) {
		return fmt.Errorf("%s: SandboxClass %q is not available to namespace %q", field, name, ns.Name)
	}
	return nil
}

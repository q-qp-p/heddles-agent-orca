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
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
	"github.com/heddles/agent-orca/internal/podbuilder"
	"github.com/heddles/agent-orca/internal/security"
)

// Sandbox mode runs the agent process in an OpenShell sandbox instead of the
// split-pod agent pod. Everything else about split-pod topology is unchanged:
// the router pod, its Service, NetworkPolicy, RBAC, config and token Secret.
// The sandbox lives in the gateway's namespace, so the router is addressed by
// its fully qualified Service name and admits the run's supervisor pod.

// sandboxRouterHost is the router Service hostname as seen from the sandbox
// namespace. It is fully qualified so it does not depend on the supervisor's
// DNS search path, and the sandbox policy pins egress to exactly this name.
func (r *AgentRunReconciler) sandboxRouterHost(run *agentorcav1alpha1.AgentRun) string {
	domain := r.ClusterDomain
	if domain == "" {
		domain = "cluster.local"
	}
	return "agentorca-router-" + run.Name + "." + run.Namespace + ".svc." + domain
}

// ensureSandbox creates the run's sandbox for the current attempt, or returns
// it if it already exists.
func (r *AgentRunReconciler) ensureSandbox(
	ctx context.Context,
	run *agentorcav1alpha1.AgentRun,
	agent *agentorcav1alpha1.Agent,
	tokenSecretName string,
	routerBaseURL string,
) (*osv1.Sandbox, error) {
	class, err := r.sandboxClass(ctx, run, agent)
	if err != nil {
		return nil, err
	}
	// From here on the Agent includes its class and egress presets.
	agent, placement := openshell.ApplyClass(agent, class, r.SandboxPlacement)

	// The token Secret holds OPENAI_BASE_URL and the per-run router credential.
	// A pod would read it through envFrom; the sandbox gets it as plain env.
	var tokenSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: tokenSecretName, Namespace: run.Namespace}, &tokenSecret); err != nil {
		return nil, fmt.Errorf("reading token secret: %w", err)
	}

	env := map[string]string{}
	for _, e := range agentRunEnv(run, agent) {
		env[e.Name] = e.Value
	}
	for _, e := range podbuilder.FrameworkEnvVars(agent.Spec.Runtime.Framework, routerBaseURL) {
		env[e.Name] = e.Value
	}
	for k, v := range tokenSecret.Data {
		env[k] = string(v)
	}
	maps.Copy(env, tokenSecret.StringData)

	tools, providers, err := r.ensureSandboxTools(ctx, run, agent, env)
	if err != nil {
		return nil, err
	}
	git, err := r.ensureSandboxGit(ctx, run, agent, env)
	if err != nil {
		return nil, err
	}
	if git != nil {
		providers = append(providers, git.Provider)
	}

	spec := openshell.BuildSpec(openshell.SpecInput{
		Agent:      agent,
		RouterHost: r.sandboxRouterHost(run),
		Env:        env,
		Tools:      tools,
		Providers:  providers,
		Placement:  placement,
		Git:        git,
	})
	name := openshell.SandboxName(run.Namespace, run.Name, run.Status.RestartCount)
	return r.Sandboxes.Create(ctx, name, spec, sandboxRunLabels(run))
}

// sandboxRunLabels identify a run's sandboxes and providers on the gateway.
func sandboxRunLabels(run *agentorcav1alpha1.AgentRun) map[string]string {
	if workflow := run.Labels[labelWorkflow]; workflow != "" && run.Spec.WorkflowSandbox != nil {
		return map[string]string{
			openshell.LabelWorkflowName:      workflow,
			openshell.LabelWorkflowNamespace: run.Namespace,
		}
	}
	return map[string]string{
		openshell.LabelRunName:      security.SafeLabelValue(run.Name),
		openshell.LabelRunNamespace: run.Namespace,
	}
}

// ensureSandboxTools resolves what the sandbox may do with the Agent's tools:
// the endpoints it may reach directly, and the static credentials it may use
// there. Each credential is registered as a provider the sandbox attaches;
// the workload sees only placeholders, which the supervisor resolves on
// requests to the tool's own endpoint. It adds the non-secret credential
// descriptor to env and returns the endpoints and provider names.
func (r *AgentRunReconciler) ensureSandboxTools(
	ctx context.Context,
	run *agentorcav1alpha1.AgentRun,
	agent *agentorcav1alpha1.Agent,
	env map[string]string,
) ([]openshell.ToolEndpoint, []string, error) {
	return ensureSandboxTools(ctx, r.Client, r.Sandboxes, run, agent, env)
}

// ensureSandboxTools is shared by the AgentRun and workflow controllers. A
// shared workflow uses one provider owner across its AgentRuns, so tools and
// dynamic steps can be merged before the sandbox is created. A fresh step uses
// its AgentRun name, matching its dedicated policy and cleanup.
func ensureSandboxTools(
	ctx context.Context,
	cl client.Client,
	gw openshell.Gateway,
	run *agentorcav1alpha1.AgentRun,
	agent *agentorcav1alpha1.Agent,
	env map[string]string,
) ([]openshell.ToolEndpoint, []string, error) {
	sb := agent.Spec.Runtime.Sandbox
	if !sb.InheritsToolEgress() {
		return nil, nil, nil
	}
	var excluded []string
	if sb.Tools != nil {
		excluded = sb.Tools.ExcludeTools
	}

	var eps []openshell.ToolEndpoint
	var creds []*openshell.ToolCredential
	seenSources := map[string]bool{}
	for _, name := range agent.Spec.Tools {
		if builtinTools[name] || slices.Contains(excluded, name) {
			continue
		}
		var tool agentorcav1alpha1.Tool
		if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: run.Namespace}, &tool); err != nil {
			return nil, nil, fmt.Errorf("reading tool %q for sandbox egress: %w", name, err)
		}
		source := name
		var serverEgress []agentorcav1alpha1.EgressRule
		if serverName := tool.Labels[LabelMCPServer]; serverName != "" {
			source = serverName
			var server agentorcav1alpha1.MCPServer
			if err := cl.Get(ctx, types.NamespacedName{Name: serverName, Namespace: run.Namespace}, &server); err == nil {
				serverEgress = server.Spec.NetworkEgress
			} else if !apierrors.IsNotFound(err) {
				return nil, nil, fmt.Errorf("reading MCPServer %q for sandbox egress: %w", serverName, err)
			}
		}
		eps = append(eps, openshell.ToolEndpoints(&tool, serverEgress...)...)
		if !seenSources[source] {
			if cred := openshell.ToolCredentials(&tool, source); cred != nil {
				seenSources[source] = true
				creds = append(creds, cred)
			}
		}
	}
	eps = openshell.FilterToolEndpoints(sb, eps)

	ownerName := run.Name
	if ref := run.Spec.WorkflowSandbox; ref != nil {
		if !ref.Fresh {
			ownerName = "workflow-" + run.Labels[labelWorkflow]
		} else if ref.ProviderOwner != "" {
			ownerName = ref.ProviderOwner
		}
	}
	providerName := func(c *openshell.ToolCredential) string {
		return openshell.ProviderName(run.Namespace, ownerName, c.Source)
	}
	eps, used, err := openshell.BindCredentials(eps, creds, providerName)
	if err != nil {
		return nil, nil, err
	}

	providers := make([]string, 0, len(used))
	for _, cred := range used {
		values := map[string]string{}
		for _, k := range cred.Keys {
			var secret corev1.Secret
			if err := cl.Get(ctx, types.NamespacedName{Name: k.SecretName, Namespace: run.Namespace}, &secret); err != nil {
				return nil, nil, fmt.Errorf("reading secret %q for tool %q: %w", k.SecretName, cred.Tool, err)
			}
			v, ok := secret.Data[k.SecretKey]
			if !ok {
				return nil, nil, fmt.Errorf("secret %q has no key %q (tool %q)", k.SecretName, k.SecretKey, cred.Tool)
			}
			// Match the router, which trims the mounted secret file.
			values[k.EnvVar] = strings.TrimSpace(string(v))
		}
		if err := gw.EnsureProfile(ctx, openshell.Profile(run.Namespace, cred)); err != nil {
			return nil, nil, err
		}
		if err := gw.EnsureProvider(ctx, openshell.Provider(run.Namespace, ownerName, cred, values, sandboxRunLabels(run))); err != nil {
			return nil, nil, err
		}
		providers = append(providers, providerName(cred))
	}
	if desc := openshell.DescribeCredentials(used); desc != "" {
		env[openshell.CredentialsEnvVar] = desc
	}
	return eps, providers, nil
}

// ensureRouterSandboxIngress admits the sandbox's supervisor to the router pod.
// It needs the sandbox ID, which identifies exactly this sandbox's supervisor,
// so it is a no-op until the gateway reports one — until then the router stays
// closed to the sandbox namespace. A retry gets a new sandbox, so the policy is
// updated in place rather than only created.
func (r *AgentRunReconciler) ensureRouterSandboxIngress(ctx context.Context, run *agentorcav1alpha1.AgentRun, sb *osv1.Sandbox) error {
	if sb.ID == "" {
		return nil
	}
	want := security.BuildRouterSandboxIngressPolicy(run, run.Namespace, r.Sandboxes.SandboxNamespace(), map[string]string{
		openshell.SupervisorComponentLabel: openshell.SupervisorComponentValue,
		openshell.SandboxIDLabel:           sb.ID,
	})
	np := &networkingv1.NetworkPolicy{}
	np.Name = want.Name
	np.Namespace = want.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = want.Labels
		np.Spec = want.Spec
		return controllerutil.SetControllerReference(run, np, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensuring router sandbox ingress policy: %w", err)
	}
	return nil
}

// checkSandboxProgress is checkProgress for sandboxed runs: it polls the
// gateway and advances the run when the sandbox's main process exits.
func (r *AgentRunReconciler) checkSandboxProgress(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if r.Sandboxes == nil {
		return r.failRun(ctx, run, "sandbox runtime is no longer configured on the operator")
	}

	sb, err := r.Sandboxes.Get(ctx, run.Status.SandboxName)
	if err != nil {
		if osv1.IsNotFound(err) {
			return r.handlePodFailure(ctx, run, agent, "sandbox not found")
		}
		// The gateway being briefly unreachable must not fail a running agent.
		logger.Error(err, "reading sandbox status", "sandbox", run.Status.SandboxName)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if err := r.ensureRouterSandboxIngress(ctx, run, sb); err != nil {
		return ctrl.Result{}, err
	}
	r.refreshSandboxGit(ctx, run, agent)
	r.recordSandboxDecisions(ctx, run, sb)
	r.reconcileEgressApprovals(ctx, run, agent, sb)

	outcome, reason := openshell.Observe(sb)
	switch outcome {
	case openshell.OutcomeSucceeded:
		return r.handleRunSuccess(ctx, run, func(ctx context.Context) string {
			out, err := r.Sandboxes.MainProcessLogs(ctx, sb)
			if err != nil {
				logger.Error(err, "collecting sandbox output", "sandbox", sb.Name)
				return "(log collection failed)"
			}
			return out
		})
	case openshell.OutcomeFailed:
		// The sandbox (and its workload logs) is deleted on retry, so keep the
		// tail of the agent's output in the failure reason, as for agent pods.
		if out, err := r.Sandboxes.MainProcessLogs(ctx, sb); err == nil {
			if tail := lastLines(strings.TrimSpace(out), 10); tail != "" {
				reason += "\n[agent] " + truncate(tail, 1024)
			}
		}
		return r.handlePodFailure(ctx, run, agent, reason)
	default:
		logger.V(2).Info("sandbox still running", "sandbox", sb.Name, "phase", sb.Status.Phase)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

// deleteRunProviders revokes the run's tool credentials on the gateway. Unlike
// the sandbox, providers survive retries, so this runs only when the run ends.
// It reports retry=true while a provider is still attached to a sandbox the
// gateway is deleting (deletion is asynchronous), so the caller requeues
// instead of leaking the credential. Other failures are logged and recorded as
// an event: a gateway outage must not block a run's finalizer.
func (r *AgentRunReconciler) deleteRunProviders(ctx context.Context, run *agentorcav1alpha1.AgentRun) (retry bool) {
	if run.Spec.WorkflowSandbox != nil {
		return false // workflow credential providers live until the workflow ends
	}
	if r.Sandboxes == nil || run.Status.RouterPodName == "" {
		return false // not a split-pod run, so it never had a sandbox
	}
	err := r.Sandboxes.DeleteProviders(ctx, sandboxRunLabels(run))
	if err == nil {
		return false
	}
	if osv1.IsConflict(err) {
		log.FromContext(ctx).V(1).Info("sandbox credential providers still attached; retrying", "run", run.Name)
		return true
	}
	log.FromContext(ctx).Error(err, "deleting sandbox providers", "run", run.Name)
	if r.Recorder != nil {
		r.Recorder.Eventf(run, corev1.EventTypeWarning, "SandboxProviderDeleteFailed",
			"deleting sandbox credential providers: %v", err)
	}
	return false
}

// deleteRunSandbox deletes the run's sandbox. Failures are logged and recorded
// as an event rather than returned: a gateway outage must not block a run's
// finalizer, and the sandbox carries labels naming its run for manual cleanup.
func (r *AgentRunReconciler) deleteRunSandbox(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	r.deleteShellSandbox(ctx, run)
	if ref := run.Spec.WorkflowSandbox; ref != nil {
		if !ref.Fresh || run.Status.ContinuationRunRef != "" {
			return // shared sandboxes and fresh clarification chains stay alive
		}
	}
	if run.Status.SandboxName == "" || r.Sandboxes == nil {
		return
	}
	if err := r.Sandboxes.Delete(ctx, run.Status.SandboxName); err != nil {
		log.FromContext(ctx).Error(err, "deleting sandbox", "sandbox", run.Status.SandboxName)
		if r.Recorder != nil {
			r.Recorder.Eventf(run, corev1.EventTypeWarning, "SandboxDeleteFailed",
				"deleting sandbox %s: %v", run.Status.SandboxName, err)
		}
	}
}

// lastLines returns the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// sandboxClass returns the Agent's SandboxClass, or nil when it names none.
func (r *AgentRunReconciler) sandboxClass(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent) (*agentorcav1alpha1.SandboxClass, error) {
	return loadSandboxClass(ctx, r.Client, run.Namespace, agent.Spec.Runtime.Sandbox.ClassRef)
}

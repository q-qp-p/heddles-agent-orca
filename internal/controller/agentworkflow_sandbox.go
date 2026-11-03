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
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
	agentwebhook "github.com/heddles/agent-orca/internal/webhook"
)

const (
	workflowSandboxFinalizer = "agentorca.io/workflow-sandbox-cleanup"
	workflowCheckoutJobID    = "checkout"
	workflowCheckoutDir      = "/sandbox/repo"
	workflowSandboxRetry     = 15 * time.Second
	workflowIdleExit         = "the workflow sandbox's idle main process exited"
)

// The checkout prints only its commit on success. Git diagnostics stay in the
// job output on failure. The job is idempotent, so a controller restart can
// safely poll or retry the start without cloning over an existing worktree.
const workflowCheckoutScript = `set -eu
if [ -e "$CHECKOUT_DIR" ]; then
  echo "checkout directory already exists" >&2
  exit 2
fi
git clone --quiet --no-checkout "$REPO_URL" "$CHECKOUT_DIR"
ref=${CHECKOUT_REF:-HEAD}
git -C "$CHECKOUT_DIR" fetch --quiet origin "$ref"
if [ -n "$CHECKOUT_BRANCH" ] && [ "${CHECKOUT_RESUME:-}" = 1 ] &&
   git -C "$CHECKOUT_DIR" ls-remote --exit-code --heads origin "$CHECKOUT_BRANCH" >/dev/null 2>&1; then
  git -C "$CHECKOUT_DIR" fetch --quiet origin "$CHECKOUT_BRANCH"
  git -C "$CHECKOUT_DIR" checkout --quiet -B "$CHECKOUT_BRANCH" FETCH_HEAD
elif [ -n "$CHECKOUT_BRANCH" ]; then
  git -C "$CHECKOUT_DIR" checkout --quiet -B "$CHECKOUT_BRANCH" FETCH_HEAD
else
  git -C "$CHECKOUT_DIR" checkout --quiet --detach FETCH_HEAD
fi
git -C "$CHECKOUT_DIR" rev-parse HEAD
`

var (
	checkoutSHAPattern  = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	workflowRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// validateWorkflowSandbox checks relationships the step-local CRD rules cannot
// see: a step's placement needs a workflow sandbox, and a shared container runs
// in the sandbox's existing image rather than pulling a different one.
func validateWorkflowSandbox(spec *agentorcav1alpha1.AgentWorkflowSpec) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if spec.Sandbox == nil {
		for _, step := range spec.Steps {
			if step.Sandbox != "" {
				add("step %q sets sandbox but spec.sandbox is not configured", step.Name)
			}
		}
		return problems
	}
	if spec.Sandbox.Image == "" {
		add("spec.sandbox.image is required")
	}
	if !workflowRepoPattern.MatchString(spec.Sandbox.Repo) {
		add("spec.sandbox.repo must be a GitHub repository in owner/name form")
	}
	if spec.AdaptivePolicy != nil && len(spec.AdaptivePolicy.AllowedAgentRefs) == 0 {
		add("spec.sandbox requires adaptivePolicy.allowedAgentRefs so dynamic agents can be included in the shared policy")
	}
	for _, step := range spec.Steps {
		if step.Sandbox != "" && step.Sandbox != agentorcav1alpha1.WorkflowStepSandboxShared && step.Sandbox != agentorcav1alpha1.WorkflowStepSandboxFresh {
			add("step %q has unknown sandbox placement %q", step.Name, step.Sandbox)
		}
		if stepType(&step) == agentorcav1alpha1.WorkflowStepTypeApproval {
			if step.Sandbox != "" {
				add("step %q is an approval step and cannot set sandbox", step.Name)
			}
			continue
		}
		if stepType(&step) != agentorcav1alpha1.WorkflowStepTypeContainer || step.Container == nil {
			continue
		}
		c := step.Container
		if len(c.Command) == 0 {
			add("step %q runs in a sandbox and needs container.command (an exec has no image entrypoint)", step.Name)
		}
		if c.Resources != nil || c.RunAsUser != nil {
			add("step %q runs in a sandbox; container.resources and runAsUser apply only to pod steps (use the SandboxClass)", step.Name)
		}
		if step.Sandbox != agentorcav1alpha1.WorkflowStepSandboxFresh && c.Image != spec.Sandbox.Image {
			add("step %q uses the shared sandbox, so container.image must equal spec.sandbox.image", step.Name)
		}
	}
	return problems
}

func workflowSandboxLabels(wf *agentorcav1alpha1.AgentWorkflow) map[string]string {
	return map[string]string{
		openshell.LabelWorkflowName:      wf.Name,
		openshell.LabelWorkflowNamespace: wf.Namespace,
	}
}

// workflowSandboxAgent adapts the workflow's sandbox to the same class,
// placement, resource and policy compilation used by AgentRun sandboxes. Its
// main process is deliberately idle: steps are detached jobs, not a stream tied
// to the lifetime of the operator or one gateway connection.
func workflowSandboxAgent(wf *agentorcav1alpha1.AgentWorkflow) *agentorcav1alpha1.Agent {
	return workflowSandboxAgentForImage(wf, wf.Spec.Sandbox.Image)
}

func workflowSandboxAgentForImage(wf *agentorcav1alpha1.AgentWorkflow, image string) *agentorcav1alpha1.Agent {
	sb := wf.Spec.Sandbox
	return &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{
		Runtime: agentorcav1alpha1.AgentRuntime{
			OCIRef:  image,
			Command: []string{"sh", "-c", `while :; do sleep 3600; done`},
			Sandbox: &agentorcav1alpha1.SandboxSpec{
				Provider:      agentorcav1alpha1.SandboxProviderOpenShell,
				ClassRef:      sb.ClassRef,
				EgressPresets: []agentorcav1alpha1.SandboxEgressPreset{agentorcav1alpha1.SandboxEgressPresetGitHubRead},
			},
		},
	}}
}

func workflowGitProviderName(wf *agentorcav1alpha1.AgentWorkflow) string {
	return openshell.GitProviderName(wf.Namespace, "workflow-"+wf.Name)
}

// workflowGitHub is the git access the workflow's sandboxes share: its own
// repository (with push when spec.sandbox.push) plus whatever the class's
// githubApp credentials grant.
func workflowGitHub(wf *agentorcav1alpha1.AgentWorkflow, classGitHub *agentorcav1alpha1.SandboxGitHub) *agentorcav1alpha1.SandboxGitHub {
	return openshell.MergeGitHub(
		&agentorcav1alpha1.SandboxGitHub{Repositories: []string{wf.Spec.Sandbox.Repo}, Push: wf.Spec.Sandbox.Push},
		classGitHub)
}

// ensureWorkflowGit maintains the optional credential-bound GitHub access. A
// workflow without the operator's GitHub App can still clone a public repo;
// GIT_TERMINAL_PROMPT=0 makes an accidental private repo fail instead of wait.
// A class that grants GitHub access (classGitHub, from its githubApp
// credentials) needs the App: the workflow does not start without it.
func (r *AgentWorkflowReconciler) ensureWorkflowGit(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, classGitHub *agentorcav1alpha1.SandboxGitHub) (*openshell.GitAccess, error) {
	if r.GitHubApp == nil {
		if classGitHub != nil {
			return nil, errNoGitHubApp
		}
		return nil, nil
	}
	gh := workflowGitHub(wf, classGitHub)
	access := &openshell.GitAccess{Repositories: gh.Repositories, Push: gh.Push, Provider: workflowGitProviderName(wf)}
	status := wf.Status.Sandbox
	if status != nil && status.GitTokenExpiry != nil && time.Until(status.GitTokenExpiry.Time) > gitTokenRefreshBefore {
		return access, nil
	}
	if err := r.Sandboxes.EnsureProfile(ctx, openshell.GitProfile(wf.Namespace)); err != nil {
		return nil, err
	}
	tok, err := r.GitHubApp.InstallationToken(ctx, gitTokenRequest(gh))
	if err != nil {
		return nil, fmt.Errorf("minting the workflow's GitHub token: %w", err)
	}
	labels := workflowSandboxLabels(wf)
	name := "workflow-" + wf.Name
	if err := r.Sandboxes.EnsureProvider(ctx, openshell.GitProvider(wf.Namespace, name, tok.Value, labels)); err != nil {
		return nil, fmt.Errorf("storing the workflow's GitHub token: %w", err)
	}
	if wf.Status.Sandbox == nil {
		wf.Status.Sandbox = &agentorcav1alpha1.WorkflowSandboxStatus{}
	}
	expiry := metav1.NewTime(tok.ExpiresAt)
	wf.Status.Sandbox.GitTokenExpiry = &expiry
	return access, nil
}

// ensureWorkflowSandbox creates and checks out the shared sandbox. Its status
// changes are in-memory; the main reconcile persists them with the step state.
// ready is true only when steps may use the checkout.
func (r *AgentWorkflowReconciler) ensureWorkflowSandbox(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) (ready bool, result ctrl.Result, err error) {
	if wf.Spec.Sandbox == nil {
		return true, ctrl.Result{}, nil
	}
	if wf.Status.Sandbox == nil {
		wf.Status.Sandbox = &agentorcav1alpha1.WorkflowSandboxStatus{}
	}
	if r.Sandboxes == nil {
		r.failWorkflowSandbox(ctx, wf, "spec.sandbox is set but the operator has no OpenShell gateway configured")
		return false, ctrl.Result{}, nil
	}
	if err := agentwebhook.CheckRegistryAllowlist(wf.Spec.Sandbox.Image, r.AllowedRegistries); err != nil {
		r.failWorkflowSandbox(ctx, wf, fmt.Sprintf("sandbox image rejected: %v", err))
		return false, ctrl.Result{}, nil
	}

	class, err := loadSandboxClass(ctx, r.Client, wf.Namespace, wf.Spec.Sandbox.ClassRef)
	if err != nil {
		if errors.Is(err, errSandboxClassUnusable) {
			r.failWorkflowSandbox(ctx, wf, err.Error())
			return false, ctrl.Result{}, nil
		}
		return false, ctrl.Result{}, err
	}
	agent, placement := openshell.ApplyClass(workflowSandboxAgent(wf), class, r.SandboxPlacement)
	tools, toolProviders, err := r.mergeWorkflowAgentPolicies(ctx, wf, agent)
	if err != nil {
		if errors.Is(err, errWorkflowAgentUnusable) || errors.Is(err, errSandboxClassUnusable) {
			r.failWorkflowSandbox(ctx, wf, err.Error())
			return false, ctrl.Result{}, nil
		}
		return false, ctrl.Result{}, err
	}
	git, err := r.ensureWorkflowGit(ctx, wf, agent.Spec.Runtime.Sandbox.GitHub)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	status := wf.Status.Sandbox // ensureWorkflowGit may have allocated it.
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	providers := toolProviders
	if git != nil {
		maps.Copy(env, openshell.GitEnv())
		providers = append(providers, git.Provider)
	}
	var routerHosts []string
	if len(workflowPolicyAgents(wf)) > 0 {
		routerHosts = r.workflowSharedRouterHosts(wf)
	}
	spec := openshell.BuildSpec(openshell.SpecInput{
		Agent: agent, ExtraRouterHosts: routerHosts, Env: env,
		Tools: tools, Providers: providers, Placement: placement, Git: git,
	})
	name := openshell.WorkflowSandboxName(wf.Namespace, wf.Name)
	status.Name = name

	var sb *osv1.Sandbox
	if status.ID == "" && status.Phase == "" {
		markWorkflowStarted(wf, metav1.Now())
		sb, err = r.Sandboxes.Create(ctx, name, spec, workflowSandboxLabels(wf))
		if err != nil {
			if osv1.IsInvalidArgument(err) || osv1.IsPermissionDenied(err) {
				r.failWorkflowSandbox(ctx, wf, fmt.Sprintf("creating workflow sandbox: %v", err))
				return false, ctrl.Result{}, nil
			}
			return false, ctrl.Result{}, fmt.Errorf("creating workflow sandbox: %w", err)
		}
	} else {
		sb, err = r.Sandboxes.Get(ctx, name)
		if err != nil {
			if osv1.IsNotFound(err) {
				r.failWorkflowSandbox(ctx, wf, "the workflow sandbox was deleted")
				return false, ctrl.Result{}, nil
			}
			// A gateway restart must not fail a checkout or a running step. Its
			// detached state remains in the sandbox and can be polled next time.
			log.FromContext(ctx).Error(err, "reading the workflow sandbox", "sandbox", name)
			return false, ctrl.Result{RequeueAfter: workflowSandboxRetry}, nil
		}
	}
	status.ID = sb.ID
	if status.CheckoutSHA != "" {
		if sb.Status.Phase != osv1.SandboxReady {
			if outcome, reason := openshell.Observe(sb); outcome != openshell.OutcomeRunning {
				if reason == "" {
					reason = workflowIdleExit
				}
				r.failWorkflowSandbox(ctx, wf, reason)
				return false, ctrl.Result{}, nil
			}
			return false, ctrl.Result{RequeueAfter: workflowPollInterval}, nil
		}
		status.Phase = agentorcav1alpha1.WorkflowSandboxPhaseReady
		return true, ctrl.Result{}, nil
	}
	status.Phase = agentorcav1alpha1.WorkflowSandboxPhaseProvisioning
	if sb.Status.Phase != osv1.SandboxReady {
		if outcome, reason := openshell.Observe(sb); outcome != openshell.OutcomeRunning {
			if reason == "" {
				reason = workflowIdleExit
			}
			r.failWorkflowSandbox(ctx, wf, reason)
			return false, ctrl.Result{}, nil
		}
		return false, ctrl.Result{RequeueAfter: workflowPollInterval}, nil
	}
	return r.ensureWorkflowCheckout(ctx, wf)
}

// workflowResumed reports whether a step has run more than once, which can only
// have happened if the workflow's sandbox was provisioned again after a retry
// (agentworkflow_retry.go). That sandbox continues from the pushed branch.
func workflowResumed(wf *agentorcav1alpha1.AgentWorkflow) bool {
	for i := range wf.Status.Steps {
		if wf.Status.Steps[i].NameGeneration() > 0 {
			return true
		}
	}
	return false
}

func (r *AgentWorkflowReconciler) ensureWorkflowCheckout(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) (bool, ctrl.Result, error) {
	ready, result, failure, err := r.ensureCheckout(ctx, wf.Status.Sandbox, wf.Spec.Sandbox.Repo, wf.Spec.Sandbox.Ref, wf.Spec.Sandbox.Branch, workflowResumed(wf))
	if failure != "" {
		r.failWorkflowSandbox(ctx, wf, failure)
	}
	return ready, result, err
}

// ensureCheckout advances one sandbox's checkout. Failure is returned
// separately because it belongs to the workflow for the shared sandbox, but to
// an individual step for a fresh sandbox.
func (r *AgentWorkflowReconciler) ensureCheckout(
	ctx context.Context,
	status *agentorcav1alpha1.WorkflowSandboxStatus,
	repo, ref, branch string,
	resume bool,
) (bool, ctrl.Result, string, error) {
	st, err := openshell.PollJob(ctx, r.Sandboxes, status.Name, workflowCheckoutJobID)
	if err != nil {
		log.FromContext(ctx).Error(err, "polling a workflow checkout", "sandbox", status.Name)
		return false, ctrl.Result{RequeueAfter: workflowSandboxRetry}, "", nil
	}
	switch st.Phase {
	case openshell.JobMissing:
		env := map[string]string{
			"REPO_URL":        "https://github.com/" + repo + ".git",
			"CHECKOUT_REF":    ref,
			"CHECKOUT_BRANCH": branch,
			"CHECKOUT_DIR":    workflowCheckoutDir,
		}
		if resume {
			// Continue from the branch's pushed work rather than the base ref.
			env["CHECKOUT_RESUME"] = "1"
		}
		if err := openshell.StartJob(ctx, r.Sandboxes, status.Name, workflowCheckoutJobID, workflowCheckoutScript, env, ""); err != nil {
			log.FromContext(ctx).Error(err, "starting a workflow checkout", "sandbox", status.Name)
			return false, ctrl.Result{RequeueAfter: workflowSandboxRetry}, "", nil
		}
		return false, ctrl.Result{RequeueAfter: workflowPollInterval}, "", nil
	case openshell.JobRunning:
		return false, ctrl.Result{RequeueAfter: workflowPollInterval}, "", nil
	case openshell.JobLost:
		return false, ctrl.Result{}, "the checkout process was lost before it reported an exit code", nil
	case openshell.JobExited:
		if st.ExitCode != 0 {
			return false, ctrl.Result{}, fmt.Sprintf("checking out %s failed (exit %d): %s", repo, st.ExitCode, strings.TrimSpace(st.Output)), nil
		}
		lines := strings.Fields(st.Output)
		if len(lines) == 0 {
			return false, ctrl.Result{}, "the checkout succeeded without reporting a commit", nil
		}
		sha := lines[len(lines)-1]
		if !checkoutSHAPattern.MatchString(sha) {
			return false, ctrl.Result{}, fmt.Sprintf("the checkout reported an invalid commit %q", sha), nil
		}
		status.CheckoutSHA = sha
		status.Phase = agentorcav1alpha1.WorkflowSandboxPhaseReady
		return true, ctrl.Result{}, "", nil
	default:
		return false, ctrl.Result{}, "", fmt.Errorf("unknown checkout job phase %q", st.Phase)
	}
}

func (r *AgentWorkflowReconciler) failWorkflowSandbox(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, reason string) {
	if wf.Status.Sandbox == nil {
		wf.Status.Sandbox = &agentorcav1alpha1.WorkflowSandboxStatus{}
	}
	wf.Status.Sandbox.Phase = agentorcav1alpha1.WorkflowSandboxPhaseFailed
	r.failWorkflow(ctx, wf, reason)
}

func workflowStepJobID(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, iteration int32) string {
	return openshell.WorkflowJobName(wf.Namespace, fmt.Sprintf("%s-%s", wf.Name, iterStepName(step.Name, iteration)))
}

func (r *AgentWorkflowReconciler) startSandboxContainerStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) error {
	if step.Sandbox == agentorcav1alpha1.WorkflowStepSandboxFresh {
		return r.startFreshSandboxContainerStep(ctx, wf, step, ss)
	}
	if err := r.startSandboxContainerJob(ctx, wf, step, ss.NameGeneration(), wf.Status.Sandbox.Name); err != nil {
		return err
	}
	now := metav1.Now()
	ss.Phase = agentorcav1alpha1.WorkflowStepPhaseRunning
	ss.JobID = workflowStepJobID(wf, step, ss.NameGeneration())
	ss.StartTime = &now
	markWorkflowStarted(wf, now)
	return nil
}

func (r *AgentWorkflowReconciler) startSandboxContainerJob(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, iteration int32, sandboxName string) error {
	c := step.Container
	allStatuses := append(wf.Status.Steps, wf.Status.DynamicStepStatuses...)
	env := map[string]string{
		"AGENTORC_WORKFLOW":     wf.Name,
		"AGENTORC_STEP":         step.Name,
		"AGENTORC_CHECKOUT_SHA": wf.Status.Sandbox.CheckoutSHA,
	}
	for _, e := range c.Env {
		env[e.Name] = resolveTemplates(e.Value, allStatuses)
	}
	if err := openshell.StartCommandJob(ctx, r.Sandboxes, sandboxName, workflowStepJobID(wf, step, iteration), c.Command, c.Args, env, workflowCheckoutDir); err != nil {
		return fmt.Errorf("starting sandbox job for step %q: %w", step.Name, err)
	}
	return nil
}

func (r *AgentWorkflowReconciler) freshSandboxSpec(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep) (*osv1.SandboxSpec, error) {
	class, err := loadSandboxClass(ctx, r.Client, wf.Namespace, wf.Spec.Sandbox.ClassRef)
	if err != nil {
		return nil, err
	}
	agent, placement := openshell.ApplyClass(workflowSandboxAgentForImage(wf, step.Container.Image), class, r.SandboxPlacement)
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	var git *openshell.GitAccess
	var providers []string
	if r.GitHubApp != nil {
		// Fresh sandboxes stay read-only unless their class grants more: the
		// workflow's own push never reaches them.
		gh := openshell.MergeGitHub(&agentorcav1alpha1.SandboxGitHub{Repositories: []string{wf.Spec.Sandbox.Repo}}, agent.Spec.Runtime.Sandbox.GitHub)
		git = &openshell.GitAccess{Repositories: gh.Repositories, Push: gh.Push, Provider: workflowGitProviderName(wf)}
		maps.Copy(env, openshell.GitEnv())
		providers = append(providers, git.Provider)
	}
	return openshell.BuildSpec(openshell.SpecInput{Agent: agent, Env: env, Providers: providers, Placement: placement, Git: git}), nil
}

func (r *AgentWorkflowReconciler) startFreshSandboxContainerStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) error {
	if err := agentwebhook.CheckRegistryAllowlist(step.Container.Image, r.AllowedRegistries); err != nil {
		finishStep(wf, ss, false, "", fmt.Sprintf("image rejected: %v", err))
		return nil
	}
	spec, err := r.freshSandboxSpec(ctx, wf, step)
	if err != nil {
		if errors.Is(err, errSandboxClassUnusable) {
			finishStep(wf, ss, false, "", err.Error())
			return nil
		}
		return err
	}
	name := openshell.WorkflowStepSandboxName(wf.Namespace, wf.Name, iterStepName(step.Name, ss.NameGeneration()))
	labels := workflowSandboxLabels(wf)
	labels[labelWorkflowStep] = step.Name
	sb, err := r.Sandboxes.Create(ctx, name, spec, labels)
	if err != nil {
		if osv1.IsInvalidArgument(err) || osv1.IsPermissionDenied(err) {
			finishStep(wf, ss, false, "", fmt.Sprintf("creating fresh sandbox: %v", err))
			return nil
		}
		return fmt.Errorf("creating fresh sandbox for step %q: %w", step.Name, err)
	}
	now := metav1.Now()
	ss.Phase = agentorcav1alpha1.WorkflowStepPhaseRunning
	ss.JobID = workflowStepJobID(wf, step, ss.NameGeneration())
	ss.StartTime = &now
	ss.Sandbox = &agentorcav1alpha1.WorkflowSandboxStatus{
		Name: name, ID: sb.ID, Phase: agentorcav1alpha1.WorkflowSandboxPhaseProvisioning,
	}
	markWorkflowStarted(wf, now)
	return nil
}

// ensureFreshStepSandbox advances a fresh sandbox to an exact checkout of the
// shared commit. It does not start the step itself; container and agent steps
// use the same recovery path and then launch their own workload.
func (r *AgentWorkflowReconciler) ensureFreshStepSandbox(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) (bool, ctrl.Result, string, error) {
	status := ss.Sandbox
	sb, err := r.Sandboxes.Get(ctx, status.Name)
	if err != nil {
		if osv1.IsNotFound(err) {
			return false, ctrl.Result{}, "the step's fresh sandbox was deleted", nil
		}
		log.FromContext(ctx).Error(err, "reading a fresh step sandbox", "step", step.Name, "sandbox", status.Name)
		return false, ctrl.Result{RequeueAfter: workflowSandboxRetry}, "", nil
	}
	status.ID = sb.ID
	if status.CheckoutSHA != "" {
		if sb.Status.Phase != osv1.SandboxReady {
			if outcome, reason := openshell.Observe(sb); outcome != openshell.OutcomeRunning {
				if reason == "" {
					reason = "the fresh sandbox's idle main process exited"
				}
				return false, ctrl.Result{}, reason, nil
			}
			return false, ctrl.Result{RequeueAfter: workflowPollInterval}, "", nil
		}
	} else {
		status.Phase = agentorcav1alpha1.WorkflowSandboxPhaseProvisioning
		if sb.Status.Phase != osv1.SandboxReady {
			if outcome, reason := openshell.Observe(sb); outcome != openshell.OutcomeRunning {
				if reason == "" {
					reason = "the fresh sandbox's idle main process exited"
				}
				return false, ctrl.Result{}, reason, nil
			}
			return false, ctrl.Result{RequeueAfter: workflowPollInterval}, "", nil
		}
		ready, result, failure, err := r.ensureCheckout(ctx, status, wf.Spec.Sandbox.Repo, wf.Status.Sandbox.CheckoutSHA, wf.Spec.Sandbox.Branch, false)
		if err != nil || failure != "" || !ready {
			return ready, result, failure, err
		}
	}
	if status.CheckoutSHA != wf.Status.Sandbox.CheckoutSHA {
		return false, ctrl.Result{}, fmt.Sprintf("fresh checkout %s does not match workflow commit %s", status.CheckoutSHA, wf.Status.Sandbox.CheckoutSHA), nil
	}
	status.Phase = agentorcav1alpha1.WorkflowSandboxPhaseReady
	return true, ctrl.Result{}, "", nil
}

func (r *AgentWorkflowReconciler) checkFreshSandboxContainerStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) (ctrl.Result, error) {
	if r.sandboxStepTimedOut(wf, step, ss) {
		r.deleteFreshStepSandbox(ctx, ss)
		finishStep(wf, ss, false, "", "the step timed out")
		return ctrl.Result{}, nil
	}
	ready, result, failure, err := r.ensureFreshStepSandbox(ctx, wf, step, ss)
	if err != nil {
		return result, err
	}
	if failure != "" {
		ss.Sandbox.Phase = agentorcav1alpha1.WorkflowSandboxPhaseFailed
		r.deleteFreshStepSandbox(ctx, ss)
		finishStep(wf, ss, false, "", failure)
		return ctrl.Result{}, nil
	}
	if !ready {
		return result, nil
	}
	if err := r.startSandboxContainerJob(ctx, wf, step, ss.NameGeneration(), ss.Sandbox.Name); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: workflowPollInterval}, nil
}

func (r *AgentWorkflowReconciler) sandboxStepTimedOut(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) bool {
	return ss.StartTime != nil && time.Since(ss.StartTime.Time) >= stepTimeout(wf, step, defaultContainerStepTimeout)
}

func (r *AgentWorkflowReconciler) deleteFreshStepSandbox(ctx context.Context, ss *agentorcav1alpha1.WorkflowStepStatus) {
	if ss.Sandbox == nil || ss.Sandbox.Name == "" || r.Sandboxes == nil {
		return
	}
	if err := r.Sandboxes.Delete(ctx, ss.Sandbox.Name); err != nil {
		log.FromContext(ctx).Error(err, "deleting a fresh step sandbox", "sandbox", ss.Sandbox.Name)
	}
}

func (r *AgentWorkflowReconciler) checkSandboxContainerStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) (ctrl.Result, error) {
	if ss.Sandbox != nil && ss.Sandbox.Phase != agentorcav1alpha1.WorkflowSandboxPhaseReady {
		return r.checkFreshSandboxContainerStep(ctx, wf, step, ss)
	}
	sandboxName := wf.Status.Sandbox.Name
	if ss.Sandbox != nil {
		sandboxName = ss.Sandbox.Name
	}
	st, err := openshell.PollJob(ctx, r.Sandboxes, sandboxName, ss.JobID)
	if err != nil {
		log.FromContext(ctx).Error(err, "polling a sandbox step", "step", step.Name, "job", ss.JobID)
		return ctrl.Result{RequeueAfter: workflowSandboxRetry}, nil
	}
	out := truncateTail(strings.TrimRight(st.Output, "\n"))
	switch st.Phase {
	case openshell.JobRunning:
		if r.sandboxStepTimedOut(wf, step, ss) {
			if err := openshell.StopJob(ctx, r.Sandboxes, sandboxName, ss.JobID); err != nil {
				log.FromContext(ctx).Error(err, "stopping a timed-out sandbox step", "step", step.Name)
			}
			r.deleteFreshStepSandbox(ctx, ss)
			finishStep(wf, ss, false, out, "the step timed out")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: workflowPollInterval}, nil
	case openshell.JobExited:
		if st.ExitCode == 0 {
			finishStep(wf, ss, true, out, "")
		} else {
			finishStep(wf, ss, false, out, fmt.Sprintf("the container exited with code %d", st.ExitCode))
		}
		r.deleteFreshStepSandbox(ctx, ss)
		return ctrl.Result{}, nil
	case openshell.JobLost:
		finishStep(wf, ss, false, out, "the step's process was lost before it reported an exit code")
		r.deleteFreshStepSandbox(ctx, ss)
		return ctrl.Result{}, nil
	case openshell.JobMissing:
		finishStep(wf, ss, false, out, "the step's job is missing from the workflow sandbox")
		r.deleteFreshStepSandbox(ctx, ss)
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, fmt.Errorf("unknown job phase %q for step %q", st.Phase, step.Name)
	}
}

// cleanupWorkflowSandbox deletes gateway resources, which cannot have
// Kubernetes owner references. It is safe to repeat after a crash and covers
// both the shared sandbox and any fresh step sandboxes.
func (r *AgentWorkflowReconciler) cleanupWorkflowSandbox(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) (ctrl.Result, error) {
	if r.Sandboxes == nil {
		return ctrl.Result{}, nil
	}
	names := map[string]bool{}
	if wf.Spec.Sandbox != nil {
		names[openshell.WorkflowSandboxName(wf.Namespace, wf.Name)] = true
	}
	if wf.Status.Sandbox != nil && wf.Status.Sandbox.Name != "" {
		names[wf.Status.Sandbox.Name] = true
	}
	statuses := append(append([]agentorcav1alpha1.WorkflowStepStatus{}, wf.Status.Steps...), wf.Status.DynamicStepStatuses...)
	for _, ss := range statuses {
		if ss.Sandbox != nil && ss.Sandbox.Name != "" {
			names[ss.Sandbox.Name] = true
		}
	}
	for name := range names {
		if err := r.Sandboxes.Delete(ctx, name); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting workflow sandbox %s: %w", name, err)
		}
	}
	if err := r.Sandboxes.DeleteProviders(ctx, workflowSandboxLabels(wf)); err != nil {
		if osv1.IsConflict(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("deleting workflow sandbox credentials: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *AgentWorkflowReconciler) handleWorkflowDeletion(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(wf, workflowSandboxFinalizer) {
		return ctrl.Result{}, nil
	}
	result, err := r.cleanupWorkflowSandbox(ctx, wf)
	if err != nil || result.RequeueAfter > 0 {
		return result, err
	}
	controllerutil.RemoveFinalizer(wf, workflowSandboxFinalizer)
	return ctrl.Result{}, r.Update(ctx, wf)
}

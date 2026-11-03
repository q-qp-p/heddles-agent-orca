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
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/shellsandbox"
	agentwebhook "github.com/heddles/agent-orca/internal/webhook"
)

// AnnotationShellSandbox is set on an AgentRun when its sandbox_exec sandbox
// has been created. The sandbox lives on the OpenShell gateway, outside
// owner-reference garbage collection, so the run's cleanup deletes it by this name.
const AnnotationShellSandbox = "agentorca.io/shell-sandbox"

// AnnotationShellGitTokenExpiry records, on an AgentRun, when the GitHub token
// of its shell sandbox's class credential (RFC 3339) expires. The shell
// service renews the token while commands run.
const AnnotationShellGitTokenExpiry = "agentorca.io/shell-git-token-expiry"

// defaultShellWorkDir is where commands run when neither the Agent nor the
// call names a directory: the writable workspace of every sandbox policy.
const defaultShellWorkDir = "/sandbox"

// ShellSandboxService implements shellsandbox.Runner: it keeps one sandbox per
// run for the sandbox_exec tool (Agent.spec.shell) and runs commands in it as
// detached jobs. The sandbox is governed by the SandboxClass the Agent names and
// nothing else: the Agent cannot widen the class's egress, resources or placement.
type ShellSandboxService struct {
	Client client.Client
	// Sandboxes is the OpenShell gateway; nil answers ErrUnavailable.
	Sandboxes         openshell.Gateway
	SandboxPlacement  openshell.Placement
	AllowedRegistries []string
	// GitHubApp mints the tokens of githubApp class credentials; nil makes a
	// class that has one unusable.
	GitHubApp GitHubTokenMinter
}

var _ shellsandbox.Runner = (*ShellSandboxService)(nil)

// shellContext is what a request resolves to.
type shellContext struct {
	run   *agentorcav1alpha1.AgentRun
	agent *agentorcav1alpha1.Agent
	name  string // the sandbox's gateway name
}

// resolve finds the run and its Agent and checks that the run may use a shell.
// It does not check anything that only matters when a sandbox is created.
func (s *ShellSandboxService) resolve(ctx context.Context, namespace, runName string) (*shellContext, error) {
	if s.Sandboxes == nil {
		return nil, shellsandbox.ErrUnavailable
	}
	var run agentorcav1alpha1.AgentRun
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: runName}, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, shellsandbox.Rejectedf("AgentRun %s/%s not found", namespace, runName)
		}
		return nil, fmt.Errorf("reading AgentRun: %w", err)
	}
	var agent agentorcav1alpha1.Agent
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: run.Spec.AgentRef}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, shellsandbox.Rejectedf("Agent %q not found", run.Spec.AgentRef)
		}
		return nil, fmt.Errorf("reading Agent: %w", err)
	}
	if agent.Spec.Shell == nil {
		return nil, shellsandbox.Rejectedf("Agent %q has no spec.shell", agent.Name)
	}
	return &shellContext{run: &run, agent: &agent, name: openshell.ShellSandboxName(namespace, runName)}, nil
}

// Start implements shellsandbox.Runner.
func (s *ShellSandboxService) Start(ctx context.Context, namespace, runName, id, command, workDir string) (bool, error) {
	sc, err := s.resolve(ctx, namespace, runName)
	if err != nil {
		return false, err
	}
	switch sc.run.Status.Phase {
	case agentorcav1alpha1.AgentRunPhaseSucceeded, agentorcav1alpha1.AgentRunPhaseFailed, agentorcav1alpha1.AgentRunPhaseHandedOff:
		// The run's cleanup may already have run; a sandbox created now would leak.
		return false, shellsandbox.Rejectedf("the run has ended (%s)", sc.run.Status.Phase)
	}
	ready, err := s.ensure(ctx, sc)
	if err != nil || !ready {
		return false, err
	}
	if workDir == "" {
		workDir = sc.agent.Spec.Shell.WorkDir
	}
	if workDir == "" {
		workDir = defaultShellWorkDir
	}
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	if s.refreshGit(ctx, sc) {
		maps.Copy(env, openshell.GitEnv())
	}
	if err := openshell.StartJob(ctx, s.Sandboxes, sc.name, id, command, env, workDir); err != nil {
		return false, fmt.Errorf("starting the command: %w", err)
	}
	return true, nil
}

// Poll implements shellsandbox.Runner.
func (s *ShellSandboxService) Poll(ctx context.Context, namespace, runName, id string) (shellsandbox.Job, error) {
	sc, err := s.resolve(ctx, namespace, runName)
	if err != nil {
		return shellsandbox.Job{}, err
	}
	// A long command outlives the hour a GitHub token lasts.
	s.refreshGit(ctx, sc)
	st, err := openshell.PollJob(ctx, s.Sandboxes, sc.name, id)
	if err != nil {
		if osv1.IsNotFound(err) {
			return shellsandbox.Job{Phase: shellsandbox.PhaseMissing}, nil
		}
		return shellsandbox.Job{}, err
	}
	return shellsandbox.Job{Phase: string(st.Phase), ExitCode: st.ExitCode, Output: st.Output}, nil
}

// Stop implements shellsandbox.Runner.
func (s *ShellSandboxService) Stop(ctx context.Context, namespace, runName, id string) error {
	sc, err := s.resolve(ctx, namespace, runName)
	if err != nil {
		return err
	}
	if err := openshell.StopJob(ctx, s.Sandboxes, sc.name, id); err != nil && !osv1.IsNotFound(err) {
		return err
	}
	return nil
}

// ensure creates the run's sandbox if it does not exist and reports whether it
// is ready for commands. A sandbox that failed is deleted so the next call
// starts a fresh one.
func (s *ShellSandboxService) ensure(ctx context.Context, sc *shellContext) (bool, error) {
	sb, err := s.Sandboxes.Get(ctx, sc.name)
	if err != nil {
		if !osv1.IsNotFound(err) {
			return false, fmt.Errorf("reading the sandbox: %w", err)
		}
		sb, err = s.create(ctx, sc)
		if err != nil {
			return false, err
		}
	}
	if sb.Status.Phase == osv1.SandboxReady {
		return true, nil
	}
	if outcome, reason := openshell.Observe(sb); outcome != openshell.OutcomeRunning {
		if delErr := s.Sandboxes.Delete(ctx, sc.name); delErr != nil {
			log.FromContext(ctx).Error(delErr, "deleting a failed shell sandbox", "sandbox", sc.name)
		}
		if reason == "" {
			reason = "its main process exited"
		}
		return false, fmt.Errorf("the sandbox failed: %s", reason)
	}
	return false, nil
}

func (s *ShellSandboxService) create(ctx context.Context, sc *shellContext) (*osv1.Sandbox, error) {
	cfg := sc.agent.Spec.Shell
	if err := agentwebhook.CheckRegistryAllowlist(cfg.Image, s.AllowedRegistries); err != nil {
		return nil, shellsandbox.Rejectedf("the shell image is not allowed: %v", err)
	}
	var ns corev1.Namespace
	if err := s.Client.Get(ctx, client.ObjectKey{Name: sc.run.Namespace}, &ns); err != nil {
		return nil, fmt.Errorf("reading namespace %q: %w", sc.run.Namespace, err)
	}
	if ns.Labels[security.LabelEnableSandboxes] != annotationTrue {
		return nil, shellsandbox.Rejectedf("namespace %q is not opted in to sandboxes (label %s=true)",
			sc.run.Namespace, security.LabelEnableSandboxes)
	}
	class, err := loadSandboxClass(ctx, s.Client, sc.run.Namespace, cfg.ClassRef)
	if err != nil {
		if errors.Is(err, errSandboxClassUnusable) {
			return nil, shellsandbox.Rejectedf("%v", err)
		}
		return nil, err
	}
	agent, placement := openshell.ApplyClass(shellSandboxAgent(cfg), class, s.SandboxPlacement)
	creds, err := resolveClassCredentials(ctx, s.Client, sc.run.Namespace, class)
	if err != nil {
		if errors.Is(err, errSandboxClassUnusable) {
			return nil, shellsandbox.Rejectedf("%v", err)
		}
		return nil, err
	}
	// The providers carry the run's labels, so the run's cleanup revokes them
	// with its other credential providers.
	providers, err := ensureClassCredentials(ctx, s.Sandboxes, sc.run.Namespace, sc.run.Name+"-shell", sandboxRunLabels(sc.run), creds)
	if err != nil {
		return nil, fmt.Errorf("preparing the class credentials: %w", err)
	}
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	var git *openshell.GitAccess
	if gh := agent.Spec.Runtime.Sandbox.GitHub; gh != nil {
		if s.GitHubApp == nil {
			return nil, shellsandbox.Rejectedf("%v", errNoGitHubApp)
		}
		if git, err = s.storeGitToken(ctx, sc, gh); err != nil {
			return nil, fmt.Errorf("preparing the class's GitHub access: %w", err)
		}
		maps.Copy(env, openshell.GitEnv())
		providers = append(providers, git.Provider)
	}
	spec := openshell.BuildSpec(openshell.SpecInput{
		Agent:            agent,
		Env:              env,
		Providers:        providers,
		Placement:        placement,
		ClassCredentials: creds,
		Git:              git,
	})
	labels := map[string]string{
		openshell.LabelRunName:      sc.run.Name,
		openshell.LabelRunNamespace: sc.run.Namespace,
		openshell.LabelShell:        annotationTrue,
	}
	sb, err := s.Sandboxes.Create(ctx, sc.name, spec, labels)
	if err != nil {
		if osv1.IsInvalidArgument(err) || osv1.IsPermissionDenied(err) {
			return nil, shellsandbox.Rejectedf("the gateway refused the sandbox: %v", err)
		}
		return nil, fmt.Errorf("creating the sandbox: %w", err)
	}
	// Record the name so the run's cleanup deletes the sandbox; the annotation
	// is only needed from here on, so a failure to write it fails the call and
	// the retry finds the sandbox already created.
	patch := client.MergeFrom(sc.run.DeepCopy())
	if sc.run.Annotations == nil {
		sc.run.Annotations = map[string]string{}
	}
	sc.run.Annotations[AnnotationShellSandbox] = sc.name
	if err := s.Client.Patch(ctx, sc.run, patch); err != nil {
		return nil, fmt.Errorf("recording the sandbox on the run: %w", err)
	}
	return sb, nil
}

// storeGitToken mints a GitHub token for gh, stores it in the shell sandbox's
// git provider (creating or updating it) and records its expiry on the run.
// Updating the provider reaches the running sandbox: each git command is a new
// process and resolves the placeholder to the provider's current value. The
// provider carries the run's labels, so the run's cleanup revokes it.
func (s *ShellSandboxService) storeGitToken(ctx context.Context, sc *shellContext, gh *agentorcav1alpha1.SandboxGitHub) (*openshell.GitAccess, error) {
	if err := s.Sandboxes.EnsureProfile(ctx, openshell.GitProfile(sc.run.Namespace)); err != nil {
		return nil, err
	}
	tok, err := s.GitHubApp.InstallationToken(ctx, gitTokenRequest(gh))
	if err != nil {
		return nil, err
	}
	provider := openshell.GitProvider(sc.run.Namespace, sc.run.Name+"-shell", tok.Value, sandboxRunLabels(sc.run))
	if err := s.Sandboxes.EnsureProvider(ctx, provider); err != nil {
		return nil, err
	}
	patch := client.MergeFrom(sc.run.DeepCopy())
	if sc.run.Annotations == nil {
		sc.run.Annotations = map[string]string{}
	}
	sc.run.Annotations[AnnotationShellGitTokenExpiry] = tok.ExpiresAt.UTC().Format(time.RFC3339)
	if err := s.Client.Patch(ctx, sc.run, patch); err != nil {
		return nil, fmt.Errorf("recording the git token expiry: %w", err)
	}
	return &openshell.GitAccess{Repositories: gh.Repositories, Push: gh.Push, Provider: provider.Name}, nil
}

// refreshGit renews the shell sandbox's GitHub token when it is close to
// expiry, and reports whether the class gives the sandbox GitHub access at all.
// A failure is logged and retried on the next call: the current token stays
// valid until its expiry.
func (s *ShellSandboxService) refreshGit(ctx context.Context, sc *shellContext) bool {
	class, err := loadSandboxClass(ctx, s.Client, sc.run.Namespace, sc.agent.Spec.Shell.ClassRef)
	if err != nil {
		return false
	}
	gh := openshell.ClassGitHub(class)
	if gh == nil || s.GitHubApp == nil {
		return false
	}
	if exp, err := time.Parse(time.RFC3339, sc.run.Annotations[AnnotationShellGitTokenExpiry]); err == nil && time.Until(exp) > gitTokenRefreshBefore {
		return true
	}
	if _, err := s.storeGitToken(ctx, sc, gh); err != nil {
		log.FromContext(ctx).Error(err, "refreshing the shell sandbox's GitHub token", "run", sc.run.Name)
	}
	return true
}

// shellSandboxAgent adapts Agent.spec.shell to the Agent shape the sandbox
// compiler takes. Its main process is idle: commands are detached jobs. Only
// the class decides egress, resources and placement, and enforcement is always
// on, so a host the class does not allow is denied (or proposed for approval).
func shellSandboxAgent(cfg *agentorcav1alpha1.ShellAccessSpec) *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{
		Runtime: agentorcav1alpha1.AgentRuntime{
			OCIRef:  cfg.Image,
			Command: []string{"sh", "-c", `while :; do sleep 3600; done`},
			Sandbox: &agentorcav1alpha1.SandboxSpec{
				Provider:    agentorcav1alpha1.SandboxProviderOpenShell,
				ClassRef:    cfg.ClassRef,
				Enforcement: agentorcav1alpha1.SandboxEnforcementEnforce,
			},
		},
	}}
}

// deleteShellSandbox deletes the sandbox behind the run's sandbox_exec tool, if
// it created one. Failures are logged, as for the run's own sandbox: a gateway
// outage must not block the run's finalizer.
func (r *AgentRunReconciler) deleteShellSandbox(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	name := run.Annotations[AnnotationShellSandbox]
	if name == "" || r.Sandboxes == nil {
		return
	}
	if err := r.Sandboxes.Delete(ctx, name); err != nil {
		log.FromContext(ctx).Error(err, "deleting the shell sandbox", "sandbox", name)
		if r.Recorder != nil {
			r.Recorder.Eventf(run, corev1.EventTypeWarning, "SandboxDeleteFailed",
				"deleting shell sandbox %s: %v", name, err)
		}
	}
}

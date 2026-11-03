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
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/githubapp"
	"github.com/heddles/agent-orca/internal/openshell"
)

// gitTokenRefreshBefore is how long before expiry the operator replaces a
// run's GitHub installation token. Tokens last an hour.
const gitTokenRefreshBefore = 15 * time.Minute

// errNoGitHubApp fails runs whose Agent asks for GitHub access when the
// operator has no GitHub App configured.
var errNoGitHubApp = errors.New("spec.runtime.sandbox.github or a githubApp class credential is set but the operator has no GitHub App configured " +
	"(chart value github.app.secretName)")

// GitHubTokenMinter mints GitHub App installation tokens (githubapp.Client).
type GitHubTokenMinter interface {
	InstallationToken(ctx context.Context, req githubapp.TokenRequest) (*githubapp.Token, error)
}

// gitTokenRequest narrows the installation token to the Agent's repositories
// and to the contents permission it needs.
func gitTokenRequest(gh *agentorcav1alpha1.SandboxGitHub) githubapp.TokenRequest {
	req := githubapp.TokenRequest{Permissions: map[string]string{"contents": "read", "metadata": "read"}}
	if gh.Push {
		req.Permissions["contents"] = string(agentorcav1alpha1.SandboxCredentialPermissionWrite)
	}
	for _, repo := range gh.Repositories {
		_, name, _ := strings.Cut(repo, "/")
		req.Repositories = append(req.Repositories, name)
	}
	return req
}

// ensureSandboxGit mints the run's GitHub token, stores it in the run's git
// provider and records its expiry. It returns the git access to compile into
// the sandbox policy, or nil when the Agent has no GitHub access.
func (r *AgentRunReconciler) ensureSandboxGit(
	ctx context.Context,
	run *agentorcav1alpha1.AgentRun,
	agent *agentorcav1alpha1.Agent,
	env map[string]string,
) (*openshell.GitAccess, error) {
	gh := agent.Spec.Runtime.Sandbox.GitHub
	if gh == nil {
		return nil, nil
	}
	if r.GitHubApp == nil {
		return nil, errNoGitHubApp
	}
	if err := r.Sandboxes.EnsureProfile(ctx, openshell.GitProfile(run.Namespace)); err != nil {
		return nil, err
	}
	if err := r.storeGitToken(ctx, run, gh); err != nil {
		return nil, err
	}
	maps.Copy(env, openshell.GitEnv())
	return &openshell.GitAccess{
		Repositories: gh.Repositories,
		Push:         gh.Push,
		Provider:     openshell.GitProviderName(run.Namespace, run.Name),
	}, nil
}

// storeGitToken mints a token, writes it to the run's provider (creating or
// updating it) and records its expiry in the run status. Updating the provider
// reaches a running sandbox: each git command is a new process and resolves
// the placeholder to the provider's current value.
func (r *AgentRunReconciler) storeGitToken(ctx context.Context, run *agentorcav1alpha1.AgentRun, gh *agentorcav1alpha1.SandboxGitHub) error {
	tok, err := r.GitHubApp.InstallationToken(ctx, gitTokenRequest(gh))
	if err != nil {
		return err
	}
	if err := r.Sandboxes.EnsureProvider(ctx, openshell.GitProvider(run.Namespace, run.Name, tok.Value, sandboxRunLabels(run))); err != nil {
		return err
	}
	patch := client.MergeFrom(run.DeepCopy())
	expiry := metav1.NewTime(tok.ExpiresAt)
	run.Status.SandboxGitTokenExpiry = &expiry
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		return fmt.Errorf("recording git token expiry: %w", err)
	}
	return nil
}

// refreshSandboxGit replaces the run's GitHub token when it is close to
// expiry. A failure is logged and retried on the next poll: the current token
// stays valid until its expiry.
func (r *AgentRunReconciler) refreshSandboxGit(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent) {
	gh := agent.Spec.Runtime.Sandbox.GitHub
	// The class's githubApp credentials are part of the run's git access.
	if class, err := r.sandboxClass(ctx, run, agent); err == nil {
		gh = openshell.MergeGitHub(gh, openshell.ClassGitHub(class))
	}
	if gh == nil || r.GitHubApp == nil {
		return
	}
	exp := run.Status.SandboxGitTokenExpiry
	if exp != nil && time.Until(exp.Time) > gitTokenRefreshBefore {
		return
	}
	if err := r.storeGitToken(ctx, run, gh); err != nil {
		log.FromContext(ctx).Error(err, "refreshing the sandbox's GitHub token")
	}
}

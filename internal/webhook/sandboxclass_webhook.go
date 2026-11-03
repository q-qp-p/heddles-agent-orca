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
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
)

// githubHost is the only host a githubApp credential may bind: its token is
// for git over HTTPS to github.com.
const githubHost = "github.com"

// SandboxClassValidator validates SandboxClass credentials.
type SandboxClassValidator struct {
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-agentorca-agentorca-io-v1alpha1-sandboxclass,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorca.agentorca.io,resources=sandboxclasses,verbs=create;update,versions=v1alpha1,name=vsandboxclass.kb.io,admissionReviewVersions=v1

// Handle implements admission.Handler.
func (v *SandboxClassValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	var class agentorcav1alpha1.SandboxClass
	if err := v.Decoder.DecodeRaw(req.Object, &class); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	warnings, err := ValidateClassCredentials(&class.Spec)
	if err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("ok").WithWarnings(warnings...)
}

// SetupSandboxClassWebhook registers the SandboxClass validating webhook.
func SetupSandboxClassWebhook(mgr ctrl.Manager) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	mgr.GetWebhookServer().Register("/validate-agentorca-agentorca-io-v1alpha1-sandboxclass",
		&admission.Webhook{Handler: &SandboxClassValidator{Decoder: decoder}},
	)
	return nil
}

// ValidateClassCredentials checks the rules the CRD schema cannot: a bound host
// is reachable under the class's egress, an endpoint carries one credential,
// and a githubApp credential's repositories are slugs under one owner. It
// returns warnings for credentials any process in the sandbox may use.
func ValidateClassCredentials(spec *agentorcav1alpha1.SandboxClassSpec) (warnings []string, err error) {
	rules := slices.Clone(spec.Egress)
	for _, p := range spec.EgressPresets {
		rules = append(rules, openshell.PresetEgress(p)...)
	}
	endpoints := map[string]string{} // host:port -> credential name
	for i := range spec.Credentials {
		c := &spec.Credentials[i]
		at := fmt.Sprintf("spec.credentials[%d] (%s)", i, c.Name)

		port := c.Bind.Port
		if port == 0 {
			port = openshell.DefaultCredentialPort
		}
		key := fmt.Sprintf("%s:%d", strings.ToLower(c.Bind.Host), port)
		if other, taken := endpoints[key]; taken {
			return nil, fmt.Errorf("%s: %s is already bound by credential %q; an endpoint carries one credential", at, key, other)
		}
		endpoints[key] = c.Name

		if !openshell.EgressAllows(rules, c.Bind.Host, port) {
			return nil, fmt.Errorf("%s: bind %s is not reachable under the class's egress or presets; a credential binds an endpoint, it does not open one", at, key)
		}
		for j, b := range c.Bind.Binaries {
			if !strings.HasPrefix(b, "/") {
				return nil, fmt.Errorf("%s: bind.binaries[%d] %q must be an absolute path", at, j, b)
			}
		}
		for j, p := range c.Bind.Paths {
			if !strings.HasPrefix(p, "/") {
				return nil, fmt.Errorf("%s: bind.paths[%d] %q must start with /", at, j, p)
			}
		}
		if gh := c.Source.GitHubApp; gh != nil {
			if err := validateGitHubAppCredential(c, gh, port); err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
			continue // bound to git and the listed repositories, whatever binary runs git
		}
		if len(c.Bind.Binaries) == 0 {
			warnings = append(warnings, fmt.Sprintf("credential %q has no bind.binaries: any process in the sandbox can use it", c.Name))
		}
	}
	return warnings, nil
}

// validateGitHubAppCredential checks a githubApp credential. Its token is usable
// only for git over HTTPS to github.com and the listed repositories, by the git
// binary, so settings that would narrow or reshape that binding are refused
// rather than silently ignored.
func validateGitHubAppCredential(c *agentorcav1alpha1.SandboxCredential, gh *agentorcav1alpha1.SandboxCredentialGitHubApp, port int32) error {
	if !strings.EqualFold(c.Bind.Host, githubHost) || port != 443 {
		return fmt.Errorf("a githubApp credential must bind github.com:443")
	}
	if len(c.Bind.Binaries) > 0 || len(c.Bind.Paths) > 0 {
		return fmt.Errorf("a githubApp credential is bound to git and its repositories; bind.binaries and bind.paths do not apply")
	}
	if c.Inject != nil {
		return fmt.Errorf("a githubApp credential is presented to git as x-access-token basic auth; inject does not apply")
	}
	for perm := range gh.Permissions {
		if perm != "contents" {
			return fmt.Errorf("permission %q is not accepted: the token is usable only for git, so only contents applies", perm)
		}
	}
	return validateGitHubRepositories(gh.Repositories)
}

// validateGitHubRepositories requires owner/name slugs under one owner, since
// the operator's App installation belongs to one account.
func validateGitHubRepositories(repos []string) error {
	owner := ""
	for i, repo := range repos {
		if !githubRepoPattern.MatchString(repo) || strings.HasSuffix(repo, ".git") {
			return fmt.Errorf("source.githubApp.repositories[%d] %q is not an owner/name repository", i, repo)
		}
		o, _, _ := strings.Cut(repo, "/")
		if owner == "" {
			owner = o
		} else if !strings.EqualFold(o, owner) {
			return fmt.Errorf("source.githubApp.repositories must share one owner: %q and %q differ", owner, o)
		}
	}
	return nil
}

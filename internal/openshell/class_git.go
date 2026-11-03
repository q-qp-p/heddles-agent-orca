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

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// ClassGitHub is the git access a class's githubApp credentials grant: the
// union of their repositories, with push when any of them has contents: write.
// It is nil when the class has none. A githubApp credential is the class-level
// form of an Agent's spec.runtime.sandbox.github and compiles to the same
// credential-bound git endpoint.
func ClassGitHub(class *agentorcav1alpha1.SandboxClass) *agentorcav1alpha1.SandboxGitHub {
	if class == nil {
		return nil
	}
	var out *agentorcav1alpha1.SandboxGitHub
	for i := range class.Spec.Credentials {
		app := class.Spec.Credentials[i].Source.GitHubApp
		if app == nil {
			continue
		}
		out = MergeGitHub(out, &agentorcav1alpha1.SandboxGitHub{
			Repositories: app.Repositories,
			Push:         app.Permissions["contents"] == agentorcav1alpha1.SandboxCredentialPermissionWrite,
		})
	}
	return out
}

// MergeGitHub unions two git accesses (either may be nil): all repositories,
// and push if either pushes. It never narrows, like ApplyClass. The inputs are
// not modified.
func MergeGitHub(a, b *agentorcav1alpha1.SandboxGitHub) *agentorcav1alpha1.SandboxGitHub {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return b.DeepCopy()
	case b == nil:
		return a.DeepCopy()
	}
	out := a.DeepCopy()
	for _, repo := range b.Repositories {
		if !slices.Contains(out.Repositories, repo) {
			out.Repositories = append(out.Repositories, repo)
		}
	}
	out.Push = a.Push || b.Push
	return out
}

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
	"fmt"
	"slices"
	"strings"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// presets are the vetted host lists behind egress presets. All are read-only
// (GET/HEAD/OPTIONS) except github.com, whose smart-HTTP clone and fetch POST
// to git-upload-pack. Without a credential that cannot change anything; with
// sandbox.github, the credential-bound endpoint takes over github.com:443.
var presets = map[agentorcav1alpha1.SandboxEgressPreset][]agentorcav1alpha1.SandboxEgressRule{
	agentorcav1alpha1.SandboxEgressPresetGo: {
		{Host: "proxy.golang.org"}, {Host: "sum.golang.org"}, {Host: "storage.googleapis.com"},
	},
	agentorcav1alpha1.SandboxEgressPresetNPM: {
		{Host: "registry.npmjs.org"},
	},
	agentorcav1alpha1.SandboxEgressPresetPyPI: {
		{Host: "pypi.org"}, {Host: "files.pythonhosted.org"},
	},
	agentorcav1alpha1.SandboxEgressPresetCrates: {
		{Host: "crates.io"}, {Host: "index.crates.io"}, {Host: "static.crates.io"},
	},
	agentorcav1alpha1.SandboxEgressPresetMaven: {
		{Host: "repo.maven.apache.org"}, {Host: "repo1.maven.org"},
	},
	agentorcav1alpha1.SandboxEgressPresetGitHubRead: {
		{Host: "github.com", Access: agentorcav1alpha1.SandboxAccessReadWrite},
		{Host: "codeload.github.com"}, {Host: "api.github.com"}, {Host: "*.githubusercontent.com"},
	},
	// mise resolves versions from its own index and downloads most tools from
	// GitHub releases.
	agentorcav1alpha1.SandboxEgressPresetMise: {
		{Host: "mise.jdx.dev"}, {Host: "mise-versions.jdx.dev"},
		{Host: "api.github.com"}, {Host: "*.githubusercontent.com"},
	},
}

// PresetEgress returns the rules behind a preset.
func PresetEgress(p agentorcav1alpha1.SandboxEgressPreset) []agentorcav1alpha1.SandboxEgressRule {
	return slices.Clone(presets[p])
}

// ApplyClass returns the Agent as the sandbox sees it, with its SandboxClass
// (nil for none) and egress presets folded in, and the placement for its pods.
// Class and Agent settings are combined, never narrowed: egress and filesystem
// paths are unioned, enforcement is the stricter of the two, resources come
// from the Agent and default to the class's, git access is unioned (the class's
// githubApp credentials become spec.runtime.sandbox.github), and placement
// comes from the class, else the operator's. The Agent passed in is not modified.
func ApplyClass(agent *agentorcav1alpha1.Agent, class *agentorcav1alpha1.SandboxClass, operatorPlacement Placement) (*agentorcav1alpha1.Agent, Placement) {
	out := agent.DeepCopy()
	sb := out.Spec.Runtime.Sandbox
	if sb == nil {
		return out, operatorPlacement
	}
	placement := operatorPlacement

	var egress []agentorcav1alpha1.SandboxEgressRule
	var presetNames []agentorcav1alpha1.SandboxEgressPreset
	if class != nil {
		c := class.Spec
		if c.Enforcement == agentorcav1alpha1.SandboxEnforcementEnforce {
			sb.Enforcement = agentorcav1alpha1.SandboxEnforcementEnforce
		}
		egress = append(egress, c.Egress...)
		presetNames = append(presetNames, c.EgressPresets...)
		sb.Filesystem = mergeFilesystem(c.Filesystem, sb.Filesystem)
		sb.GitHub = MergeGitHub(sb.GitHub, ClassGitHub(class))
		if out.Spec.Resources == nil && c.Resources != nil {
			out.Spec.Resources = c.Resources.DeepCopy()
		}
		classPlacement := Placement{
			NodeSelector:      c.NodeSelector,
			Tolerations:       c.Tolerations,
			RuntimeClassName:  c.RuntimeClassName,
			PriorityClassName: c.PriorityClassName,
		}
		if !classPlacement.IsZero() {
			placement = classPlacement
		}
	}
	presetNames = append(presetNames, sb.EgressPresets...)
	for _, p := range presetNames {
		egress = append(egress, presets[p]...)
	}
	egress = append(egress, sb.Egress...)
	sb.Egress = mergeEgress(egress)
	sb.EgressPresets = nil
	return out, placement
}

// MergeSandboxSpecs unions the parts of sandbox configuration that a shared
// workflow sandbox can honor: egress, filesystem and the stricter enforcement.
// Runtime identity, resources, placement, tool selection and git credentials
// stay with the base. The inputs are not modified.
func MergeSandboxSpecs(base *agentorcav1alpha1.SandboxSpec, additions ...*agentorcav1alpha1.SandboxSpec) *agentorcav1alpha1.SandboxSpec {
	var out *agentorcav1alpha1.SandboxSpec
	if base != nil {
		out = base.DeepCopy()
	} else {
		out = &agentorcav1alpha1.SandboxSpec{Provider: agentorcav1alpha1.SandboxProviderOpenShell}
	}
	for _, addition := range additions {
		if addition == nil {
			continue
		}
		if addition.Enforcement == agentorcav1alpha1.SandboxEnforcementEnforce {
			out.Enforcement = agentorcav1alpha1.SandboxEnforcementEnforce
		}
		out.EgressPresets = append(out.EgressPresets, addition.EgressPresets...)
		out.Egress = mergeEgress(slices.Concat(out.Egress, addition.Egress))
		out.Filesystem = mergeFilesystem(out.Filesystem, addition.Filesystem)
	}
	return out
}

// mergeEgress folds rules for the same host and ports into one: the policy
// may hold one endpoint per destination. The merged rule allows what either
// allowed (the broader access, and any binary if either allowed any).
func mergeEgress(rules []agentorcav1alpha1.SandboxEgressRule) []agentorcav1alpha1.SandboxEgressRule {
	type key struct {
		host  string
		ports string
	}
	index := map[key]int{}
	var out []agentorcav1alpha1.SandboxEgressRule
	for _, r := range rules {
		ports := slices.Clone(r.Ports)
		if len(ports) == 0 {
			ports = []int32{443} // egressPorts' default
		}
		slices.Sort(ports)
		k := key{strings.ToLower(r.Host), fmt.Sprint(ports)}
		i, seen := index[k]
		if !seen {
			index[k] = len(out)
			out = append(out, *r.DeepCopy())
			continue
		}
		m := &out[i]
		if accessRank(r.Access) > accessRank(m.Access) {
			m.Access = r.Access
		}
		if len(m.Binaries) == 0 || len(r.Binaries) == 0 {
			m.Binaries = nil
		} else {
			for _, b := range r.Binaries {
				if !slices.Contains(m.Binaries, b) {
					m.Binaries = append(m.Binaries, b)
				}
			}
		}
	}
	return out
}

func accessRank(a agentorcav1alpha1.SandboxAccess) int {
	switch a {
	case agentorcav1alpha1.SandboxAccessFull:
		return 2
	case agentorcav1alpha1.SandboxAccessReadWrite:
		return 1
	default:
		return 0
	}
}

func mergeFilesystem(a, b *agentorcav1alpha1.SandboxFilesystem) *agentorcav1alpha1.SandboxFilesystem {
	if a == nil {
		return b
	}
	out := a.DeepCopy()
	if b != nil {
		for _, p := range b.ReadOnly {
			if !slices.Contains(out.ReadOnly, p) {
				out.ReadOnly = append(out.ReadOnly, p)
			}
		}
		for _, p := range b.ReadWrite {
			if !slices.Contains(out.ReadWrite, p) {
				out.ReadWrite = append(out.ReadWrite, p)
			}
		}
	}
	return out
}

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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxEgressPreset names a vetted set of package-registry and source hosts.
// +kubebuilder:validation:Enum=go;npm;pypi;crates;maven;github-read;mise
type SandboxEgressPreset string

// Egress presets. Each expands to read-only rules for the ecosystem's hosts
// (see internal/openshell/presets.go).
const (
	SandboxEgressPresetGo         SandboxEgressPreset = "go"
	SandboxEgressPresetNPM        SandboxEgressPreset = "npm"
	SandboxEgressPresetPyPI       SandboxEgressPreset = "pypi"
	SandboxEgressPresetCrates     SandboxEgressPreset = "crates"
	SandboxEgressPresetMaven      SandboxEgressPreset = "maven"
	SandboxEgressPresetGitHubRead SandboxEgressPreset = "github-read"
	SandboxEgressPresetMise       SandboxEgressPreset = "mise"
)

// SandboxClassSpec is a reusable sandbox profile set by cluster admins.
type SandboxClassSpec struct {
	// NamespaceSelector limits which namespaces' Agents may use the class. Empty
	// allows every namespace that has opted in to sandboxes.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// Enforcement is the minimum network enforcement for Agents of this class:
	// with enforce, an Agent cannot weaken it to audit.
	// +optional
	Enforcement SandboxEnforcement `json:"enforcement,omitempty"`

	// EgressPresets add vetted read-only host lists, e.g. go or npm.
	// +optional
	EgressPresets []SandboxEgressPreset `json:"egressPresets,omitempty"`

	// Egress adds endpoints every Agent of the class may reach, on top of the
	// Agent's own.
	// +optional
	Egress []SandboxEgressRule `json:"egress,omitempty"`

	// Filesystem adds paths every Agent of the class gets.
	// +optional
	Filesystem *SandboxFilesystem `json:"filesystem,omitempty"`

	// Resources are the defaults for Agents that set no spec.resources.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector, Tolerations, RuntimeClassName and PriorityClassName place the
	// class's sandbox pods, replacing the operator-wide openshell.placement. They
	// live here, not on the Agent, because a toleration can admit a pod to any
	// tainted node; the class is cluster-scoped, so only cluster admins set them.
	// They need the gateway's drivers.kubernetes.allowDriverConfig.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// Approval governs requests for egress the policy does not allow: a
	// sandbox that tries to reach an unlisted host gets a proposed rule, which
	// an auto-approve rule or an approver may allow while the run continues.
	// Without it such requests stay denied.
	// +optional
	Approval *SandboxApproval `json:"approval,omitempty"`

	// Credentials are secrets the class's sandboxes may use without holding.
	// The sandbox gets a placeholder; the supervisor swaps in the real value
	// only on requests to the credential's bound endpoint. Every sandbox made
	// from the class gets all of them, and no Agent or workflow can add one.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Credentials []SandboxCredential `json:"credentials,omitempty"`
}

// SandboxCredential is a secret a sandbox can use but never see.
type SandboxCredential struct {
	// Name identifies the credential within the class. It names the placeholder
	// environment variable, AGENTORCA_CRED_<NAME> (upper case, dashes as
	// underscores).
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,30}$`
	Name string `json:"name"`

	// Source is where the real value comes from.
	Source SandboxCredentialSource `json:"source"`

	// Bind is the only place the value works.
	Bind SandboxCredentialBind `json:"bind"`

	// Inject says how the value is presented to the endpoint. The default is
	// bearer. The process sends the placeholder (the AGENTORCA_CRED_<NAME>
	// environment variable) in the matching header and the supervisor swaps in
	// the real value on the bound endpoint.
	// +optional
	Inject *SandboxCredentialInject `json:"inject,omitempty"`
}

// SandboxCredentialSource names exactly one source of a credential's value.
// +kubebuilder:validation:XValidation:rule="(has(self.githubApp) ? 1 : 0) + (has(self.secretRef) ? 1 : 0) == 1",message="set exactly one of githubApp or secretRef"
type SandboxCredentialSource struct {
	// GitHubApp is an installation token of the operator's GitHub App, narrowed
	// to the listed repositories and valid for one hour. The operator renews it.
	// +optional
	GitHubApp *SandboxCredentialGitHubApp `json:"githubApp,omitempty"`

	// SecretRef is a key of a Kubernetes Secret.
	// +optional
	SecretRef *SandboxCredentialSecretRef `json:"secretRef,omitempty"`
}

// SandboxCredentialGitHubApp mints a GitHub App installation token.
type SandboxCredentialGitHubApp struct {
	// Repositories are "owner/name" slugs, all under the owner the operator's
	// GitHub App is installed on.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=50
	Repositories []string `json:"repositories"`

	// Permissions are the token's repository permissions. The token is usable
	// only for git over HTTPS (clone, fetch and, with contents: write, push) to
	// the listed repositories, so only contents is accepted. Default is
	// contents: read.
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	Permissions map[string]SandboxCredentialPermission `json:"permissions,omitempty"`
}

// SandboxCredentialPermission is the level of one GitHub App permission.
// +kubebuilder:validation:Enum=read;write
type SandboxCredentialPermission string

// GitHub App permission levels.
const (
	SandboxCredentialPermissionRead  SandboxCredentialPermission = "read"
	SandboxCredentialPermissionWrite SandboxCredentialPermission = "write"
)

// SandboxCredentialSecretRef reads one key of a Secret.
type SandboxCredentialSecretRef struct {
	// Name is the Secret's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key in the Secret's data.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// Namespace is where the Secret lives. Empty means the namespace of the
	// workload that uses the class, so each tenant's workloads get their own
	// value. A namespace must be one of the allowedNamespaces of the TenantConfig
	// the workload's namespace belongs to; the operator checks that when it
	// creates the sandbox.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SandboxCredentialBind restricts where a credential works.
type SandboxCredentialBind struct {
	// Host is the exact destination host. It must be reachable under the
	// class's egress rules or presets: a credential binds an endpoint, it does
	// not open one.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`
	Host string `json:"host"`

	// Port is the destination port. Default 443.
	// +kubebuilder:default=443
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Binaries are the executables (absolute paths) that may use the value.
	// Empty lets any process in the sandbox use it.
	// +optional
	Binaries []string `json:"binaries,omitempty"`

	// Paths limits the value to requests whose URL path matches one of these
	// globs (e.g. /heddles/agent-orca.git/**). Empty means any path.
	// +optional
	Paths []string `json:"paths,omitempty"`
}

// SandboxCredentialInjectStyle is how a credential is presented to the endpoint.
// +kubebuilder:validation:Enum=bearer;basic;header
type SandboxCredentialInjectStyle string

// Credential injection styles.
const (
	SandboxCredentialInjectBearer SandboxCredentialInjectStyle = "bearer"
	SandboxCredentialInjectBasic  SandboxCredentialInjectStyle = "basic"
	SandboxCredentialInjectHeader SandboxCredentialInjectStyle = "header"
)

// SandboxCredentialInject says where in the request the placeholder is
// replaced with the value.
// +kubebuilder:validation:XValidation:rule="self.style != 'header' || (has(self.name) && size(self.name) > 0)",message="style header needs a header name"
// +kubebuilder:validation:XValidation:rule="self.style == 'header' || !has(self.name)",message="name applies to style header only"
type SandboxCredentialInject struct {
	// Style is bearer (Authorization: Bearer <value>), basic (Authorization:
	// Basic, with the value as the password; the client chooses the user) or
	// header (the value alone in the named header).
	Style SandboxCredentialInjectStyle `json:"style"`

	// Name is the header for style header, e.g. X-Api-Key.
	// +optional
	Name string `json:"name,omitempty"`
}

// SandboxApproval configures egress approvals for a SandboxClass.
type SandboxApproval struct {
	// ApproverGroups are the identity-provider groups whose members may decide.
	// Empty lets any user with access to the run's namespace decide.
	// +optional
	ApproverGroups []string `json:"approverGroups,omitempty"`

	// Timeout is how long a request waits for a decision before it is rejected.
	// Default 30m.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// AutoApprove allows matching requests without a person. A request the
	// sandbox's policy prover flags with security notes is never auto-approved.
	// +optional
	AutoApprove []SandboxAutoApproveRule `json:"autoApprove,omitempty"`
}

// SandboxAutoApproveRule matches egress requests to approve automatically.
type SandboxAutoApproveRule struct {
	// Host is the destination host, or a wildcard such as *.example.com.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Ports the request may name. Default [443].
	// +optional
	Ports []int32 `json:"ports,omitempty"`

	// Binaries the requesting process may be (absolute paths). Empty matches any.
	// +optional
	Binaries []string `json:"binaries,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Enforcement",type=string,JSONPath=`.spec.enforcement`
// +kubebuilder:printcolumn:name="Presets",type=string,JSONPath=`.spec.egressPresets`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SandboxClass is a cluster-wide sandbox profile: egress, filesystem, resource
// defaults, placement and minimum enforcement. Agents use one with
// spec.runtime.sandbox.classRef and may add to it, but not weaken it.
type SandboxClass struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SandboxClassSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxClassList contains a list of SandboxClass.
type SandboxClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxClass{}, &SandboxClassList{})
}

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentSpec defines the desired state of an Agent.
type AgentSpec struct {
	// ModelSelectorRef names the ModelSelector that routes LLM calls for this agent.
	// +kubebuilder:validation:MinLength=1
	ModelSelectorRef string `json:"modelSelectorRef"`

	// Tools is the list of Tool names (in the same namespace) available to this agent.
	// The model-router injects these as OpenAI function definitions for every run.
	// +optional
	Tools []string `json:"tools,omitempty"`

	// SystemPrompt is the system-level instruction prepended to every conversation.
	// Leave empty to use the agent's own system prompt.
	// +optional
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Runtime defines the OCI image used to run the agent process.
	// +kubebuilder:validation:Required
	Runtime AgentRuntime `json:"runtime"`

	// Memory configures conversation state persistence and failure recovery.
	// +optional
	Memory *AgentMemoryConfig `json:"memory,omitempty"`

	// Resources sets default CPU/memory limits for AgentRun pods.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`

	// ServiceAccountRef references a pre-existing ServiceAccount to use for agent pods.
	// If unset, the operator creates and manages "agentorca-agent-<name>".
	// Use this when the SA already has cloud provider annotations (IRSA, Workload Identity).
	// +optional
	ServiceAccountRef *LocalObjectRef `json:"serviceAccountRef,omitempty"`

	// CloudAuth declares cloud-provider identity bindings.
	// The operator applies the appropriate SA annotations automatically.
	// +optional
	CloudAuth *CloudAuthSpec `json:"cloudAuth,omitempty"`

	// KnowledgeBases lists KnowledgeBase names (same namespace) available to this agent.
	// When non-empty, the model-router injects _rag_search and _rag_ingest as built-in tools.
	// For finer control (e.g. confirmRequired), use KnowledgeBaseRefs instead.
	// +optional
	KnowledgeBases []string `json:"knowledgeBases,omitempty"`

	// KnowledgeBaseRefs is the structured alternative to KnowledgeBases.
	// When set, KnowledgeBases is ignored. Each ref supports per-KB options
	// such as requiring user confirmation before ingesting fixes.
	// +optional
	KnowledgeBaseRefs []KnowledgeBaseRef `json:"knowledgeBaseRefs,omitempty"`

	// GuardrailPolicyRef names the GuardrailPolicy (same namespace) applied to this agent's
	// inputs and outputs. The model-router enforces the policy's filters on every LLM call.
	// +optional
	GuardrailPolicyRef string `json:"guardrailPolicyRef,omitempty"`

	// NetworkIsolation configures network-level isolation between the agent and model-router.
	// When SplitPod is enabled, the agent and model-router run in separate Kubernetes Pods
	// with distinct NetworkPolicies, providing network-level egress isolation that the
	// default single-pod sidecar topology cannot enforce (containers in a Pod share a
	// network namespace, so NetworkPolicy cannot differentiate them).
	// +optional
	NetworkIsolation *NetworkIsolationSpec `json:"networkIsolation,omitempty"`

	// DisableClarify prevents the auto-trigger clarify safety net from activating.
	// Use this for autonomous agents that should not block waiting for human input.
	// When true, the _clarify tool is still available but auto-detection is disabled.
	// +kubebuilder:default=false
	// +optional
	DisableClarify bool `json:"disableClarify,omitempty"`

	// Filesystem configures access to the user's local workspace via the ACP
	// bridge's fs/* relay (issue #34): when set, the agent gets built-in tools
	// fs_read_text_file / fs_write_text_file whose calls are forwarded through
	// the `aoctl acp serve` bridge to the ACP client (e.g. Zed), which reads
	// and edits the user's real files. The bridge is the enforcement point for
	// workspace-root confinement and (optionally) write approval; this field
	// only decides whether and in which mode the tools exist. Nil disables
	// filesystem access entirely.
	// +optional
	Filesystem *FilesystemAccessSpec `json:"filesystem,omitempty"`

	// Shell gives the agent a built-in sandbox_exec tool that runs shell
	// commands in a sandbox the operator creates for the run on the first call.
	// The sandbox is the run's own: files and installed tools persist between
	// calls, and it is deleted when the run ends. Unlike spec.runtime.sandbox,
	// which confines the agent process itself, the agent stays in its normal pod
	// and only the commands it asks for run in the sandbox. Requires an OpenShell
	// gateway and a namespace labelled `agentorca.io/enable-sandboxes: "true"`.
	// Nil (the default) gives the agent no shell.
	// +optional
	Shell *ShellAccessSpec `json:"shell,omitempty"`
}

// ShellAccessSpec configures the sandbox_exec tool of an Agent.
type ShellAccessSpec struct {
	// ClassRef names the cluster-scoped SandboxClass whose egress, filesystem,
	// resources, placement and enforcement govern the sandbox. The class is the
	// only policy: the Agent cannot widen it, and a host the class does not allow
	// is denied (or goes to the egress approval inbox).
	// +kubebuilder:validation:MinLength=1
	ClassRef string `json:"classRef"`

	// Image is the sandbox workload image, which decides the toolchains the
	// agent has. It must come from an allowed registry.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// WorkDir is the directory commands run in unless the call names another.
	// +kubebuilder:default=/sandbox
	// +optional
	WorkDir string `json:"workDir,omitempty"`

	// MaxTimeoutSeconds caps how long one command may run. A call may ask for
	// less; the default for a call that does not ask is 120 seconds.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=600
	// +optional
	MaxTimeoutSeconds int32 `json:"maxTimeoutSeconds,omitempty"`
}

// FilesystemAccessMode selects how the agent may touch the user's workspace
// through the ACP bridge relay.
type FilesystemAccessMode string

const (
	// FilesystemReadOnly exposes only fs_read_text_file.
	FilesystemReadOnly FilesystemAccessMode = "readOnly"
	// FilesystemReadWrite exposes fs_read_text_file and fs_write_text_file.
	FilesystemReadWrite FilesystemAccessMode = "readWrite"
)

// FilesystemAccessSpec configures the ACP bridge fs/* relay for an agent.
type FilesystemAccessSpec struct {
	// Mode grants read-only or read-write access to the workspace. In readOnly
	// mode only fs_read_text_file is exposed; fs_write_text_file is refused by
	// the model-router (defense in depth) and rejected by the bridge.
	// +kubebuilder:validation:Enum=readOnly;readWrite
	// +kubebuilder:default=readOnly
	Mode FilesystemAccessMode `json:"mode"`

	// WriteApproval asks the bridge to request explicit user approval (via the
	// ACP elicitation flow) before applying every fs_write_text_file. Only
	// meaningful with mode readWrite. Default: true — the conservative default
	// given writes land in the user's checked-out workspace.
	// +kubebuilder:default=true
	// +optional
	WriteApproval bool `json:"writeApproval"`

	// RememberApproval lets the bridge stop re-prompting for the rest of an
	// ACP session once the user approves one write ("approve once"). Set false
	// to require approval for every write. Only meaningful with writeApproval
	// true. Agent policy and the workstation's --fs-remember-approval flag can
	// only narrow each other: approvals are remembered only when both allow
	// it (issue #41). Default: true.
	// +kubebuilder:default=true
	// +optional
	RememberApproval *bool `json:"rememberApproval,omitempty"`
}

// NetworkIsolationSpec configures split-pod topology for agent/router network separation.
type NetworkIsolationSpec struct {
	// SplitPod runs the agent and model-router in separate Pods with distinct NetworkPolicies.
	// The agent pod's egress is restricted to the router Service (ports 8080/8082) and DNS.
	// The router pod retains all required egress (provider HTTPS, Kubernetes API, Redis, tools).
	// This provides a Kubernetes-level network guarantee that agent code cannot open arbitrary
	// outbound connections when platform policy requires all LLM traffic to go via model-router.
	// +kubebuilder:default=false
	// +optional
	SplitPod bool `json:"splitPod,omitempty"`
}

// KnowledgeBaseRef references a KnowledgeBase with per-KB options.
type KnowledgeBaseRef struct {
	// Name is the KnowledgeBase CR name (same namespace).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// ConfirmRequired when true replaces the _rag_ingest tool with _propose_fix and
	// _confirm_fix for this knowledge base. Fixes are staged in Redis and only promoted
	// to the KnowledgeBase when the user explicitly confirms they worked.
	// +optional
	ConfirmRequired bool `json:"confirmRequired,omitempty"`
}

// AgentRuntime describes the agent process container image and framework.
type AgentRuntime struct {
	// OCIRef is the OCI image reference for the agent process.
	// Must be signed with Cosign unless imageVerification.skip is set.
	// +kubebuilder:validation:MinLength=1
	OCIRef string `json:"ociRef"`

	// Framework hints to the operator which env var injection tier to use.
	// Supported values:
	//   openai-compatible  - inject OPENAI_BASE_URL + OPENAI_API_KEY (default)
	//   autogen            - inject OAI_CONFIG_LIST via init container
	//   semantic-kernel    - inject appsettings via init container
	//   langgraph          - openai-compatible + inject LANGGRAPH_CHECKPOINT_URL
	//   shim               - hosts-file shim for frameworks that hardcode their endpoint
	// +kubebuilder:validation:Enum=openai-compatible;autogen;semantic-kernel;langgraph;shim
	// +kubebuilder:default=openai-compatible
	// +optional
	Framework string `json:"framework,omitempty"`

	// ShimTarget is the hostname to intercept when Framework is "shim".
	// Example: "api.openai.com", "generativelanguage.googleapis.com"
	// +optional
	ShimTarget string `json:"shimTarget,omitempty"`

	// Command overrides the container entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args overrides the container command arguments.
	// +optional
	Args []string `json:"args,omitempty"`

	// InputMode controls how the operator delivers the run input to the agent.
	// env  - inject AGENTORC_INPUT env var (default)
	// http - POST the input as JSON to the agent's HTTP endpoint
	// +kubebuilder:validation:Enum=env;http
	// +optional
	InputMode string `json:"inputMode,omitempty"`

	// InputPort is the TCP port the agent's HTTP server listens on (http mode only).
	// Defaults to 8000.
	// +optional
	InputPort int `json:"inputPort,omitempty"`

	// InputPath is the HTTP path to POST the run input to (http mode only).
	// Defaults to /invoke.
	// +optional
	InputPath string `json:"inputPath,omitempty"`

	// SecurityContextOverride opts the agent container (only the "agent" container,
	// never the model-router sidecar) out of the default restricted PodSecurityStandard.
	//
	// Intended ONLY for purpose-built agents that require elevated privileges the
	// restricted baseline forbids — e.g. a red-team pwnbox that must bring up a VPN
	// tun device (requires NET_ADMIN + /dev/net/tun). When set, the operator mounts
	// /dev/net/tun into the agent container and applies the requested posture.
	//
	// Admission is gated by a validating webhook: the agent's namespace must carry the
	// label `agentorca.io/enable-privileged-pods: "true"` AND the Agent must reference a
	// GuardrailPolicyRef. These checks fail‑closed.
	// +optional
	SecurityContextOverride *PodSecurityOverride `json:"securityContextOverride,omitempty"`

	// SecretRefs lists Kubernetes Secrets to mount into the agent container as volumes
	// (or, if MountPath is empty, inject as env vars). Unlike Tool.spec.secretRefs —
	// which target the model-router sidecar / tool pods — these mount directly into the
	// agent container. Useful for agent containers that need an on-disk credential the
	// agent code reads directly, e.g. an HTB OpenVPN ``.ovpn`` config mounted at
	// ``/etc/htb``. Mounted read-only.
	// +optional
	SecretRefs []SecretMount `json:"secretRefs,omitempty"`

	// Sandbox runs the agent process inside an isolated sandbox runtime instead of
	// a plain agent pod. The agent gets a real shell and may exec subprocesses and
	// install tools, while the sandbox confines its filesystem (Landlock), syscalls
	// (seccomp) and network egress (per-host L7 policy enforced by a supervisor).
	//
	// Requires spec.networkIsolation.splitPod=true: the model-router keeps running in
	// its own pod and stays the only LLM egress path. Admission is gated by the
	// namespace label `agentorca.io/enable-sandboxes: "true"` and a GuardrailPolicyRef.
	// Nil (the default) keeps today's pod topology unchanged.
	// +optional
	Sandbox *SandboxSpec `json:"sandbox,omitempty"`

	// LocalTools lets the agent process run tools itself. A function tool the
	// agent declares in its chat request that the model-router does not provide
	// (e.g. a shell inside the sandbox) is returned to the agent with
	// finish_reason "tool_calls"; the agent runs it and sends the result as a
	// "tool" message on its next request. The router still runs its own tools,
	// and applies safeguards, guardrails and tracing to both kinds. Agent tools
	// may not reuse a router tool or built-in name.
	// +optional
	LocalTools bool `json:"localTools,omitempty"`
}

// SandboxProvider names a sandbox runtime implementation.
// +kubebuilder:validation:Enum=openshell
type SandboxProvider string

const (
	// SandboxProviderOpenShell runs the agent in an NVIDIA OpenShell sandbox.
	SandboxProviderOpenShell SandboxProvider = "openshell"
)

// SandboxEnforcement controls whether the sandbox's network policy blocks
// violations or only records them.
// +kubebuilder:validation:Enum=audit;enforce
type SandboxEnforcement string

const (
	// SandboxEnforcementAudit logs L7 policy violations without blocking them.
	SandboxEnforcementAudit SandboxEnforcement = "audit"
	// SandboxEnforcementEnforce blocks L7 policy violations.
	SandboxEnforcementEnforce SandboxEnforcement = "enforce"
)

// SandboxAccess selects the set of protocol operations an egress endpoint permits.
// +kubebuilder:validation:Enum=read-only;read-write;full
type SandboxAccess string

const (
	// SandboxAccessReadOnly permits read operations (e.g. HTTP GET/HEAD).
	SandboxAccessReadOnly SandboxAccess = "read-only"
	// SandboxAccessReadWrite permits read and write operations.
	SandboxAccessReadWrite SandboxAccess = "read-write"
	// SandboxAccessFull permits every operation the protocol supports.
	SandboxAccessFull SandboxAccess = "full"
)

// SandboxSpec configures the sandbox runtime for an Agent.
type SandboxSpec struct {
	// ClassRef names a cluster-scoped SandboxClass whose egress, filesystem,
	// resource defaults, placement and minimum enforcement apply to this Agent.
	// The Agent's own settings add to the class but cannot weaken it.
	// +optional
	ClassRef string `json:"classRef,omitempty"`

	// Provider selects the sandbox runtime.
	// +kubebuilder:default=openshell
	Provider SandboxProvider `json:"provider"`

	// Enforcement is the network policy mode. Start with audit, then promote to
	// enforce once the run's egress has been observed.
	// +kubebuilder:default=audit
	// +optional
	Enforcement SandboxEnforcement `json:"enforcement,omitempty"`

	// Egress lists extra endpoints the agent process may reach directly, such as
	// package registries for installing tools. The model-router is always
	// reachable, and the Agent's tools are added per spec.runtime.sandbox.tools;
	// everything else is denied.
	// +optional
	Egress []SandboxEgressRule `json:"egress,omitempty"`

	// EgressPresets add vetted read-only host lists for package registries and
	// sources, e.g. go (module proxy and checksum database) or npm.
	// +optional
	EgressPresets []SandboxEgressPreset `json:"egressPresets,omitempty"`

	// Tools controls whether the agent process may reach its tools' endpoints
	// directly from the sandbox. By default it can: each Tool's networkEgress
	// hosts and the URL of each remote (http/sse) MCP tool are added to the
	// sandbox egress. Tool calls made through the model-router are unaffected.
	// +optional
	Tools *SandboxTools `json:"tools,omitempty"`

	// Filesystem adds paths to the sandbox's default filesystem policy
	// (read-write: /sandbox, /tmp; read-only: system paths).
	// +optional
	Filesystem *SandboxFilesystem `json:"filesystem,omitempty"`

	// GitHub gives the agent git access (clone, fetch and optionally push) to
	// GitHub repositories over HTTPS, using an installation token of the
	// operator's GitHub App. The token is minted per run, narrowed to these
	// repositories, refreshed before it expires, and never visible to the
	// agent: the workload holds a placeholder that the sandbox supervisor
	// replaces on requests from git to github.com only. Which branches a push
	// may update is governed by the repositories' rulesets on GitHub.
	// +optional
	GitHub *SandboxGitHub `json:"github,omitempty"`
}

// SandboxGitHub configures git access to GitHub from a sandbox.
type SandboxGitHub struct {
	// Repositories are the "owner/name" repositories the token is scoped to.
	// They must belong to the account the operator's GitHub App is installed on.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=50
	Repositories []string `json:"repositories"`

	// Push allows git push (contents: write). Without it the token can only
	// clone and fetch.
	// +optional
	Push bool `json:"push,omitempty"`
}

// SandboxTools selects which of the Agent's tool endpoints the sandbox may reach.
type SandboxTools struct {
	// Inherit adds the Agent's tool endpoints to the sandbox egress. Set it to
	// false to keep tools reachable only through the model-router.
	// +kubebuilder:default=true
	// +optional
	Inherit *bool `json:"inherit,omitempty"`

	// ExcludeTools lists Tool names (from spec.tools) whose endpoints are not added.
	// +optional
	ExcludeTools []string `json:"excludeTools,omitempty"`

	// ExcludeHosts lists hosts that are not added even when a tool declares them.
	// Matched exactly against the tool's host. It does not filter spec.runtime.sandbox.egress.
	// +optional
	ExcludeHosts []string `json:"excludeHosts,omitempty"`
}

// InheritsToolEgress reports whether the sandbox gets the Agent's tool
// endpoints. It defaults to true, including when Tools is unset.
func (s *SandboxSpec) InheritsToolEgress() bool {
	return s.Tools == nil || s.Tools.Inherit == nil || *s.Tools.Inherit
}

// SandboxEgressRule allows the agent to reach one host.
type SandboxEgressRule struct {
	// Host is the destination hostname. Wildcards like "*.pythonhosted.org" are supported.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Ports lists the destination ports. Defaults to [443].
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	// +optional
	Ports []int32 `json:"ports,omitempty"`

	// Access selects the permitted HTTP operations. read-only (the default) allows
	// GET/HEAD/OPTIONS, enough for package downloads; git clone over HTTPS POSTs,
	// so it needs read-write.
	// +kubebuilder:default=read-only
	// +optional
	Access SandboxAccess `json:"access,omitempty"`

	// Binaries restricts the rule to these executables (absolute paths, e.g.
	// /usr/bin/git). Empty means any binary in the sandbox.
	// +optional
	Binaries []string `json:"binaries,omitempty"`
}

// SandboxFilesystem adds paths to the sandbox filesystem policy.
type SandboxFilesystem struct {
	// ReadOnly lists extra directories the agent may read.
	// +optional
	ReadOnly []string `json:"readOnly,omitempty"`

	// ReadWrite lists extra directories the agent may read and write.
	// +optional
	ReadWrite []string `json:"readWrite,omitempty"`
}

// PodSecurityOverride describes a scoped relaxation of the restricted pod-security
// baseline for the agent container of an Agent. See AgentRuntime.
type PodSecurityOverride struct {
	// Privileged grants the agent container full privileges (CAP_NET_ADMIN,
	// /dev/net/tun device access, seccomp/capability enforcement disabled). Use this
	// for tun-device VPNs. Mutually exclusive with AddCapabilities.
	// +optional
	Privileged bool `json:"privileged,omitempty"`

	// AddCapabilities adds specific capabilities to the agent container while keeping
	// the restricted profile otherwise (e.g. ["NET_ADMIN"] + a /dev/net/tun hostPath
	// mount). Mutually exclusive with Privileged.
	// +optional
	AddCapabilities []string `json:"addCapabilities,omitempty"`

	// RunAsUser overrides the default non-root UID (65532). 0 (root) is permitted only
	// when Privileged is true or a capability that requires root (e.g. NET_ADMIN to
	// create network interfaces) is added; the webhook enforces this.
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// ReadOnlyRootFilesystem overrides the default read-only root filesystem of the
	// agent container. When omitted and Privileged is true, defaults to writable
	// (false) since some security tooling writes helpers/logs to the root FS; when
	// omitted and non-privileged, retains the restricted default (true).
	// +optional
	ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem,omitempty"`
}

// AgentMemoryConfig controls conversation state persistence.
type AgentMemoryConfig struct {
	// CheckpointEvery controls how often (in LLM turns) the model-router writes a checkpoint.
	// 1 means every turn. Higher values reduce storage writes but increase recovery loss window.
	// +kubebuilder:default=1
	// +optional
	CheckpointEvery int `json:"checkpointEvery,omitempty"`

	// ResumeWindowSeconds is how long to retain state after a failure for pod restart/resume.
	// Defaults to 3600 (1 hour).
	// +kubebuilder:default=3600
	// +optional
	ResumeWindowSeconds int `json:"resumeWindowSeconds,omitempty"`

	// ArchiveOnCompletion, when true, promotes state to object storage when the run succeeds.
	// +kubebuilder:default=false
	// +optional
	ArchiveOnCompletion bool `json:"archiveOnCompletion,omitempty"`

	// EpisodicSummaryEvery used to summarize the conversation every N LLM turns.
	//
	// Deprecated: ignored. The model-router now summarizes the conversation when
	// it reaches the compaction threshold (80% of the window by default).
	// +optional
	EpisodicSummaryEvery int `json:"episodicSummaryEvery,omitempty"`

	// SummaryModelSelectorRef names the ModelSelector whose first provider writes
	// the conversation summaries made at the compaction threshold. It is used only
	// when that provider is also one of the agent's providers (its ModelSelector
	// or fallback chain); otherwise, and when unset, the provider serving the
	// request writes the summary.
	// +optional
	SummaryModelSelectorRef string `json:"summaryModelSelectorRef,omitempty"`

	// LongTermMemoryRef names a KnowledgeBase (same namespace) used as persistent
	// long-term memory. The model-router automatically retrieves semantically relevant
	// memories before each turn and exposes a _memory_store tool so the agent can
	// explicitly persist facts across sessions.
	// If the KnowledgeBase is not Ready, the run proceeds without long-term memory
	// and a MemoryUnavailable event is emitted.
	// +optional
	LongTermMemoryRef string `json:"longTermMemoryRef,omitempty"`

	// Hindsight configures the hindsight memory system integration. It is
	// opt-in: without this block the agent has no hindsight memory. When
	// enabled, the model-router recalls relevant memories before each LLM call
	// and retains the conversation after each turn, in one bank per Agent by
	// default, shared by its runs and deployments.
	// +optional
	Hindsight *HindsightMemoryConfig `json:"hindsight,omitempty"`
}

// HindsightMemoryConfig configures the hindsight memory system integration.
type HindsightMemoryConfig struct {
	// Enabled turns hindsight on. It defaults to true once the hindsight block
	// is set.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// URL is the hindsight API endpoint (e.g. http://hindsight.hindsight.svc.cluster.local:8888).
	// +optional
	URL string `json:"url,omitempty"`
	// BankIDTemplate is the template for the hindsight bank ID. Placeholders:
	// {namespace}, {agent-name}, {run-name} and {deployment}. Default
	// "{namespace}--{agent-name}". A per-run bank is never read again.
	// +optional
	BankIDTemplate string `json:"bankIdTemplate,omitempty"`
	// RecallBudget is the maximum number of memories recalled per turn.
	// +kubebuilder:default=5
	// +optional
	RecallBudget int `json:"recallBudget,omitempty"`
	// RecallMaxTokens caps the size of the recalled memories, in tokens
	// (hindsight's max_tokens). Default 1024.
	// +kubebuilder:validation:Minimum=64
	// +optional
	RecallMaxTokens int `json:"recallMaxTokens,omitempty"`
	// RetainOnEveryTurn is true to retain conversation after every LLM turn.
	// +kubebuilder:default=true
	// +optional
	RetainOnEveryTurn bool `json:"retainOnEveryTurn,omitempty"`
}

// AgentStatus defines the observed state of an Agent.
// Context tracking is handled at the AgentDeployment or AgentRun level,
// not on the template Agent resource itself.
type AgentStatus struct {
	// ServiceAccountName is the name of the stable SA managed by the operator.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// CloudProviderDetected is the cloud provider the operator detected at startup.
	// +optional
	CloudProviderDetected string `json:"cloudProviderDetected,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Selector",type=string,JSONPath=`.spec.modelSelectorRef`
// +kubebuilder:printcolumn:name="Framework",type=string,JSONPath=`.spec.runtime.framework`
// +kubebuilder:printcolumn:name="ServiceAccount",type=string,JSONPath=`.status.serviceAccountName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent defines a reusable agent configuration that can be instantiated as AgentRuns.
type Agent struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}

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

// AgentRunPhase describes the lifecycle phase of an AgentRun.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;HandedOff;WaitingForInput
type AgentRunPhase string

const (
	AgentRunPhasePending         AgentRunPhase = "Pending"
	AgentRunPhaseRunning         AgentRunPhase = "Running"
	AgentRunPhaseSucceeded       AgentRunPhase = "Succeeded"
	AgentRunPhaseFailed          AgentRunPhase = "Failed"
	AgentRunPhaseHandedOff       AgentRunPhase = "HandedOff"
	AgentRunPhaseWaitingForInput AgentRunPhase = "WaitingForInput"
)

// AgentRunSafeguards configures behavioral guardrails that halt a run when the agent
// appears stuck in a loop or is making no meaningful progress.
type AgentRunSafeguards struct {
	// MaxConsecutiveNoopTurns halts the run if the LLM produces this many consecutive
	// turns with no tool calls and fewer than MinSubstantiveTokens output tokens.
	// Not applied to chat (http input) runs. Unset uses the default of 15; a
	// negative value disables the check.
	// +optional
	MaxConsecutiveNoopTurns int `json:"maxConsecutiveNoopTurns,omitempty"`

	// MinSubstantiveTokens is the minimum number of output tokens required for a turn
	// to count as substantive. Only evaluated when MaxConsecutiveNoopTurns > 0.
	// +kubebuilder:default=20
	// +optional
	MinSubstantiveTokens int `json:"minSubstantiveTokens,omitempty"`

	// MaxRepeatedToolCalls halts the run if the same tool is called with identical
	// arguments this many times within the run. Unset uses the default of 50; a
	// negative value disables the check.
	// +optional
	MaxRepeatedToolCalls int `json:"maxRepeatedToolCalls,omitempty"`

	// ToolFrequencyCap halts the run if any single tool is called more than this many
	// times in total across the run. Unset (the default) or a negative value
	// disables the check.
	// +optional
	ToolFrequencyCap int `json:"toolFrequencyCap,omitempty"`

	// ToolExecutionTimeoutSec is the maximum number of seconds a single tool call may
	// take before it is cancelled and a structured error is returned to the LLM.
	// This prevents a hanging tool (e.g. blocking I/O, deadlock) from stalling the
	// entire run. Defaults to 60. 0 means use the default.
	// +kubebuilder:default=60
	// +optional
	ToolExecutionTimeoutSec int `json:"toolExecutionTimeoutSec,omitempty"`
}

// LoopDetectedInfo describes a safeguard trip that caused a run to fail.
type LoopDetectedInfo struct {
	// Reason is a human-readable description of which safeguard was tripped.
	Reason string `json:"reason"`

	// TripCount is the counter value that exceeded the configured limit.
	TripCount int `json:"tripCount"`

	// ToolName is the name of the tool involved, for tool-based trips.
	// +optional
	ToolName string `json:"toolName,omitempty"`
}

// WorkflowSandboxRef selects the existing workflow-owned sandbox in which the
// AgentRun's agent process runs as a detached job. The workflow controller sets
// it; ordinary AgentRuns leave it nil and create their own workload sandbox.
type WorkflowSandboxRef struct {
	// Name is the gateway sandbox name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// ID is the gateway sandbox ID, used for router ingress.
	// +optional
	ID string `json:"id,omitempty"`
	// Fresh is true for the step's dedicated sandbox. It is deleted when the last
	// run in a clarification chain ends; a shared sandbox is workflow-owned.
	// +optional
	Fresh bool `json:"fresh,omitempty"`
	// ProviderOwner is the original step AgentRun name used for tool credential
	// providers. Continuations retain it so their policy and providers still match.
	// +optional
	ProviderOwner string `json:"providerOwner,omitempty"`
}

// AgentRunSpec defines the desired state of an AgentRun.
type AgentRunSpec struct {
	// AgentRef names the Agent to run.
	// +kubebuilder:validation:MinLength=1
	AgentRef string `json:"agentRef"`

	// Input is the user task passed to the agent process via the AGENTORC_INPUT env var.
	// +kubebuilder:validation:MinLength=1
	Input string `json:"input"`

	// Timeout is the maximum duration for this run. Defaults to 5 minutes.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Callbacks configures webhooks invoked on run completion.
	// +optional
	Callbacks *CallbackConfig `json:"callbacks,omitempty"`

	// RestartPolicy controls failure recovery behaviour.
	// +optional
	RestartPolicy *AgentRunRestartPolicy `json:"restartPolicy,omitempty"`

	// ParentRunRef is the name of the parent AgentRun when this run was spawned
	// by an orchestrator agent via the agent-as-tool pattern.
	// Set automatically by the tool executor; do not set manually.
	// +optional
	ParentRunRef string `json:"parentRunRef,omitempty"`

	// PriorRunRef is the name of the previous AgentRun in this chat session.
	// When set, the model-router loads that run's accumulated message checkpoint as
	// the initial conversation history, enabling multi-turn context across runs.
	// Set automatically by the deployment chat API; do not set manually.
	// +optional
	PriorRunRef string `json:"priorRunRef,omitempty"`

	// Safeguards configures behavioral guardrails that halt the run when the agent
	// appears stuck in a loop or making no progress.
	// +optional
	Safeguards *AgentRunSafeguards `json:"safeguards,omitempty"`

	// Egress configures durable result delivery to an external message bus
	// (Kafka, Pub/Sub, or Redis stream) on terminal phase. When set, the
	// controller publishes the final TaskResponse to the configured sink.
	// Independent of webhook callbacks — both can be configured simultaneously.
	// +optional
	Egress *EgressConfig `json:"egress,omitempty"`

	// Budget caps the run's model spend. When the cap is reached the run fails or
	// pauses for approval, per budget.onExceeded. Without it the run is not capped
	// (the ModelSelector's budgetCap only steers routing).
	// +optional
	Budget *RunBudget `json:"budget,omitempty"`

	// WorkflowSandbox runs the agent in an existing workflow sandbox instead of
	// creating a sandbox whose main process is the agent. Set only by the
	// AgentWorkflow controller and inherited by clarification continuations.
	// +optional
	WorkflowSandbox *WorkflowSandboxRef `json:"workflowSandbox,omitempty"`
}

// ConditionToolApprovalPending is True on an AgentRun while a tool call waits
// for an approver (Tool.spec.sideEffect with GuardrailPolicy.spec.toolApproval).
const ConditionToolApprovalPending = "ToolApprovalPending"

// RunBudgetAction is what happens when a run reaches its budget.
// +kubebuilder:validation:Enum=fail;pause
type RunBudgetAction string

const (
	// RunBudgetFail halts the run, like a safeguard trip.
	RunBudgetFail RunBudgetAction = "fail"
	// RunBudgetPause holds the run (its agent keeps waiting, with sandbox and
	// workspace intact) and asks approvers to raise the cap or close it.
	RunBudgetPause RunBudgetAction = "pause"
)

// RunBudget caps an AgentRun's model spend.
type RunBudget struct {
	// MaxUSD is the spend, in USD, at which the run fails or pauses. Example: "2.50".
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	MaxUSD string `json:"maxUSD"`

	// WarnAtPercent is the share of the cap at which a warning is emitted (a
	// trace event and a Kubernetes event). Default 80.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=99
	// +kubebuilder:default=80
	// +optional
	WarnAtPercent int `json:"warnAtPercent,omitempty"`

	// OnExceeded is what happens at the cap: "fail" (default) halts the run;
	// "pause" holds it until an approver raises the cap or the pause times out.
	// Pausing needs PostgreSQL (approvals are stored there); without it a run that
	// reaches its cap fails.
	// +kubebuilder:default=fail
	// +optional
	OnExceeded RunBudgetAction `json:"onExceeded,omitempty"`

	// RaiseByUSD is how much one approval adds to the cap. Defaults to MaxUSD.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	RaiseByUSD string `json:"raiseByUSD,omitempty"`

	// PauseTimeout is how long a paused run waits for a decision before it fails.
	// Default 24h.
	// +optional
	PauseTimeout *metav1.Duration `json:"pauseTimeout,omitempty"`

	// ApproverGroups are the identity-provider groups whose members may decide a
	// raise. Empty lets any user with access to the run's namespace decide.
	// +optional
	ApproverGroups []string `json:"approverGroups,omitempty"`
}

// RunBudgetState is where a run stands against its budget.
// +kubebuilder:validation:Enum=ok;warning;exceeded
type RunBudgetState string

// Run budget states.
const (
	RunBudgetOK       RunBudgetState = "ok"
	RunBudgetWarning  RunBudgetState = "warning"
	RunBudgetExceeded RunBudgetState = "exceeded"
)

// RunBudgetStatus is the observed state of a run's budget.
type RunBudgetStatus struct {
	// State is ok, warning (past warnAtPercent) or exceeded (paused or failing).
	State RunBudgetState `json:"state"`

	// CapUSD is the current cap: spec.budget.maxUSD plus the approved raises. Only
	// the operator sets it, never the model-router.
	CapUSD string `json:"capUSD"`

	// SpentUSD is the spend the model-router last reported.
	// +optional
	SpentUSD string `json:"spentUSD,omitempty"`

	// Raises counts the approved raises.
	// +optional
	Raises int `json:"raises,omitempty"`

	// UpdatedAt is when the router last reported.
	// +optional
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
}

// CallbackConfig defines webhooks triggered on AgentRun completion.
type CallbackConfig struct {
	// OnComplete is a URL that receives a POST when the run succeeds.
	// +optional
	OnComplete string `json:"onComplete,omitempty"`

	// OnFailed is a URL that receives a POST when the run fails.
	// +optional
	OnFailed string `json:"onFailed,omitempty"`
}

// AgentRunRestartPolicy controls how failures are handled.
type AgentRunRestartPolicy struct {
	// MaxRetries is the number of times to restart a failed pod before marking the run Failed.
	// +kubebuilder:default=3
	// +optional
	MaxRetries int `json:"maxRetries,omitempty"`
}

// RoutingDecision records a single model routing decision made during an AgentRun.
type RoutingDecision struct {
	// Model is the selected model identifier (e.g. "anthropic/claude-sonnet-4-6").
	Model string `json:"model"`

	// Provider is the ModelProvider CRD name that was selected.
	Provider string `json:"provider"`

	// Strategy is the routing strategy used (e.g. "rule-based", "llm-meta", "hybrid").
	Strategy string `json:"strategy,omitempty"`

	// Reason is a human-readable explanation of why this provider was chosen.
	Reason string `json:"reason,omitempty"`

	// Confidence is a 0.0–1.0 score indicating routing confidence.
	// +optional
	Confidence string `json:"confidence,omitempty"`

	// Timestamp is when the routing decision was made.
	// +optional
	Timestamp *metav1.Time `json:"timestamp,omitempty"`

	// DecisionID identifies this decision across traces, status and telemetry.
	// +optional
	DecisionID string `json:"decisionID,omitempty"`

	// Path names what decided: a rule path, the meta-router, or a fallback.
	// +optional
	Path string `json:"path,omitempty"`

	// Category is the task stage the request was classified as.
	// +optional
	Category string `json:"category,omitempty"`

	// ParentDecisionID is the decision a fallback replaced.
	// +optional
	ParentDecisionID string `json:"parentDecisionID,omitempty"`
}

// AgentRunStatus defines the observed state of an AgentRun.
type AgentRunStatus struct {
	// Phase is the current lifecycle phase of this run.
	// +optional
	Phase AgentRunPhase `json:"phase,omitempty"`

	// PodName is the name of the agent pod for this run.
	// +optional
	PodName string `json:"podName,omitempty"`

	// RouterPodName is the name of the model-router pod when split-pod topology is active.
	// Empty when using the default combined-pod (sidecar) topology.
	// +optional
	RouterPodName string `json:"routerPodName,omitempty"`

	// SandboxName is the sandbox runtime's name for this run's sandbox when
	// spec.runtime.sandbox is set on the Agent. Empty otherwise.
	// +optional
	SandboxName string `json:"sandboxName,omitempty"`

	// SandboxGitTokenExpiry is when the run's current GitHub installation token
	// expires (spec.runtime.sandbox.github). The operator mints a new one before
	// then.
	// +optional
	SandboxGitTokenExpiry *metav1.Time `json:"sandboxGitTokenExpiry,omitempty"`

	// SandboxDecisionsSeen is the time of the last sandbox network decision
	// copied into the run's trace; the next read continues after it.
	// +optional
	SandboxDecisionsSeen *metav1.MicroTime `json:"sandboxDecisionsSeen,omitempty"`

	// StartTime is when the agent pod started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// SpendUSD is the total USD spend for this run so far, updated periodically.
	// +optional
	SpendUSD string `json:"spendUSD,omitempty"`

	// Output is the agent's final output captured from pod stdout.
	// Truncated to 10KB; full output is in the checkpoint store.
	// +optional
	Output string `json:"output,omitempty"`

	// RawOutput is the raw pod stdout, useful for debugging.
	// Truncated to 4KB.
	// +optional
	RawOutput string `json:"rawOutput,omitempty"`

	// CheckpointRef is the storage key of the latest conversation checkpoint.
	// Used by the controller to resume a failed run.
	// +optional
	CheckpointRef string `json:"checkpointRef,omitempty"`

	// RestartCount is the number of times this run's pod has been restarted.
	// +optional
	RestartCount int `json:"restartCount,omitempty"`

	// LastRestartReason describes why the pod was last restarted.
	// +optional
	LastRestartReason string `json:"lastRestartReason,omitempty"`

	// ChildRunRefs lists the names of child AgentRuns spawned by this run.
	// +optional
	ChildRunRefs []string `json:"childRunRefs,omitempty"`

	// HandoffTarget is the Agent name this run handed off to, if Phase is HandedOff.
	// +optional
	HandoffTarget string `json:"handoffTarget,omitempty"`

	// ClarifyQuestion is the question posed to the human when Phase is WaitingForInput.
	// +optional
	ClarifyQuestion string `json:"clarifyQuestion,omitempty"`

	// ClarifyAnswer is the human's response, set via the UI API.
	// +optional
	ClarifyAnswer string `json:"clarifyAnswer,omitempty"`

	// WaitingSince records when the run entered WaitingForInput.
	// Used to enforce a waiting timeout separate from the run timeout.
	// +optional
	WaitingSince *metav1.Time `json:"waitingSince,omitempty"`

	// ContinuationRunRef is the name of the follow-up AgentRun created when
	// the human answers a clarifying question. The original run stays in
	// WaitingForInput (for audit) and the continuation carries the work forward.
	// +optional
	ContinuationRunRef string `json:"continuationRunRef,omitempty"`

	// Budget is the run's standing against spec.budget.
	// +optional
	Budget *RunBudgetStatus `json:"budget,omitempty"`

	// LoopDetected is set when the run was halted by a safeguard trip.
	// The run phase will be Failed when this is set.
	// +optional
	LoopDetected *LoopDetectedInfo `json:"loopDetected,omitempty"`

	// FailureReason is a human-readable explanation of why the run failed,
	// set by the agent via the _fail built-in tool.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// InputMode mirrors Agent.spec.runtime.inputMode, cached here so the controller
	// does not need to re-fetch the Agent in handlePodSuccess.
	// +optional
	InputMode string `json:"inputMode,omitempty"`

	// EpisodicChunks is the number of episodic memory summaries generated during this run.
	// +optional
	EpisodicChunks int `json:"episodicChunks,omitempty"`

	// RoutingDecisions records the model routing decisions made during this run.
	// Populated by the controller from the resolved ModelSelector configuration
	// and updated by the model-router sidecar at runtime. It holds the most
	// recent decisions only (see RoutingDecisionsTotal).
	// +optional
	RoutingDecisions []RoutingDecision `json:"routingDecisions,omitempty"`

	// RoutingDecisionsTotal counts every decision recorded during this run,
	// including those that have rolled out of RoutingDecisions.
	// +optional
	RoutingDecisionsTotal int `json:"routingDecisionsTotal,omitempty"`

	// ContextUsedTokens is the estimated token count of the conversation context.
	// Updated by the model-router at runtime and reflects the current size of the
	// accumulated message history.
	// +optional
	ContextUsedTokens int `json:"contextUsedTokens,omitempty"`

	// MaxContextTokens is the maximum context window size for the selected model.
	// Used to calculate how much context is available for the conversation.
	// +optional
	MaxContextTokens int `json:"maxContextTokens,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Context",type=integer,JSONPath=`.status.contextUsedTokens`
// +kubebuilder:printcolumn:name="Spend",type=string,JSONPath=`.status.spendUSD`
// +kubebuilder:printcolumn:name="Restarts",type=integer,JSONPath=`.status.restartCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentRun is an instantiation of an Agent executing a specific task.
type AgentRun struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentRunSpec   `json:"spec,omitempty"`
	Status AgentRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentRunList contains a list of AgentRun.
type AgentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRun{}, &AgentRunList{})
}

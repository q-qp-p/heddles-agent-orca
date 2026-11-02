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

// Benchmark modes.
const (
	// BenchmarkModeReportOnly produces recommendations that a person applies.
	BenchmarkModeReportOnly = "reportOnly"
	// BenchmarkModeProposeApply raises an approval request for each recommendation.
	BenchmarkModeProposeApply = "proposeApply"
)

// Shadow arm kinds.
const (
	// ShadowArmsCounterfactual replays the call on the provider the selector did
	// not choose: the other tier of a two-tier algorithm.
	ShadowArmsCounterfactual = "counterfactual"
	// ShadowArmsProviders replays the call on each provider in shadow.providers.
	ShadowArmsProviders = "providers"
)

// RoutingBenchmarkSpec asks agent-orca to measure how well routing serves a set
// of Agents: to replay a sample of their calls on another model, have a judge
// compare the two answers, and report whether each routing choice was worth its
// cost. Evidence is kept per Agent, whether the work ran as an AgentRun, an
// AgentDeployment or a workflow step, so it builds up across runs.
// +kubebuilder:validation:XValidation:rule="(has(self.agentRefs) && size(self.agentRefs) > 0) || has(self.agentSelector)",message="name the Agents to benchmark with agentRefs or agentSelector (an empty agentSelector matches every Agent)"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'proposeApply' || (has(self.approverGroups) && size(self.approverGroups) > 0)",message="approverGroups is required when mode is proposeApply"
// +kubebuilder:validation:XValidation:rule="!has(self.shadow) || !has(self.shadow.enabled) || !self.shadow.enabled || has(self.judge)",message="judge is required when shadow sampling is enabled"
// +kubebuilder:validation:XValidation:rule="!has(self.shadow) || !has(self.shadow.arms) || self.shadow.arms != 'providers' || (has(self.shadow.providers) && size(self.shadow.providers) > 0)",message="shadow.providers is required when shadow.arms is providers"
type RoutingBenchmarkSpec struct {
	// AgentRefs names Agents, in this namespace, to benchmark.
	// +listType=set
	// +optional
	AgentRefs []string `json:"agentRefs,omitempty"`

	// AgentSelector picks Agents, in this namespace, by label. An empty selector
	// matches every Agent. An Agent named in agentRefs is benchmarked as well.
	// +optional
	AgentSelector *metav1.LabelSelector `json:"agentSelector,omitempty"`

	// Mode says what happens to a recommendation. reportOnly leaves it for a
	// person to read and apply. proposeApply raises an approval request for it,
	// and applies it only once approved.
	// +kubebuilder:validation:Enum=reportOnly;proposeApply
	// +kubebuilder:default=reportOnly
	// +optional
	Mode string `json:"mode,omitempty"`

	// Shadow configures the counterfactual calls. Without it the benchmark reads
	// passive outcomes and feedback only.
	// +optional
	Shadow *ShadowSpec `json:"shadow,omitempty"`

	// Judge is the model that compares a served answer with its shadow answer.
	// +optional
	Judge *JudgeSpec `json:"judge,omitempty"`

	// Budget caps what shadow and judge calls may spend. It does not cap the
	// runs themselves; those keep the selector's own budget.
	Budget BenchmarkBudget `json:"budget"`

	// Evaluation says how results are grouped and how much evidence is enough.
	// +optional
	Evaluation *EvaluationSpec `json:"evaluation,omitempty"`

	// Thresholds say when a difference is worth acting on.
	// +optional
	Thresholds *BenchmarkThresholds `json:"thresholds,omitempty"`

	// ApproverGroups are the groups allowed to approve a recommendation. Required
	// when mode is proposeApply.
	// +optional
	ApproverGroups []string `json:"approverGroups,omitempty"`

	// StoreTranscripts keeps the prompt and the two answers of each sampled call so
	// a person can read the judge's reasoning. Off by default: without it only the
	// verdict, its scores and the cost are stored.
	// +kubebuilder:default=false
	// +optional
	StoreTranscripts bool `json:"storeTranscripts,omitempty"`
}

// ShadowSpec configures counterfactual sampling.
type ShadowSpec struct {
	// Enabled turns sampling on.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// SampleRate is the fraction of calls to replay, from 0 to 1. The choice is
	// made from the decision ID, so the same call is always in or out.
	// +kubebuilder:validation:Pattern=`^(0(\.\d+)?|1(\.0+)?)$`
	// +kubebuilder:default="0.05"
	// +optional
	SampleRate string `json:"sampleRate,omitempty"`

	// Arms says which models a sampled call is replayed on.
	// +kubebuilder:validation:Enum=counterfactual;providers
	// +kubebuilder:default=counterfactual
	// +optional
	Arms string `json:"arms,omitempty"`

	// Providers are the ModelProviders to replay on when arms is providers.
	// +optional
	Providers []string `json:"providers,omitempty"`

	// MaxPerRun is the most calls sampled from one AgentRun. It does not apply
	// to an AgentDeployment, which keeps running; maxPerHour bounds that.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	// +optional
	MaxPerRun int32 `json:"maxPerRun,omitempty"`

	// MaxPerHour is the most calls sampled per hour for each Agent, across all
	// its runs and deployments. It stops one busy Agent using the whole budget.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=20
	// +optional
	MaxPerHour int32 `json:"maxPerHour,omitempty"`

	// ExcludeRunSelector skips AgentRuns whose labels match it, for example runs
	// labelled as experiments. Deployments are never excluded this way.
	// +optional
	ExcludeRunSelector *metav1.LabelSelector `json:"excludeRunSelector,omitempty"`

	// IncludeToolTurns samples calls that end in tool calls too. The replay never
	// runs a tool: the judge compares which tools each model chose to call.
	// +kubebuilder:default=true
	// +optional
	IncludeToolTurns *bool `json:"includeToolTurns,omitempty"`
}

// JudgeSpec configures the pairwise judge.
type JudgeSpec struct {
	// ProviderRef is the ModelProvider, in this namespace, that judges. It should
	// not be one of the models being compared; JudgeIsArm warns when it is.
	// +kubebuilder:validation:MinLength=1
	ProviderRef string `json:"providerRef"`

	// PositionSwap asks the judge twice with the two answers in opposite order and
	// counts a disagreement as a tie, which removes position bias.
	// +kubebuilder:default=true
	// +optional
	PositionSwap *bool `json:"positionSwap,omitempty"`

	// Rubric replaces the default grading instructions.
	// +kubebuilder:validation:MaxLength=4000
	// +optional
	Rubric string `json:"rubric,omitempty"`

	// MaxTokens bounds one judge reply.
	// +kubebuilder:validation:Minimum=64
	// +kubebuilder:default=512
	// +optional
	MaxTokens int32 `json:"maxTokens,omitempty"`
}

// BenchmarkBudget caps benchmarking spend in USD.
type BenchmarkBudget struct {
	// PerDayUSD is the most shadow and judge calls may spend in a UTC day,
	// for example "2.00". Sampling stops for the day once it is reached.
	// +kubebuilder:validation:Pattern=`^\d+(\.\d{1,6})?$`
	PerDayUSD string `json:"perDayUSD"`

	// PerRunUSD is the most they may spend on one run. A deployment counts as one
	// run for each UTC day.
	// +kubebuilder:validation:Pattern=`^\d+(\.\d{1,6})?$`
	// +optional
	PerRunUSD string `json:"perRunUSD,omitempty"`
}

// EvaluationSpec says how results are grouped and how much is enough.
type EvaluationSpec struct {
	// Window is how far back results count.
	// +kubebuilder:default="168h"
	// +optional
	Window *metav1.Duration `json:"window,omitempty"`

	// Interval is how often the benchmark is re-evaluated.
	// +kubebuilder:default="6h"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// MinSamples is the judged pairs needed per category and arm pair before a
	// finding is reported.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=30
	// +optional
	MinSamples int32 `json:"minSamples,omitempty"`

	// GroupBy picks the task categories results are grouped by within each
	// Agent: stage, workflowStep or syLabel. Results are always kept per Agent.
	// +kubebuilder:default={"stage"}
	// +optional
	GroupBy []string `json:"groupBy,omitempty"`

	// ConfidenceLevel is the confidence of the intervals reported.
	// +kubebuilder:validation:Pattern=`^0\.\d+$`
	// +kubebuilder:default="0.95"
	// +optional
	ConfidenceLevel string `json:"confidenceLevel,omitempty"`
}

// BenchmarkThresholds say when a difference is worth acting on.
type BenchmarkThresholds struct {
	// NonInferiorityMargin is how much worse than the capable model the cheaper one
	// may be, as a win-rate difference, and still count as equal.
	// +kubebuilder:validation:Pattern=`^0(\.\d+)?$`
	// +kubebuilder:default="0.05"
	// +optional
	NonInferiorityMargin string `json:"nonInferiorityMargin,omitempty"`

	// MinNetWinDelta is the smallest net win rate worth reporting.
	// +kubebuilder:validation:Pattern=`^0(\.\d+)?$`
	// +optional
	MinNetWinDelta string `json:"minNetWinDelta,omitempty"`

	// MaxUSDPerNetWin is the most you would pay for one extra judged win. Unset,
	// the cost per net win is reported but no recommendation is made from it.
	// +kubebuilder:validation:Pattern=`^\d+(\.\d{1,6})?$`
	// +optional
	MaxUSDPerNetWin string `json:"maxUSDPerNetWin,omitempty"`

	// DeadProviderErrorRate is the error rate above which a provider is reported
	// as dead.
	// +kubebuilder:validation:Pattern=`^(0(\.\d+)?|1(\.0+)?)$`
	// +kubebuilder:default="0.5"
	// +optional
	DeadProviderErrorRate string `json:"deadProviderErrorRate,omitempty"`
}

// RoutingBenchmark condition types.
const (
	// BenchmarkConditionReady says the benchmark is valid and sampling can run.
	BenchmarkConditionReady = "Ready"
	// BenchmarkConditionPostgresUnavailable says the operator has no Postgres, so
	// nothing can be stored or evaluated.
	BenchmarkConditionPostgresUnavailable = "PostgresUnavailable"
	// BenchmarkConditionJudgeIsArm warns that the judge is also compared.
	BenchmarkConditionJudgeIsArm = "JudgeIsArm"
)

// CategorySummary is what the benchmark found for one task category of one Agent.
type CategorySummary struct {
	// Agent is the Agent whose calls these are.
	Agent string `json:"agent"`
	// Category is the group key, such as "stage:tool-exec".
	Category string `json:"category"`
	// Samples is the judged pairs in the window.
	Samples int32 `json:"samples"`
	// ServedWins, ShadowWins and Ties count the judge's verdicts.
	ServedWins int32 `json:"servedWins,omitempty"`
	ShadowWins int32 `json:"shadowWins,omitempty"`
	Ties       int32 `json:"ties,omitempty"`
}

// RoutingBenchmarkStatus is the observed state of a RoutingBenchmark.
type RoutingBenchmarkStatus struct {
	// ObservedGeneration is the spec generation the conditions describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SampledCalls, JudgedPairs and InconsistentJudgments count results in the window.
	// +optional
	SampledCalls int32 `json:"sampledCalls,omitempty"`
	// +optional
	JudgedPairs int32 `json:"judgedPairs,omitempty"`
	// +optional
	InconsistentJudgments int32 `json:"inconsistentJudgments,omitempty"`

	// JudgeInconsistentRate is InconsistentJudgments over JudgedPairs, as a
	// decimal string. A high rate means the judge cannot tell the answers apart.
	// +optional
	JudgeInconsistentRate string `json:"judgeInconsistentRate,omitempty"`

	// ShadowSpendTodayUSD is what shadow and judge calls have cost today (UTC).
	// +optional
	ShadowSpendTodayUSD string `json:"shadowSpendTodayUSD,omitempty"`

	// BudgetExhausted is true once today's budget is spent.
	// +optional
	BudgetExhausted bool `json:"budgetExhausted,omitempty"`

	// MatchedAgents are the Agents the benchmark covers now, up to 100.
	// +kubebuilder:validation:MaxItems=100
	// +listType=set
	// +optional
	MatchedAgents []string `json:"matchedAgents,omitempty"`

	// Categories summarises each task category of each Agent, the best sampled
	// first, up to 200.
	// +kubebuilder:validation:MaxItems=200
	// +listType=map
	// +listMapKey=agent
	// +listMapKey=category
	// +optional
	Categories []CategorySummary `json:"categories,omitempty"`

	// LastEvaluated is when the results were last read.
	// +optional
	LastEvaluated *metav1.Time `json:"lastEvaluated,omitempty"`

	// Conditions holds Ready, PostgresUnavailable and JudgeIsArm.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Pairs",type=integer,JSONPath=`.status.judgedPairs`
// +kubebuilder:printcolumn:name="Spend",type=string,JSONPath=`.status.shadowSpendTodayUSD`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RoutingBenchmark measures how well routing serves a set of Agents, and what
// each routing choice costs against what it buys.
type RoutingBenchmark struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RoutingBenchmarkSpec   `json:"spec,omitempty"`
	Status RoutingBenchmarkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RoutingBenchmarkList contains a list of RoutingBenchmark.
type RoutingBenchmarkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RoutingBenchmark `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RoutingBenchmark{}, &RoutingBenchmarkList{})
}

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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/mcp"
	"github.com/heddles/agent-orca/internal/podbuilder"
	"github.com/heddles/agent-orca/internal/router"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/state"
)

// AgentDeploymentReconciler reconciles an AgentDeployment object.
type AgentDeploymentReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Recorder         record.EventRecorder
	K8s              kubernetes.Interface
	ModelRouterImage string
	// StateConfig is injected into router configs. The operator strips the Redis
	// password from it; RedisPassword carries it to a mounted Secret instead.
	StateConfig   state.Config
	RedisPassword string
	CloudProvider security.CloudProvider
	// TokenReviewerClusterRole is the ClusterRole that grants "create tokenreviews".
	TokenReviewerClusterRole string
	// OperatorAPIURL is the base URL of the operator's internal API server.
	// Injected into the router config so the model-router can call RAG, handoff, etc.
	OperatorAPIURL string
	// LLMRequestTimeout is the per-LLM-request timeout written into every router
	// config (default 1h). Injected from the LLM_REQUEST_TIMEOUT env var.
	LLMRequestTimeout time.Duration
	// ContextCompactionThreshold is the fraction of a provider's window at
	// which the router compacts the conversation (default 0.8, #47). Injected
	// from the CONTEXT_COMPACTION_THRESHOLD env var.
	ContextCompactionThreshold float64
	// ContextCompactionMode is "summary" (default) or "truncate", the rollback
	// switch (#47). Injected from the CONTEXT_COMPACTION_MODE env var.
	ContextCompactionMode string
	// ContextCompactionRatio is the fraction of the window drop-oldest
	// truncation cuts down to ("truncate" mode and the summary fallback).
	// Injected from the deprecated CONTEXT_COMPACTION_RATIO env var.
	ContextCompactionRatio float64

	// HindsightURL is the configured hindsight API endpoint (e.g. http://hindsight.local).
	// Injected from the HINDSIGHT_URL env var. Used to generate ipBlock egress rules
	// in the per-run NetworkPolicies when the cluster has a default-deny egress policy.
	HindsightURL string
	// HindsightAPIKey authenticates the model-router to a hindsight API that
	// requires a tenant API key. Injected from the HINDSIGHT_API_KEY env var; the
	// operator mirrors it into each run namespace (see ensureHindsightKeySecret).
	HindsightAPIKey string
}

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agents,verbs=get;list
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get

// Reconcile implements the reconciliation loop for AgentDeployment.
func (r *AgentDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := slog.With("agentdeployment", req.NamespacedName)

	var deployment agentorcav1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error("failed to fetch AgentDeployment", "error", err)
		return ctrl.Result{}, err
	}
	// Snapshot for status patch — avoids optimistic concurrency conflicts
	// that occur when concurrent reconciliations race on status updates.
	statusBase := deployment.DeepCopy()

	// Set initial phase if not set
	if deployment.Status.Phase == "" {
		deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhaseCreating
		deployment.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
	}

	// Verify the referenced Agent exists
	var agent agentorcav1alpha1.Agent
	if err := r.Get(ctx, types.NamespacedName{
		Name:      deployment.Spec.AgentRef,
		Namespace: deployment.Namespace,
	}, &agent); err != nil {
		log.Error("referenced Agent not found", "agent", deployment.Spec.AgentRef)
		deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("Agent %s not found", deployment.Spec.AgentRef)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Deployment and warm-pool pods are plain agent pods. Running a sandboxed
	// Agent in them would silently drop its sandbox, so refuse instead.
	if agent.Spec.Runtime.Sandbox != nil {
		deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("Agent %s uses spec.runtime.sandbox, which AgentDeployments do not support yet; run it as AgentRuns", agent.Name)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{}, nil
	}

	saName := security.AgentSAName(agent.Name)
	if agent.Spec.ServiceAccountRef != nil {
		saName = agent.Spec.ServiceAccountRef.Name
	}

	// Resolve MCP sidecar image volumes (must run before buildDeploymentRouterConfig
	// so the binary path rewrites are available for the router config).
	mcpBinVolumes, mcpBinMounts, binPathRewrites, err := podbuilder.ResolveMCPSidecarVolumes(ctx, r.Client, deployment.Namespace, &agent)
	if err != nil {
		deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("resolving MCP sidecar volumes: %v", err)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Resolve tool secret volumes (MCP envFrom secrets).
	toolSecretVolumes, toolSecretMounts, err := podbuilder.ResolveToolSecretVolumes(ctx, r.Client, deployment.Namespace, &agent)
	if err != nil {
		deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("resolving tool secret volumes: %v", err)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Resolve ModelSelector + tools → build router config.
	routerCfg, err := r.buildDeploymentRouterConfig(ctx, &deployment, &agent, saName, binPathRewrites)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("router config dependency not ready, retrying", "error", err)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("building router config: %v", err)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Ensure router config ConfigMap.
	routerConfigHash, err := r.ensureDeploymentRouterConfigMap(ctx, &deployment, routerCfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure per-deployment token Secret.
	// The Redis password Secret is shared by the deployment's pods and its warm pool.
	if err := ensureRedisPasswordSecret(ctx, r.Client, r.Scheme, &deployment,
		redisSecretName("agentorca-deploy-"+deployment.Name, r.RedisPassword), r.RedisPassword); err != nil {
		return ctrl.Result{}, err
	}

	tokenSecretName, err := r.ensureDeploymentTokenSecret(ctx, &deployment, saName)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure TokenReviewer ClusterRoleBinding so the model-router sidecar can validate tokens.
	if err := r.ensureDeploymentTokenReviewerBinding(ctx, &deployment, saName); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve provider secret volumes.
	providerVolumes, providerMounts, err := podbuilder.ResolveProviderVolumes(ctx, r.Client, deployment.Namespace, &agent)
	if err != nil {
		deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("resolving provider volumes: %v", err)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Create or update the underlying Kubernetes Deployment.
	if err := r.reconcileDeployment(ctx, &deployment, &agent, saName, tokenSecretName, routerConfigHash, routerCfg.Strategy, providerVolumes, providerMounts, toolSecretVolumes, toolSecretMounts, mcpBinVolumes, mcpBinMounts); err != nil {
		log.Error("failed to reconcile Deployment", "error", err)
		deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = err.Error()
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Sync pod status from underlying Deployment
	if err := r.syncPodStatus(ctx, &deployment); err != nil {
		log.Error("failed to sync pod status", "error", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	// Reconcile warm pod pool for fast chat dispatch.
	if deployment.Spec.WarmPoolSize > 0 {
		if err := r.reconcileWarmPool(ctx, &deployment, &agent, saName, routerConfigHash, routerCfg, providerVolumes, providerMounts, toolSecretVolumes, toolSecretMounts, mcpBinVolumes, mcpBinMounts); err != nil {
			log.Error("failed to reconcile warm pool", "error", err)
			// Non-fatal: continue with status update.
		}
	} else {
		deployment.Status.WarmPoolReady = 0
	}

	// Check if we should pause due to too many failures
	restartPolicy := deployment.Spec.RestartPolicy
	if restartPolicy != nil && restartPolicy.MaxConsecutiveFailures > 0 {
		if deployment.Status.ConsecutiveFailures >= restartPolicy.MaxConsecutiveFailures {
			deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhasePaused
			deployment.Status.Message = fmt.Sprintf(
				"Paused after %d consecutive failures (max: %d)",
				deployment.Status.ConsecutiveFailures,
				restartPolicy.MaxConsecutiveFailures,
			)
			_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
	}

	// Update phase to Running if pods are ready
	if deployment.Status.ReadyReplicas > 0 {
		deployment.Status.Phase = agentorcav1alpha1.AgentDeploymentPhaseRunning
		deployment.Status.Message = ""
	}

	deployment.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
	if err := r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase)); err != nil {
		log.Error("failed to update status", "error", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileDeployment creates or updates the underlying K8s Deployment with the
// model-router sidecar, provider volumes, token secret, and security hardening.
func (r *AgentDeploymentReconciler) reconcileDeployment(
	ctx context.Context,
	agentDeploy *agentorcav1alpha1.AgentDeployment,
	agent *agentorcav1alpha1.Agent,
	saName string,
	tokenSecretName string,
	routerConfigHash string,
	routerStrategy string,
	providerVolumes []corev1.Volume,
	providerMounts []corev1.VolumeMount,
	toolSecretVolumes []corev1.Volume,
	toolSecretMounts []corev1.VolumeMount,
	mcpBinVolumes []corev1.Volume,
	mcpBinMounts []corev1.VolumeMount,
) error {
	deploymentName := agentDeploy.Name
	namespace := agentDeploy.Namespace
	if err := ensureHindsightKeySecret(ctx, r.Client, namespace, r.HindsightAPIKey); err != nil {
		return err
	}

	// Get or create the Kubernetes Deployment
	var k8sDeploy appsv1.Deployment
	k8sDeployKey := types.NamespacedName{Name: deploymentName, Namespace: namespace}

	isNew := false
	if err := r.Get(ctx, k8sDeployKey, &k8sDeploy); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		isNew = true
	}

	// Build the deployment spec
	replicas := agentDeploy.Spec.Replicas
	if replicas == nil {
		one := int32(1)
		replicas = &one
	}

	selector := map[string]string{
		"app.kubernetes.io/name":       "agent-orca",
		"app.kubernetes.io/component":  "agent-deployment",
		"agentdeployment.agentorca.io": agentDeploy.Name,
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       "agent-orca",
		"app.kubernetes.io/component":  "agent-deployment",
		"agentdeployment.agentorca.io": agentDeploy.Name,
		"agent.agentorca.io":           agent.Name,
		security.LabelManagedBy:        security.ManagedByValue,
	}

	inputSourceType := agentorcav1alpha1.InputSourceChat
	if agentDeploy.Spec.InputSource != nil {
		inputSourceType = agentDeploy.Spec.InputSource.Type
	}

	agentEnv := []corev1.EnvVar{
		{Name: "AGENTORC_AGENT", Value: agent.Name},
		{Name: "AGENTORC_NAMESPACE", Value: namespace},
		{Name: "AGENTORC_INPUT_SOURCE", Value: string(inputSourceType)},
	}

	agentSecretVolumes, agentSecretMounts := podbuilder.ResolveAgentSecretRefs(agent)

	pod := podbuilder.Build(podbuilder.PodConfig{
		RouterStrategy:           routerStrategy,
		HindsightAPIKeySecretRef: hindsightKeyRef(r.HindsightAPIKey),
		Namespace:                namespace,
		Labels:                   labels,
		Annotations: map[string]string{
			"agentorca.io/router-config-hash": routerConfigHash,
		},
		Agent:              agent,
		AgentEnv:           agentEnv,
		TokenSecretName:    tokenSecretName,
		ServiceAccount:     saName,
		RestartPolicy:      corev1.RestartPolicyAlways,
		ModelRouterImage:   r.ModelRouterImage,
		RouterConfigName:   "agentorca-deploy-" + agentDeploy.Name,
		RedisSecretName:    redisSecretName("agentorca-deploy-"+agentDeploy.Name, r.RedisPassword),
		ProviderVolumes:    providerVolumes,
		ProviderMounts:     providerMounts,
		ToolSecretVolumes:  toolSecretVolumes,
		ToolSecretMounts:   toolSecretMounts,
		MCPBinVolumes:      mcpBinVolumes,
		MCPBinMounts:       mcpBinMounts,
		AgentSecretVolumes: agentSecretVolumes,
		AgentSecretMounts:  agentSecretMounts,
		CloudProvider:      r.CloudProvider,
	})

	if isNew {
		k8sDeploy = appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
				Labels:    labels,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(agentDeploy, agentorcav1alpha1.GroupVersion.WithKind("AgentDeployment")),
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: replicas,
				Selector: &metav1.LabelSelector{MatchLabels: selector},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: pod.ObjectMeta,
					Spec:       pod.Spec,
				},
			},
		}
		if err := r.Create(ctx, &k8sDeploy); err != nil {
			return fmt.Errorf("creating Deployment: %w", err)
		}
	} else {
		// Update existing deployment spec. Setting the router-config-hash annotation
		// on the pod template ensures a rollout when the router config changes even
		// if the pod spec itself is unchanged (e.g. only ModelSelector weights changed).
		// Use Patch instead of Update to avoid optimistic concurrency conflicts when
		// the Deployment controller modifies the object concurrently.
		deployBase := k8sDeploy.DeepCopy()
		k8sDeploy.Spec.Replicas = replicas
		k8sDeploy.Spec.Template.Labels = pod.Labels
		k8sDeploy.Spec.Template.Annotations = pod.Annotations
		k8sDeploy.Spec.Template.Spec = pod.Spec
		if err := r.Patch(ctx, &k8sDeploy, client.MergeFrom(deployBase)); err != nil {
			return fmt.Errorf("updating Deployment: %w", err)
		}
	}

	return nil
}

// syncPodStatus reads the underlying Deployment's status and updates AgentDeployment.
func (r *AgentDeploymentReconciler) syncPodStatus(
	ctx context.Context,
	agentDeploy *agentorcav1alpha1.AgentDeployment,
) error {
	var k8sDeploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{
		Name:      agentDeploy.Name,
		Namespace: agentDeploy.Namespace,
	}, &k8sDeploy); err != nil {
		return err
	}

	// Sync replica counts
	agentDeploy.Status.ReadyReplicas = k8sDeploy.Status.ReadyReplicas
	agentDeploy.Status.AvailableReplicas = k8sDeploy.Status.AvailableReplicas

	// List pods for this deployment
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(agentDeploy.Namespace),
		client.MatchingLabels{
			"agentdeployment.agentorca.io": agentDeploy.Name,
		},
	); err != nil {
		return err
	}

	agentDeploy.Status.PodNames = make([]string, 0, len(pods.Items))
	var consecutiveFailures int
	for _, pod := range pods.Items {
		agentDeploy.Status.PodNames = append(agentDeploy.Status.PodNames, pod.Name)
		// Check if pod is in failed state
		if pod.Status.Phase == corev1.PodFailed {
			consecutiveFailures++
			agentDeploy.Status.LastFailureTime = &metav1.Time{Time: time.Now()}
		}
	}

	// Update consecutive failures count (resets if any pod is successful)
	if consecutiveFailures > 0 && consecutiveFailures == len(pods.Items) {
		agentDeploy.Status.ConsecutiveFailures = consecutiveFailures
	} else if len(pods.Items) > 0 {
		agentDeploy.Status.ConsecutiveFailures = 0
	}

	return nil
}

// buildDeploymentRouterConfig resolves ModelSelector + Tools for an AgentDeployment.
func (r *AgentDeploymentReconciler) buildDeploymentRouterConfig( //nolint:gocyclo

	ctx context.Context,
	deploy *agentorcav1alpha1.AgentDeployment,
	agent *agentorcav1alpha1.Agent,
	saName string,
	binPathRewrites map[string]string,
) (*router.Config, error) {
	var selector agentorcav1alpha1.ModelSelector
	if err := r.Get(ctx, client.ObjectKey{Name: agent.Spec.ModelSelectorRef, Namespace: deploy.Namespace}, &selector); err != nil {
		return nil, fmt.Errorf("getting ModelSelector %q: %w", agent.Spec.ModelSelectorRef, err)
	}

	routing, err := resolveSelectorRouting(ctx, r.Client, deploy.Namespace, &selector)
	if err != nil {
		return nil, err
	}
	for _, name := range routing.SkippedFallbacks {
		if r.Recorder != nil {
			r.Recorder.Eventf(deploy, corev1.EventTypeWarning, "FallbackProviderSkipped",
				"fallback provider %q not deployed; skipped from chain", name)
		}
	}
	providers := routing.Providers

	var toolDefs []router.ToolDefinition
	var mcpServers []router.MCPServerConfig
	seenMCPServers := make(map[string]bool)
	// Builtin tools are injected automatically and don't exist as Tool CRDs
	builtinTools := map[string]bool{
		"_spawn": true, "_handoff": true, "_clarify": true, "_done": true,
		"_fail": true, "_emit_event": true, "_rag_ingest": true, "_rag_search": true,
		"_write_state": true, "_read_state": true, "_list_state": true, "_delete_state": true,
		"_memory_store": true, "_mcp_read_resource": true, "_propose_step": true,
		"_propose_fix": true, "_confirm_fix": true, "_create_workflow": true,
		"_search_history": true,
		// fs_* relay tools (issue #34) are injected from Agent.spec.filesystem;
		// listing them in spec.tools must not require Tool CRs.
		"fs_read_text_file": true, "fs_write_text_file": true, "fs_list_dir": true,
	}
	for _, toolName := range agent.Spec.Tools {
		// Skip builtin tools - they're injected separately in the router
		if builtinTools[toolName] {
			continue
		}
		var tool agentorcav1alpha1.Tool
		if err := r.Get(ctx, client.ObjectKey{Name: toolName, Namespace: deploy.Namespace}, &tool); err != nil {
			return nil, annotatedToolError(ctx, r.Client, deploy.Namespace, toolName, err)
		}
		var params json.RawMessage
		if tool.Spec.Schema != nil && tool.Spec.Schema.Input != nil {
			params = tool.Spec.Schema.Input.Raw
		}
		desc := ""
		if tool.Spec.Schema != nil {
			desc = tool.Spec.Schema.Description
		}
		backendType := string(tool.Spec.Type)
		if backendType == "" {
			backendType = "regular"
		}
		backendRef := tool.Spec.OCIRef
		if tool.Spec.Type == agentorcav1alpha1.ToolTypeAgent {
			backendRef = tool.Spec.AgentRef
		}
		if tool.Spec.Type == agentorcav1alpha1.ToolTypeMCP && tool.Spec.MCPConfig != nil {
			serverName := tool.Labels[LabelMCPServer]
			if serverName == "" || !seenMCPServers[serverName] {
				if serverName != "" {
					seenMCPServers[serverName] = true
				}
				mcpCfg := router.MCPServerConfig{
					Name:      toolName,
					Transport: tool.Spec.MCPConfig.Transport,
					URL:       tool.Spec.MCPConfig.URL,
				}
				if serverName != "" {
					var mcpServer agentorcav1alpha1.MCPServer
					if err := r.Get(ctx, client.ObjectKey{Name: serverName, Namespace: deploy.Namespace}, &mcpServer); err == nil {
						mcpCfg.AllowApps = mcpServer.Spec.AllowApps
						mcpCfg.IncludePatterns = mcpServer.Spec.IncludePatterns
						mcpCfg.ExcludePatterns = mcpServer.Spec.ExcludePatterns
					}
				}
				args := tool.Spec.MCPConfig.Args
				if tool.Spec.MCPConfig.Transport == "stdio" && len(args) > 0 { //nolint:goconst

					mcpCfg.Cmd = args[0]
					mcpCfg.Args = args[1:]
					// Rewrite absolute paths to point into the image volume mount.
					if mountDir, ok := binPathRewrites[toolName]; ok {
						if strings.HasPrefix(mcpCfg.Cmd, "/") {
							mcpCfg.Cmd = mountDir + mcpCfg.Cmd
						}
						for i, a := range mcpCfg.Args {
							if strings.HasPrefix(a, "/") {
								mcpCfg.Args[i] = mountDir + a
							}
						}
					}
				}
				envKeys := make([]string, 0, len(tool.Spec.MCPConfig.Env))
				for k := range tool.Spec.MCPConfig.Env {
					envKeys = append(envKeys, k)
				}
				slices.Sort(envKeys)
				for _, k := range envKeys {
					mcpCfg.Env = append(mcpCfg.Env, k+"="+tool.Spec.MCPConfig.Env[k])
				}
				// Resolve secret-backed env vars to file paths.
				for _, ev := range tool.Spec.MCPConfig.EnvFrom {
					if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil {
						ref := ev.ValueFrom.SecretKeyRef
						filePath := podbuilder.ToolSecretFilePath(toolName, ref.Name, ref.Key)
						mcpCfg.EnvFiles = append(mcpCfg.EnvFiles, router.EnvFileMapping{
							Name:     ev.Name,
							FilePath: filePath,
						})
					} else if ev.Value != "" {
						mcpCfg.Env = append(mcpCfg.Env, ev.Name+"="+ev.Value)
					}
				}
				// Resolve auth config to file paths. (Mirrors agentrun_controller.buildRouterConfig
				// so warm-pool pods - which bake the router config at deployment time - also
				// inject bearer/API-key/custom auth headers into remote HTTP/SSE MCP requests.)
				if auth := tool.Spec.MCPConfig.Auth; auth != nil {
					if auth.BearerToken != nil {
						ref := auth.BearerToken
						mcpCfg.AuthHeaderFiles = append(mcpCfg.AuthHeaderFiles, router.AuthHeaderFile{
							HeaderName: "Authorization",
							FilePath:   podbuilder.ToolSecretFilePath(toolName, ref.Name, ref.Key),
							Prefix:     "Bearer ",
						})
					}
					if auth.APIKey != nil {
						ref := &auth.APIKey.SecretKeyRef
						headerName := auth.APIKey.HeaderName
						if headerName == "" {
							headerName = "X-API-Key"
						}
						mcpCfg.AuthHeaderFiles = append(mcpCfg.AuthHeaderFiles, router.AuthHeaderFile{
							HeaderName: headerName,
							FilePath:   podbuilder.ToolSecretFilePath(toolName, ref.Name, ref.Key),
						})
					}
					for _, h := range auth.Headers {
						ref := &h.SecretKeyRef
						mcpCfg.AuthHeaderFiles = append(mcpCfg.AuthHeaderFiles, router.AuthHeaderFile{
							HeaderName: h.Name,
							FilePath:   podbuilder.ToolSecretFilePath(toolName, ref.Name, ref.Key),
						})
					}
				}
				if tool.Spec.MCPConfig.Auth != nil && tool.Spec.MCPConfig.Auth.OAuth != nil {
					o := tool.Spec.MCPConfig.Auth.OAuth
					mcpCfg.OAuth = &mcp.OAuthConfig{
						CredentialsDir: podbuilder.OAuthCredsMountDir(toolName, o.Credentials.Name),
						Scopes:         o.Scopes,
					}
				}
				mcpServers = append(mcpServers, mcpCfg)
			}
		}
		toolDefs = append(toolDefs, router.ToolDefinition{
			Name:        toolName,
			Description: desc,
			Parameters:  params,
			BackendType: backendType,
			BackendRef:  backendRef,
			Command:     tool.Spec.Command,
			Args:        tool.Spec.Args,
			SideEffect:  string(tool.Spec.SideEffect),
		})
	}

	// Inject builtin tools.
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_handoff",
		Description: "Transfer control to another agent with the accumulated conversation context. Use this when the task is better handled by a different specialist agent.",
		Parameters:  []byte(`{"type":"object","properties":{"targetAgent":{"type":"string","description":"Name of the Agent CRD to hand off to"},"contextSummary":{"type":"string","description":"Brief summary of work done so far to pass as input to the successor agent"}},"required":["targetAgent"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_clarify",
		Description: "Ask the human user a clarifying question when you need more information to proceed. The workflow will pause until the human responds. Only use this for genuinely ambiguous situations where you cannot make a reasonable assumption.",
		Parameters:  []byte(`{"type":"object","properties":{"question":{"type":"string","description":"The question to ask the human user"}},"required":["question"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_done",
		Description: "Signal successful task completion with a structured result. Use this when the task is fully complete and you have a final answer or output to return. The run will transition to Succeeded and no further LLM calls will be made.",
		Parameters:  []byte(`{"type":"object","properties":{"output":{"type":"string","description":"The final output or result of the task"},"summary":{"type":"string","description":"A brief human-readable summary of what was accomplished"}},"required":["output"]}`),
		BackendType: "builtin",
	})

	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_webhook_notify",
		Description: "Post a message to Slack via an incoming webhook (WEBHOOK_URL). Use to notify a team channel with task results. Returns the Slack API response.",
		Parameters:  []byte(`{"type":"object","properties":{"text":{"type":"string","description":"The message text to post to Slack."},"channel":{"type":"string","description":"Optional channel/@user override instead of the webhook's default channel"}},"required":["text"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_fail",
		Description: "Signal an unrecoverable failure with an explanation. Use this when the task cannot be completed and proceeding further would not help. The run will transition to Failed.",
		Parameters:  []byte(`{"type":"object","properties":{"reason":{"type":"string","description":"Clear explanation of why the task failed"},"retryable":{"type":"boolean","description":"Whether the failure might succeed with a retry","default":false}},"required":["reason"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_spawn",
		Description: "Launch a sub-agent to handle a subtask and wait for its result. Unlike _handoff (which transfers control away), _spawn delegates work and returns the result to you so you can continue. Use for parallelizable subtasks or specialized delegation.",
		Parameters:  []byte(`{"type":"object","properties":{"agentRef":{"type":"string","description":"Name of the Agent CRD to run"},"input":{"type":"string","description":"Task description to pass to the sub-agent"},"timeoutSeconds":{"type":"integer","description":"Maximum seconds to wait for the sub-agent (default: 300)","default":300}},"required":["agentRef","input"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_emit_event",
		Description: "Publish a named event for observability and loose coupling. Events are visible in Kubernetes (kubectl describe) and the run trace stream. Use to signal milestones or state changes.",
		Parameters:  []byte(`{"type":"object","properties":{"eventType":{"type":"string","description":"Short PascalCase event type name (e.g. DocumentProcessed, ValidationFailed)"},"message":{"type":"string","description":"Human-readable event message"}},"required":["eventType","message"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_create_workflow",
		Description: "Create and execute a deterministic AgentWorkflow with multiple steps. The workflow controller handles sequencing and you receive all step outputs when complete. Use for evaluation pipelines or multi-step tasks requiring guaranteed execution order.",
		Parameters:  []byte(`{"type":"object","properties":{"name":{"type":"string","description":"Unique workflow name"},"description":{"type":"string"},"steps":{"type":"array","description":"Array of step objects with name, agentRef, input, dependsOn","items":{"type":"object","properties":{"name":{"type":"string"},"agentRef":{"type":"string"},"input":{"type":"string"},"dependsOn":{"type":"array","items":{"type":"string"}},"condition":{"type":"string"},"timeout":{"type":"object"}},"required":["name","agentRef","input"]}},"budgetCap":{"type":"object","properties":{"total":{"type":"string"}}},"timeout":{"type":"object"},"onStepFailure":{"type":"string","enum":["stop","continue"]}},"required":["name","steps"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_search_history",
		Description: "Search prior chat turns for this deployment when the information you need is not in your current context. The model-router looks through the local warm-pod cache (an emptyDir mirroring checkpoints) first, then the shared Redis store, scoped to this deployment's prior runs. Use this BEFORE asking the human via _clarify when the answer may already exist in a previous conversation. If found=false, the human must be asked.",
		Parameters:  []byte(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"description":"The specific facts or topics to search prior conversation turns for. Use concrete terms, not vague phrases."},"topK":{"type":"integer","default":3,"description":"Maximum number of matching turn snippets to return"}},"required":["query"]}`),
		BackendType: "builtin",
	})

	// fs_read_text_file / fs_write_text_file: relay the user's local workspace
	// through the ACP bridge (issue #34), gated by Agent.spec.filesystem.
	var fsCfg *router.FilesystemConfig
	fsCfg, fsDefs := filesystemToolCfg(agent.Spec.Filesystem)
	toolDefs = append(toolDefs, fsDefs...)

	// Resolve KnowledgeBases from Agent spec and inject _rag tools.
	var kbConfigs []router.KnowledgeBaseConfig
	for _, kbName := range agent.Spec.KnowledgeBases {
		var kb agentorcav1alpha1.KnowledgeBase
		if err := r.Get(ctx, client.ObjectKey{Name: kbName, Namespace: deploy.Namespace}, &kb); err != nil {
			return nil, fmt.Errorf("getting KnowledgeBase %q: %w", kbName, err)
		}
		if kb.Spec.AllowedAgents != nil && !slices.Contains(kb.Spec.AllowedAgents, agent.Name) {
			return nil, fmt.Errorf("agent %q is not in KnowledgeBase %q allowedAgents", agent.Name, kbName)
		}
		if !kb.Status.Ready {
			// Surface the KB's own failure reason so the deployment status
			// message explains *why* the agent is failed, not just that the
			// KB isn't ready.
			kbReason := kb.Status.Message
			if kbReason == "" {
				kbReason = firstConditionMessage(kb.Status.Conditions)
			}
			if kbReason != "" {
				return nil, fmt.Errorf("KnowledgeBase %q is not ready: %s", kbName, kbReason)
			}
			return nil, fmt.Errorf("KnowledgeBase %q is not ready", kbName)
		}
		var embMS agentorcav1alpha1.ModelSelector
		if err := r.Get(ctx, client.ObjectKey{Name: kb.Spec.Embedding.ModelSelectorRef, Namespace: deploy.Namespace}, &embMS); err != nil {
			return nil, fmt.Errorf("getting embedding ModelSelector for KB %q: %w", kbName, err)
		}
		if len(embMS.Spec.Providers) == 0 {
			return nil, fmt.Errorf("embedding ModelSelector %q for KB %q has no providers", kb.Spec.Embedding.ModelSelectorRef, kbName)
		}
		embPW := embMS.Spec.Providers[0]
		var embMP agentorcav1alpha1.ModelProvider
		if err := r.Get(ctx, client.ObjectKey{Name: embPW.Name, Namespace: deploy.Namespace}, &embMP); err != nil {
			return nil, fmt.Errorf("getting embedding ModelProvider %q: %w", embPW.Name, err)
		}
		dims := kb.Status.EmbeddingDimensions
		kbConfigs = append(kbConfigs, router.KnowledgeBaseConfig{
			Name:                  kbName,
			VectorStoreURL:        kb.Status.VectorStoreURL,
			CollectionName:        kb.Status.CollectionName,
			EmbeddingProviderName: embPW.Name,
			EmbeddingModel:        embMP.Spec.LiteLLMModel,
			Dimensions:            dims,
		})
	}
	if len(kbConfigs) > 0 {
		kbNames := make([]string, len(kbConfigs))
		for i, kb := range kbConfigs {
			kbNames[i] = kb.Name
		}
		kbEnum, _ := json.Marshal(kbNames)
		kbList := strings.Join(kbNames, ", ")

		searchParams := fmt.Sprintf(`{"type":"object","properties":{"knowledgeBase":{"type":"string","enum":%s,"description":"Name of the KnowledgeBase to search"},"query":{"type":"string","minLength":1,"description":"The specific search terms derived from the user's question. Must be non-empty."},"topK":{"type":"integer","default":5,"description":"Number of results to return"}},"required":["knowledgeBase","query"]}`, kbEnum)
		ingestParams := fmt.Sprintf(`{"type":"object","properties":{"knowledgeBase":{"type":"string","enum":%s,"description":"Name of the KnowledgeBase"},"documents":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"metadata":{"type":"object"}},"required":["id","content"]}}},"required":["knowledgeBase","documents"]}`, kbEnum)

		toolDefs = append(toolDefs, router.ToolDefinition{
			Name: "_rag_search",
			Description: fmt.Sprintf("Search the knowledge base for relevant documents when the user's question is specifically about topics that may be documented there. "+
				"Do NOT call this for general knowledge questions you can answer directly. "+
				"Only call it when the answer likely requires project-specific or domain-specific information stored in the knowledge base. "+
				"Available knowledge bases: %s", kbList),
			Parameters:  json.RawMessage(searchParams),
			BackendType: "builtin",
		})
		toolDefs = append(toolDefs, router.ToolDefinition{
			Name:        "_rag_ingest",
			Description: fmt.Sprintf("Ingest documents into a knowledge base at runtime. Available knowledge bases: %s", kbList),
			Parameters:  json.RawMessage(ingestParams),
			BackendType: "builtin",
		})
	}

	// Resolve long-term memory KB and inject _memory_store tool.
	var longTermMemory router.LongTermMemoryConfig
	if agent.Spec.Memory != nil && agent.Spec.Memory.LongTermMemoryRef != "" {
		ltmKBName := agent.Spec.Memory.LongTermMemoryRef
		var ltmKB agentorcav1alpha1.KnowledgeBase
		var ltmErr error
		if err := r.Get(ctx, client.ObjectKey{Name: ltmKBName, Namespace: deploy.Namespace}, &ltmKB); err != nil {
			ltmErr = err
		} else if !ltmKB.Status.Ready {
			ltmErr = fmt.Errorf("not ready: %s", firstNonEmpty(ltmKB.Status.Message, firstConditionMessage(ltmKB.Status.Conditions)))
		}
		if ltmErr != nil {
			slog.Warn("long-term memory KnowledgeBase not ready; proceeding without it",
				"knowledgeBase", ltmKBName, "deployment", deploy.Name, "error", ltmErr)
		} else {
			var ltmEmbMS agentorcav1alpha1.ModelSelector
			if err := r.Get(ctx, client.ObjectKey{Name: ltmKB.Spec.Embedding.ModelSelectorRef, Namespace: deploy.Namespace}, &ltmEmbMS); err == nil && len(ltmEmbMS.Spec.Providers) > 0 {
				ltmPW := ltmEmbMS.Spec.Providers[0]
				var ltmMP agentorcav1alpha1.ModelProvider
				if err := r.Get(ctx, client.ObjectKey{Name: ltmPW.Name, Namespace: deploy.Namespace}, &ltmMP); err == nil {
					dims := ltmKB.Status.EmbeddingDimensions
					longTermMemory = router.LongTermMemoryConfig{
						Enabled:               true,
						KBName:                ltmKBName,
						VectorStoreURL:        ltmKB.Status.VectorStoreURL,
						CollectionName:        ltmKB.Status.CollectionName,
						EmbeddingProviderName: ltmPW.Name,
						EmbeddingModel:        ltmMP.Spec.LiteLLMModel,
						Dimensions:            dims,
					}
					toolDefs = append(toolDefs, router.ToolDefinition{
						Name:        "_memory_store",
						Description: "Persist a fact or piece of information to long-term memory so it can be recalled in future sessions.",
						Parameters:  []byte(`{"type":"object","properties":{"fact":{"type":"string","description":"The fact or information to remember"},"tags":{"type":"array","items":{"type":"string"},"description":"Optional tags for categorization"}},"required":["fact"]}`),
						BackendType: "builtin",
					})
				}
			}
		}
	}

	checkpointEvery := 1
	if agent.Spec.Memory != nil {
		checkpointEvery = agent.Spec.Memory.CheckpointEvery
	}

	// Resolve the compaction-summary model from the Agent spec (mirrors the
	// per-run wiring in agentrun_controller.buildRouterConfig). The router
	// summarizes the conversation at the compaction threshold (#47);
	// EpisodicSummaryEvery is deprecated and ignored.
	var episodicMemory router.EpisodicMemoryConfig
	//nolint:staticcheck // reads the deprecated field only to warn that it is ignored
	if agent.Spec.Memory != nil && agent.Spec.Memory.EpisodicSummaryEvery > 0 {
		slog.Warn("memory.episodicSummaryEvery is deprecated and ignored: the conversation is summarized at the compaction threshold",
			"agent", agent.Name, "deployment", deploy.Name)
	}
	if agent.Spec.Memory != nil && agent.Spec.Memory.SummaryModelSelectorRef != "" {
		summaryMSRef := agent.Spec.Memory.SummaryModelSelectorRef
		var summaryMS agentorcav1alpha1.ModelSelector
		if summaryMSRef == agent.Spec.ModelSelectorRef {
			summaryMS = selector // already loaded as the agent's ModelSelector
		} else if err := r.Get(ctx, client.ObjectKey{Name: summaryMSRef, Namespace: deploy.Namespace}, &summaryMS); err != nil {
			slog.Warn("episodic summary ModelSelector not found; summarization disabled",
				"selector", summaryMSRef, "deployment", deploy.Name)
		}
		if len(summaryMS.Spec.Providers) > 0 {
			sumPW := summaryMS.Spec.Providers[0]
			var sumMP agentorcav1alpha1.ModelProvider
			if err := r.Get(ctx, client.ObjectKey{Name: sumPW.Name, Namespace: deploy.Namespace}, &sumMP); err == nil {
				episodicMemory = router.EpisodicMemoryConfig{
					SummaryProviderName: sumPW.Name,
				}
			}
		}
	}

	cfg := &router.Config{
		RunName:                    deploy.Name,
		RunNamespace:               deploy.Namespace,
		AgentSAName:                saName,
		DeploymentName:             deploy.Name,
		AgentName:                  agent.Name,
		SelectorName:               selector.Name,
		DisableClarify:             agent.Spec.DisableClarify,
		LocalTools:                 agent.Spec.Runtime.LocalTools,
		Providers:                  providers,
		Strategy:                   routing.Strategy,
		Switchyard:                 routing.Switchyard,
		CapabilityRouting:          routing.CapabilityRouting,
		FallbackChain:              routing.FallbackChain,
		MetaRouterProviderName:     routing.MetaRouterProviderName,
		MetaRouterThreshold:        routing.MetaRouterThreshold,
		BudgetPerRunUSD:            routing.BudgetPerRunUSD,
		ToolDefinitions:            toolDefs,
		MCPServers:                 mcpServers,
		CheckpointEvery:            checkpointEvery,
		CheckpointKey:              fmt.Sprintf("agentorca/deployments/%s/state", deploy.Name),
		StateConfig:                r.StateConfig,
		EpisodicMemory:             episodicMemory,
		KubeAPIURL:                 "https://kubernetes.default.svc",
		OperatorAPIURL:             r.OperatorAPIURL,
		LLMRequestTimeout:          r.LLMRequestTimeout,
		ContextCompactionRatio:     r.ContextCompactionRatio,
		ContextCompactionThreshold: r.ContextCompactionThreshold,
		ContextCompactionMode:      r.ContextCompactionMode,
		SystemPrompt:               agent.Spec.SystemPrompt,
		KnowledgeBases:             kbConfigs,
		LongTermMemory:             longTermMemory,
		Hindsight:                  hindsightConfig(agent, r.HindsightURL),
		Filesystem:                 fsCfg,
	}

	// Per-tool-result token cap and loop guards. We deliberately seed sane defaults
	// here (in the operator, which knows each provider's ContextWindow/MaxRequestTokens)
	// rather than relying on the model-router's bare ConfigFromEnv floor, which silently
	// truncated tool results to 16k chars and left safeguards disabled — the exact
	// recipe for a confused, runaway agent run that never emits a final output.
	// An explicit deployment override always wins; see defaultMaxToolResultTokens /
	// applySafeguardDefaults for the reasoning behind the numbers.
	if deploy.Spec.MaxToolResultTokens > 0 {
		cfg.MaxToolResultTokens = deploy.Spec.MaxToolResultTokens
	} else if cfg.MaxToolResultTokens <= 0 {
		cfg.MaxToolResultTokens = defaultMaxToolResultTokens(providers)
	}
	applySafeguardDefaults(&cfg.Safeguards, deploy.Spec.ToolExecutionTimeoutSec, deploy.Spec.Safeguards)

	// GuardrailPolicy CR: wire agent's guardrailPolicyRef into cfg.Guardrails.
	if agent.Spec.GuardrailPolicyRef != "" {
		mergeGuardrailPolicyIntoRouterConfig(ctx, r.Client, deploy.Namespace, agent.Spec.GuardrailPolicyRef, cfg)
	}

	return cfg, nil
}

// defaultMaxToolResultTokens derives a sane per-tool-result cap from the configured
// providers' ContextWindow so large MCP/file/commit-patch results aren't silently cut
// off. ~10% of the window leaves room for the rest of the conversation + output; the
// floor keeps tiny-context models usable and the ceiling keeps a single result from
// dominating, and we additionally never exceed half of MaxRequestTokens (if set) so
// one oversized result can't by itself trip the rule-router's request-size exclusion.
func defaultMaxToolResultTokens(providers []router.ProviderConfig) int {
	const (
		fraction = 0.10
		floor    = 8000
		ceiling  = 64000
	)
	cw := maxContextWindow(providers)
	if cw <= 0 {
		return floor
	}
	t := int(float64(cw) * fraction)
	if mrt := maxRequestTokens(providers); mrt > 0 {
		if half := mrt / 2; t > half {
			t = half
		}
	}
	if t < floor {
		t = floor
	}
	if t > ceiling {
		t = ceiling
	}
	return t
}

// maxContextWindow returns the largest model ContextWindow among the resolved providers.
func maxContextWindow(providers []router.ProviderConfig) int {
	var m int
	for _, p := range providers {
		if p.ContextWindow > m {
			m = p.ContextWindow
		}
	}
	return m
}

// maxRequestTokens returns the smallest effective per-request token budget (MaxRequestTokens
// when set, else 0 = unset) among providers, used to bound a single tool result.
func maxRequestTokens(providers []router.ProviderConfig) int {
	var m int
	first := true
	for _, p := range providers {
		if p.MaxRequestTokens <= 0 {
			continue
		}
		if first || p.MaxRequestTokens < m {
			m = p.MaxRequestTokens
			first = false
		}
	}
	if first {
		return 0
	}
	return m
}

// applySafeguardDefaults seeds conservative loop guards that only trip on genuine stalls
// (not on legitimate repeated tool use — e.g. reading many distinct files during a PR
// review — then layers explicit run or deployment overrides on top. ToolFrequencyCap is
// left 0 (opt-in) because big batch jobs can legitimately call one tool hundreds of
// times. An override of 0 (unset) keeps the default; a negative override disables the
// check. Used for AgentDeployments and standalone AgentRuns alike.
func applySafeguardDefaults(s *router.RouterSafeguards, deployTimeoutSec int, ov *agentorcav1alpha1.AgentRunSafeguards) {
	if s == nil {
		return
	}
	s.MaxConsecutiveNoopTurns = 15 // 15 consecutive non-substantive text-only turns = stuck
	s.MinSubstantiveTokens = 20
	s.MaxRepeatedToolCalls = 50 // same tool + IDENTICAL args 50x = stuck (distinct args != trip)
	s.ToolFrequencyCap = 0      // opt-in: do NOT block high-volume legit use by default
	if s.ToolExecutionTimeoutSec <= 0 {
		s.ToolExecutionTimeoutSec = 60
	}
	if deployTimeoutSec > 0 {
		s.ToolExecutionTimeoutSec = deployTimeoutSec
	}
	if ov == nil {
		return
	}
	override := func(dst *int, v int) {
		if v != 0 {
			*dst = max(v, 0)
		}
	}
	override(&s.MaxConsecutiveNoopTurns, ov.MaxConsecutiveNoopTurns)
	if ov.MinSubstantiveTokens > 0 {
		s.MinSubstantiveTokens = ov.MinSubstantiveTokens
	}
	override(&s.MaxRepeatedToolCalls, ov.MaxRepeatedToolCalls)
	override(&s.ToolFrequencyCap, ov.ToolFrequencyCap)
	if ov.ToolExecutionTimeoutSec > 0 {
		s.ToolExecutionTimeoutSec = ov.ToolExecutionTimeoutSec
	}
}

// ensureDeploymentRouterConfigMap creates or updates the ConfigMap with the router config.
func (r *AgentDeploymentReconciler) ensureDeploymentRouterConfigMap(
	ctx context.Context,
	deploy *agentorcav1alpha1.AgentDeployment,
	cfg *router.Config,
) (string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshaling router config: %w", err)
	}

	sum := sha256.Sum256(data)
	configHash := hex.EncodeToString(sum[:])

	cmName := "agentorca-deploy-" + deploy.Name
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: deploy.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		Data: map[string]string{
			podbuilder.RouterConfigKey: string(data),
		},
	}
	if err := ctrl.SetControllerReference(deploy, cm, r.Scheme); err != nil {
		return "", fmt.Errorf("setting configmap owner ref: %w", err)
	}

	// Try create; if it exists, update.
	if err := r.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating router configmap: %w", err)
		}
		var existing corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: deploy.Namespace}, &existing); err != nil {
			return "", fmt.Errorf("getting existing router configmap: %w", err)
		}
		cmBase := existing.DeepCopy()
		existing.Data = cm.Data
		if err := r.Patch(ctx, &existing, client.MergeFrom(cmBase)); err != nil {
			return "", fmt.Errorf("updating router configmap: %w", err)
		}
	}
	return configHash, nil
}

// ensureDeploymentTokenSecret creates the per-deployment Secret with OPENAI_BASE_URL and OPENAI_API_KEY.
func (r *AgentDeploymentReconciler) ensureDeploymentTokenSecret(
	ctx context.Context,
	deploy *agentorcav1alpha1.AgentDeployment,
	saName string,
) (string, error) {
	secretName := "agentorca-deploy-" + deploy.Name + podbuilder.TokenSecretSuffix
	var existing corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: deploy.Namespace}, &existing); err == nil {
		return secretName, nil // already exists
	}

	expirySeconds := int64(900)
	tr := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{security.ModelRouterTokenAudience},
			ExpirationSeconds: new(expirySeconds),
		},
	}
	result, err := r.K8s.CoreV1().ServiceAccounts(deploy.Namespace).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating SA token for deployment %s: %w", deploy.Name, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: deploy.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		StringData: map[string]string{
			"OPENAI_BASE_URL": "http://localhost:8080/v1",
			"OPENAI_API_KEY":  result.Status.Token,
		},
	}
	if err := ctrl.SetControllerReference(deploy, secret, r.Scheme); err != nil {
		return "", fmt.Errorf("setting secret owner ref: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating token secret: %w", err)
	}
	return secretName, nil
}

// ensureDeploymentTokenReviewerBinding creates a ClusterRoleBinding so the model-router
// sidecar can validate SA tokens via the TokenReview API.
func (r *AgentDeploymentReconciler) ensureDeploymentTokenReviewerBinding(
	ctx context.Context,
	deploy *agentorcav1alpha1.AgentDeployment,
	saName string,
) error {
	if r.TokenReviewerClusterRole == "" {
		return nil
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agentorca-deploy-" + deploy.Name,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     r.TokenReviewerClusterRole,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: deploy.Namespace,
			},
		},
	}
	if err := r.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating token-reviewer ClusterRoleBinding: %w", err)
	}
	return nil
}

const (
	labelWarmPool     = "agentorca.io/warm-pool"
	labelWarmStatus   = "agentorca.io/warm-status"
	warmStatusIdle    = "idle"
	warmStatusClaimed = "claimed"
	// labelWarmRequests records how many runs a warm pod has served. Incremented on
	// each claim; when it reaches AgentDeployment.spec.maxRequestsPerPod the pod is
	// recycled (else it is reused — returned to idle) after the run completes.
	labelWarmRequests = "agentorca.io/warm-requests"

	// Defaults/caps for warm-pod lifecycle recycling (see effectiveWarmPodMaxAge
	// and warmPodTokenExpirySeconds).
	// defaultWarmPodTokenFloor is the minimum SA token lifetime we mint for a
	// warm pod, even when WarmPodMaxAge is set very low.
	defaultWarmPodTokenFloor = 1 * time.Hour
	minTokenExpiry           = 10 * time.Minute
	// maxTokenExpiry is the Kubernetes TokenRequest API hard ceiling (8760h).
	// Minted when age-based recycling is disabled (WarmPodMaxAge == 0 / nil) so
	// an indefinitely-lived warm pod can keep authenticating claim-run POSTs.
	maxTokenExpiry = 8760 * time.Hour
)

// effectiveWarmPodMaxAge returns the configured maximum warm-pod age. A nil spec
// value (the default) returns 0, which DISABLES age-based recycling: warm pods
// then persist until manually deleted, the agent process exits, config-drift
// recycle fires (see RecycleOnConfigDrift), or the MaxRequestsPerPod cap is hit.
// This is the desired default for chat/long-trajectory workloads — pods should
// not be torn down mid-session. A user opts into age recycling by setting a
// positive warmPodMaxAge explicitly.
func effectiveWarmPodMaxAge(deploy *agentorcav1alpha1.AgentDeployment) time.Duration {
	if deploy == nil || deploy.Spec.WarmPodMaxAge == nil {
		return 0
	}
	return deploy.Spec.WarmPodMaxAge.Duration
}

// recycleOnConfigDrift returns whether warm pods should be replaced when the
// router config hash drifts from the snapshot the pod was created with.
// Defaults to true (preserves existing behavior; a config change rolls the pool).
func recycleOnConfigDrift(deploy *agentorcav1alpha1.AgentDeployment) bool {
	if deploy == nil || deploy.Spec.RecycleOnConfigDrift == nil {
		return true
	}
	return *deploy.Spec.RecycleOnConfigDrift
}

// effectiveWarmLocalCache returns whether the per-warm-pod local disk cache (an
// emptyDir that supplements Redis) should be attached. Defaults to true when a
// warm pool is configured (WarmPoolSize > 0), false for one-shot deployments.
func effectiveWarmLocalCache(deploy *agentorcav1alpha1.AgentDeployment) bool {
	if deploy != nil && deploy.Spec.WarmLocalCache != nil {
		return *deploy.Spec.WarmLocalCache
	}
	return deploy != nil && deploy.Spec.WarmPoolSize > 0
}

// warmLocalCacheSizeMi returns the emptyDir SizeLimit for the warm local cache.
// Defaults to 256Mi when the field is zero.
func warmLocalCacheSizeMi(deploy *agentorcav1alpha1.AgentDeployment) int { //nolint:unused

	if deploy != nil && deploy.Spec.WarmLocalCacheSizeMi > 0 {
		return deploy.Spec.WarmLocalCacheSizeMi
	}
	return 256
}

// warmPodTokenExpirySeconds derives the SA token expiry for a warm pod from the
// configured pod max age, guaranteeing the token outlives the age at which the
// pod would be recycled. When age recycling is disabled (max age == 0) a
// long-lived token (the K8s cluster maximum) is minted so an indefinitely-lived
// pod can keep authenticating claim-run POSTs until it is manually deleted.
func warmPodTokenExpirySeconds(deploy *agentorcav1alpha1.AgentDeployment) int64 {
	maxAge := effectiveWarmPodMaxAge(deploy)
	want := maxAge
	if want <= 0 {
		// Age recycling is disabled: mint the longest-lived token K8s allows so the
		// pod can serve for its entire (manual) lifetime.
		want = maxTokenExpiry
	}
	if want < defaultWarmPodTokenFloor {
		want = defaultWarmPodTokenFloor
	}
	// Clamp to the Kubernetes TokenRequest bounds [minTokenExpiry, maxTokenExpiry].
	if want > maxTokenExpiry {
		want = maxTokenExpiry
	}
	if want < minTokenExpiry {
		want = minTokenExpiry
	}
	return int64(want / time.Second)
}

// warmPodDisposition is the fate assigned to a warm pod by classifyWarmPod.
type warmPodDisposition string

const (
	// warmDisposeRecycle means the pod is deleted on this reconcile.
	warmDisposeRecycle warmPodDisposition = "recycle"
	// warmDisposeClaimed means the pod is in-use and counts toward the pool total;
	// it is never deleted by the warm-pool reconciler.
	warmDisposeClaimed warmPodDisposition = "claimed"
	// warmDisposeIdle means the pod is reusable (ready or still starting).
	warmDisposeIdle warmPodDisposition = "idle"
)

// classifyWarmPod decides the fate of a single warm pod during reconciliation.
// reason is non-empty only when disp == warmDisposeRecycle and explains WHY the
// pod was recycled; it is surfaced in Kubernetes Events and deployment status so
// operators can tell why a warm pod disappeared.
//
// Ordering mirrors the original inline logic: a terminal pod is reaped first,
// in-use (claimed) pods are never recycled, then age / config-drift /
// request-cap checks decide whether an idle pod is rotated.
func classifyWarmPod(p *corev1.Pod, deploy *agentorcav1alpha1.AgentDeployment, routerConfigHash string, now time.Time) (disp warmPodDisposition, reason string) {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return warmDisposeRecycle, "terminal-phase"
	}
	// In-use (claimed) pods are never recycled here — wait for the run to return
	// them to idle, then apply the idle-cycle checks below.
	if p.Labels[labelWarmStatus] == warmStatusClaimed {
		return warmDisposeClaimed, ""
	}
	if maxAge := effectiveWarmPodMaxAge(deploy); maxAge > 0 && now.Sub(p.CreationTimestamp.Time) > maxAge {
		return warmDisposeRecycle, "token-age-expired"
	}
	// Recycle warm pods whose config is stale (e.g. agent spec, router config, or
	// provider weights changed) so a Deployment rollout refreshes the warm pool.
	// Pods without the annotation are treated as stale (they predate this feature).
	// Gated by RecycleOnConfigDrift so users can keep pods alive across drift.
	if recycleOnConfigDrift(deploy) {
		if podHash := p.Annotations["agentorca.io/router-config-hash"]; podHash != routerConfigHash {
			return warmDisposeRecycle, "stale-config"
		}
	}
	// Defensive: recycle idle warm pods that have served their maxRequestsPerPod cap.
	// The run controller returns pods to idle on completion; this catches any pod
	// that slipped through (e.g. a run that crashed mid-flight).
	if maxReq := deploy.Spec.MaxRequestsPerPod; warmPodOverCap(p, maxReq) {
		return warmDisposeRecycle, fmt.Sprintf("request-cap-%d", maxReq)
	}
	return warmDisposeIdle, ""
}

// warmScaleDownPlan picks which idle pods to delete when the warm pool exceeds its
// desired size: not-yet-ready starting pods first, then the oldest ready pods. It
// returns the pods to delete plus the remaining idle pools. Claimed pods are never
// passed in — the caller excludes them from the excess count so they are never deleted.
func warmScaleDownPlan(idleReady, idleStarting []*corev1.Pod, excess int) (toDelete, keepReady, keepStarting []*corev1.Pod) {
	slices.SortFunc(idleStarting, func(a, b *corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	slices.SortFunc(idleReady, func(a, b *corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	for len(idleStarting) > 0 && len(toDelete) < excess {
		toDelete = append(toDelete, idleStarting[0])
		idleStarting = idleStarting[1:]
	}
	for len(idleReady) > 0 && len(toDelete) < excess {
		toDelete = append(toDelete, idleReady[0])
		idleReady = idleReady[1:]
	}
	return toDelete, idleReady, idleStarting
}

// recordWarmRecycle emits a Kubernetes Event on the AgentDeployment recording
// that a warm pod was recycled and why, and stashes the reason/timestamp in
// status (the caller's status patch persists these in-memory writes).
func (r *AgentDeploymentReconciler) recordWarmRecycle(deploy *agentorcav1alpha1.AgentDeployment, p *corev1.Pod, reason string) {
	now := metav1.Now()
	deploy.Status.WarmPoolLastRecycleReason = reason
	deploy.Status.WarmPoolLastRecycleAt = &now
	if r.Recorder != nil {
		r.Recorder.Eventf(deploy, corev1.EventTypeNormal, "WarmPodRecycled",
			"warm pod %s recycled for deployment %s (reason: %s)", p.Name, deploy.Name, reason)
	}
}

// reconcileWarmPool ensures the pre-warmed pod pool is at the desired size.
func (r *AgentDeploymentReconciler) reconcileWarmPool(
	ctx context.Context,
	deploy *agentorcav1alpha1.AgentDeployment,
	agent *agentorcav1alpha1.Agent,
	saName string,
	routerConfigHash string,
	routerCfg *router.Config,
	providerVolumes []corev1.Volume,
	providerMounts []corev1.VolumeMount,
	toolSecretVolumes []corev1.Volume,
	toolSecretMounts []corev1.VolumeMount,
	mcpBinVolumes []corev1.Volume,
	mcpBinMounts []corev1.VolumeMount,
) error {
	if err := ensureHindsightKeySecret(ctx, r.Client, deploy.Namespace, r.HindsightAPIKey); err != nil {
		return err
	}
	// Ensure deployment-scoped RBAC for warm pods.
	if err := r.ensureWarmPoolRBAC(ctx, deploy, saName); err != nil {
		return fmt.Errorf("ensuring warm pool RBAC: %w", err)
	}

	// Ensure the warm router ConfigMap exists.
	if err := r.ensureWarmRouterConfigMap(ctx, deploy, agent, routerCfg); err != nil {
		return fmt.Errorf("ensuring warm router configmap: %w", err)
	}

	// Delete any completed claimed warm pods. Claimed pods are owned by the AgentDeployment
	// (not the AgentRun), so they are not automatically GC'd when a run finishes. For
	// non-HTTP mode agents the process exits naturally, leaving the pod in Succeeded/Failed;
	// clean those up here before re-evaluating the idle pool.
	var claimedPods corev1.PodList
	if err := r.List(ctx, &claimedPods,
		client.InNamespace(deploy.Namespace),
		client.MatchingLabels{
			labelWarmPool:   deploy.Name,
			labelWarmStatus: warmStatusClaimed,
		},
	); err != nil {
		return fmt.Errorf("listing claimed warm pods: %w", err)
	}
	for i := range claimedPods.Items {
		p := &claimedPods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			slog.Info("cleaning up completed claimed warm pod", "pod", p.Name, "phase", p.Status.Phase)
			_ = r.Delete(ctx, p)
		}
	}

	// List ALL warm pods for this deployment (any warm-status) so in-use (claimed)
	// pods count toward the desired pool size and are never killed mid-run.
	// Only idle pods are recycled; only excess IDLE pods are deleted. This prevents
	// the old over-provision bug where a replacement was spawned while a pod was
	// busy, causing the just-returned pod to be deleted as "excess" (defeating reuse).
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(deploy.Namespace),
		client.MatchingLabels{
			labelWarmPool: deploy.Name,
		},
	); err != nil {
		return fmt.Errorf("listing warm pods: %w", err)
	}

	// Delete warm pods that have completed/failed, are aging out, have stale
	// config, or have hit their request cap. In-use (claimed) pods are never
	// recycled here. The SA token is minted at creation with an expiry derived
	// from WarmPodMaxAge (warmPodTokenExpirySeconds), so pods are recycled
	// before authentication would fail when claimed. classifyWarmPod encodes
	// the decision + reason so recycled pods are observable via Events/Status.
	now := time.Now()
	var idleReady, idleStarting, claimed []*corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		// Ignore pods already being torn down — they're not pool capacity and
		// recounting them caused the repeated "deleting excess" log spam.
		if p.DeletionTimestamp != nil {
			continue
		}
		disp, reason := classifyWarmPod(p, deploy, routerConfigHash, now)
		switch disp {
		case warmDisposeRecycle:
			r.recordWarmRecycle(deploy, p, reason)
			slog.Info("recycling warm pod",
				"deployment", deploy.Name, "pod", p.Name, "reason", reason)
			_ = r.Delete(ctx, p)
		case warmDisposeClaimed:
			// In-use pods count toward the desired total and are never deleted.
			claimed = append(claimed, p)
		case warmDisposeIdle:
			modelRouterReady := false
			for _, cs := range p.Status.InitContainerStatuses {
				if cs.Name == "model-router" && cs.Ready { //nolint:goconst

					modelRouterReady = true
					break
				}
			}
			if modelRouterReady {
				idleReady = append(idleReady, p)
			} else {
				idleStarting = append(idleStarting, p)
			}
		}
	}

	readyCount := len(idleReady)
	startingCount := len(idleStarting)
	// warmPoolSize is the TOTAL number of warm pods to keep for the deployment
	// (idle + in-use). Counting in-use pods toward the total means we do NOT spawn
	// a replacement while a pod is busy — so when the run finishes and the pod
	// returns to idle, it is simply reused instead of being deleted as "excess".
	desired := deploy.Spec.WarmPoolSize
	totalExisting := readyCount + startingCount + len(claimed)

	// Scale down excess IDLE/starting pods only. Never delete in-use (claimed) pods.
	if excess := totalExisting - desired; excess > 0 {
		toDelete, keepReady, keepStarting := warmScaleDownPlan(idleReady, idleStarting, excess)
		for _, p := range toDelete {
			slog.Info("scaling down warm pool: deleting excess warm pod",
				"deployment", deploy.Name, "pod", p.Name, "desired", desired, "excess", excess)
			if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting excess warm pod %s: %w", p.Name, err)
			}
		}
		readyCount = len(keepReady)
		startingCount = len(keepStarting)
	}

	// Create warm pods up to the desired total pool size. Each warm pod gets its
	// own freshly-minted SA token so it is never blocked by the deployment-level
	// secret's 15-minute expiry.
	totalExisting = readyCount + startingCount + len(claimed)
	for i := totalExisting; i < desired; i++ {
		warmTokenName, err := r.createWarmPodTokenSecret(ctx, deploy, saName)
		if err != nil {
			return fmt.Errorf("creating warm pod token secret: %w", err)
		}
		pod := r.buildWarmPod(deploy, agent, saName, warmTokenName, routerConfigHash, routerCfg.Strategy, providerVolumes, providerMounts, toolSecretVolumes, toolSecretMounts, mcpBinVolumes, mcpBinMounts)
		if err := ctrl.SetControllerReference(deploy, pod, r.Scheme); err != nil {
			return fmt.Errorf("setting warm pod owner ref: %w", err)
		}
		if err := r.Create(ctx, pod); err != nil {
			return fmt.Errorf("creating warm pod: %w", err)
		}
		slog.Info("created warm pod", "deployment", deploy.Name, "pod", pod.Name)
		startingCount++
	}

	deploy.Status.WarmPoolReady = readyCount
	return nil
}

// ensureWarmPoolRBAC creates the deployment-scoped Role and RoleBinding for warm pods.
func (r *AgentDeploymentReconciler) ensureWarmPoolRBAC(
	ctx context.Context,
	deploy *agentorcav1alpha1.AgentDeployment,
	saName string,
) error {
	role := security.BuildDeploymentRole(deploy.Name, deploy.Namespace)
	if err := ctrl.SetControllerReference(deploy, role, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating deployment Role: %w", err)
	}

	rb := security.BuildDeploymentRoleBinding(deploy.Name, saName, deploy.Namespace)
	if err := ctrl.SetControllerReference(deploy, rb, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, rb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating deployment RoleBinding: %w", err)
	}
	return nil
}

// ensureWarmRouterConfigMap creates the ConfigMap used by warm pods (WarmMode: true).
func (r *AgentDeploymentReconciler) ensureWarmRouterConfigMap(
	ctx context.Context,
	deploy *agentorcav1alpha1.AgentDeployment,
	agent *agentorcav1alpha1.Agent,
	baseCfg *router.Config,
) error {
	// Build warm config from the deployment config.
	warmCfg := *baseCfg
	warmCfg.WarmMode = true
	warmCfg.ChatMode = true
	// Activate the warm-pod local L1 cache (disk-backed emptyDir mirror of
	// checkpoints) when enabled on the deployment. The operator mounts the
	// emptyDir at the same path the model-router reads here.
	if effectiveWarmLocalCache(deploy) {
		warmCfg.WarmLocalCacheDir = podbuilder.WarmCacheMountDir
		warmCfg.WarmLocalCacheSizeMi = warmLocalCacheSizeMiValue(deploy)
	}
	// Set HTTPInput so it starts after claiming.
	port := agent.Spec.Runtime.InputPort
	if port == 0 {
		port = 8000
	}
	path := agent.Spec.Runtime.InputPath
	if path == "" {
		path = "/invoke"
	}
	warmCfg.HTTPInput = router.HTTPInputConfig{
		Enabled: true,
		Port:    port,
		Path:    path,
		// RunName and Input are empty; they will be set at claim time via ClaimRun().
	}

	data, err := json.Marshal(warmCfg)
	if err != nil {
		return fmt.Errorf("marshaling warm router config: %w", err)
	}

	cmName := "agentorca-warm-" + deploy.Name
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: deploy.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		Data: map[string]string{
			podbuilder.RouterConfigKey: string(data),
		},
	}
	if err := ctrl.SetControllerReference(deploy, cm, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating warm router configmap: %w", err)
		}
		// Update existing.
		var existing corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: deploy.Namespace}, &existing); err != nil {
			return err
		}
		cmBase := existing.DeepCopy()
		existing.Data = cm.Data
		if err := r.Patch(ctx, &existing, client.MergeFrom(cmBase)); err != nil {
			return fmt.Errorf("updating warm router configmap: %w", err)
		}
	}
	return nil
}

// buildWarmPod constructs an idle warm pod for the given AgentDeployment.
// Built via the shared podbuilder package so all agent pod types use one codepath.
func (r *AgentDeploymentReconciler) buildWarmPod(
	deploy *agentorcav1alpha1.AgentDeployment,
	agent *agentorcav1alpha1.Agent,
	saName string,
	tokenSecretName string,
	routerConfigHash string,
	routerStrategy string,
	providerVolumes []corev1.Volume,
	providerMounts []corev1.VolumeMount,
	toolSecretVolumes []corev1.Volume,
	toolSecretMounts []corev1.VolumeMount,
	mcpBinVolumes []corev1.Volume,
	mcpBinMounts []corev1.VolumeMount,
) *corev1.Pod {
	inputSourceType := agentorcav1alpha1.InputSourceChat
	if deploy.Spec.InputSource != nil {
		inputSourceType = deploy.Spec.InputSource.Type
	}

	agentEnv := []corev1.EnvVar{
		{Name: "AGENTORC_AGENT", Value: agent.Name},
		{Name: "AGENTORC_NAMESPACE", Value: deploy.Namespace},
		{Name: "AGENTORC_INPUT_SOURCE", Value: string(inputSourceType)},
	}

	agentSecretVolumes, agentSecretMounts := podbuilder.ResolveAgentSecretRefs(agent)

	var webhookNotifyRef *corev1.SecretKeySelector
	if deploy.Spec.WebhookNotify != nil {
		webhookNotifyRef = deploy.Spec.WebhookNotify.WebhookSecretRef
	}
	return podbuilder.Build(podbuilder.PodConfig{
		RouterStrategy:           routerStrategy,
		HindsightAPIKeySecretRef: hindsightKeyRef(r.HindsightAPIKey),
		GenerateName:             "warm-" + deploy.Name + "-",
		Namespace:                deploy.Namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name":       "agent-orca",
			"app.kubernetes.io/component":  "warm-pod",
			"agentdeployment.agentorca.io": deploy.Name,
			"agent.agentorca.io":           agent.Name,
			labelWarmPool:                  deploy.Name,
			labelWarmStatus:                warmStatusIdle,
			security.LabelManagedBy:        security.ManagedByValue,
		},
		Annotations: map[string]string{
			"agentorca.io/router-config-hash": routerConfigHash,
		},
		Agent:            agent,
		AgentEnv:         agentEnv,
		TokenSecretName:  tokenSecretName,
		ServiceAccount:   saName,
		RestartPolicy:    corev1.RestartPolicyNever,
		ModelRouterImage: r.ModelRouterImage,
		RouterConfigName: "agentorca-warm-" + deploy.Name,
		RedisSecretName:  redisSecretName("agentorca-deploy-"+deploy.Name, r.RedisPassword),
		RouterExtraPorts: []corev1.ContainerPort{
			{Name: "warm-mgmt", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
		},
		RouterProbePort:    9090,
		ProviderVolumes:    providerVolumes,
		ProviderMounts:     providerMounts,
		ToolSecretVolumes:  toolSecretVolumes,
		ToolSecretMounts:   toolSecretMounts,
		MCPBinVolumes:      mcpBinVolumes,
		MCPBinMounts:       mcpBinMounts,
		AgentSecretVolumes: agentSecretVolumes,
		AgentSecretMounts:  agentSecretMounts,
		CloudProvider:      r.CloudProvider,
		// Warm-pod local L1 cache: a disk-backed emptyDir that mirrors checkpoints
		// to Redis for faster resume + Redis-outage resilience. Only on warm pods.
		WarmLocalCacheEnabled:   effectiveWarmLocalCache(deploy),
		WarmLocalCacheSizeLimit: warmCacheSizeLimitQuantity(deploy),
		WebhookNotifySecretRef:  webhookNotifyRef,
	})
}

// warmCacheSizeLimitQuantity builds the resource.Quantity SizeLimit for the warm
// local cache emptyDir from the deployment spec (default 256Mi).
func warmCacheSizeLimitQuantity(deploy *agentorcav1alpha1.AgentDeployment) *resource.Quantity {
	mi := warmLocalCacheSizeMiValue(deploy)
	q := resource.MustParse(fmt.Sprintf("%dMi", mi))
	return &q
}

// warmCacheSizeLimitValue returns the raw MiB value for the warm local cache.
func warmLocalCacheSizeMiValue(deploy *agentorcav1alpha1.AgentDeployment) int {
	if deploy != nil && deploy.Spec.WarmLocalCacheSizeMi > 0 {
		return deploy.Spec.WarmLocalCacheSizeMi
	}
	return 256
}

// createWarmPodTokenSecret mints a fresh SA token for a single warm pod.
// Unlike the deployment-level token secret (created once, 15-min expiry), this is
// created fresh per warm pod so the token is never stale at the time the pod starts.
func (r *AgentDeploymentReconciler) createWarmPodTokenSecret(
	ctx context.Context,
	deploy *agentorcav1alpha1.AgentDeployment,
	saName string,
) (string, error) {
	// Derive the token expiry from the pod's configured max age so the token
	// outlives the age at which the pod would be recycled (e.g. 50m pod age ->
	// 1h token). By default age recycling is disabled (WarmPodMaxAge nil -> 0),
	// so a long-lived token (the K8s cluster maximum) is minted for an
	// indefinitely-lived pod, letting it authenticate claim-run POSTs.
	expirySeconds := warmPodTokenExpirySeconds(deploy)
	tr := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{security.ModelRouterTokenAudience},
			ExpirationSeconds: &expirySeconds,
		},
	}
	result, err := r.K8s.CoreV1().ServiceAccounts(deploy.Namespace).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating SA token for warm pod: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "agentorca-warm-" + deploy.Name + "-token-",
			Namespace:    deploy.Namespace,
			Labels: map[string]string{
				labelWarmPool:           deploy.Name,
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		StringData: map[string]string{
			"OPENAI_BASE_URL": "http://localhost:8080/v1",
			"OPENAI_API_KEY":  result.Status.Token,
		},
	}
	if err := ctrl.SetControllerReference(deploy, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("creating warm pod token secret: %w", err)
	}
	return secret.Name, nil
}

// agentDeploymentsForAgent maps an Agent change to the AgentDeployments that reference it.
func (r *AgentDeploymentReconciler) agentDeploymentsForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	agent := obj.(*agentorcav1alpha1.Agent)
	var list agentorcav1alpha1.AgentDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(agent.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range list.Items {
		if d.Spec.AgentRef == agent.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// agentDeploymentsForModelSelector maps a ModelSelector change to affected AgentDeployments.
// The link is indirect: AgentDeployment → Agent → ModelSelector.
func (r *AgentDeploymentReconciler) agentDeploymentsForModelSelector(ctx context.Context, obj client.Object) []reconcile.Request {
	ms := obj.(*agentorcav1alpha1.ModelSelector)
	var deployList agentorcav1alpha1.AgentDeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(ms.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range deployList.Items {
		var agent agentorcav1alpha1.Agent
		if err := r.Get(ctx, types.NamespacedName{Name: d.Spec.AgentRef, Namespace: d.Namespace}, &agent); err != nil {
			continue
		}
		if agent.Spec.ModelSelectorRef == ms.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// agentDeploymentsForModelProvider maps a ModelProvider change to affected AgentDeployments.
// The link is: AgentDeployment → Agent → ModelSelector → ModelProvider.
func (r *AgentDeploymentReconciler) agentDeploymentsForModelProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	mp := obj.(*agentorcav1alpha1.ModelProvider)
	// Find ModelSelectors that reference this provider.
	var selectorList agentorcav1alpha1.ModelSelectorList
	if err := r.List(ctx, &selectorList, client.InNamespace(mp.Namespace)); err != nil {
		return nil
	}
	affectedSelectors := map[string]bool{}
	for _, ms := range selectorList.Items {
		for _, pw := range ms.Spec.Providers {
			if pw.Name == mp.Name {
				affectedSelectors[ms.Name] = true
				break
			}
		}
		if ms.Spec.MetaRouter != nil && ms.Spec.MetaRouter.ProviderRef == mp.Name {
			affectedSelectors[ms.Name] = true
		}
	}
	if len(affectedSelectors) == 0 {
		return nil
	}
	// Find AgentDeployments whose Agent uses one of those selectors.
	var deployList agentorcav1alpha1.AgentDeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(mp.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range deployList.Items {
		var agent agentorcav1alpha1.Agent
		if err := r.Get(ctx, types.NamespacedName{Name: d.Spec.AgentRef, Namespace: d.Namespace}, &agent); err != nil {
			continue
		}
		if affectedSelectors[agent.Spec.ModelSelectorRef] {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// agentDeploymentsForTool maps a Tool change to AgentDeployments whose Agent references it.
func (r *AgentDeploymentReconciler) agentDeploymentsForTool(ctx context.Context, obj client.Object) []reconcile.Request {
	tool := obj.(*agentorcav1alpha1.Tool)
	// Find Agents that list this tool.
	var agentList agentorcav1alpha1.AgentList
	if err := r.List(ctx, &agentList, client.InNamespace(tool.Namespace)); err != nil {
		return nil
	}
	affectedAgents := map[string]bool{}
	for _, ag := range agentList.Items {
		if slices.Contains(ag.Spec.Tools, tool.Name) {
			affectedAgents[ag.Name] = true
		}
	}
	if len(affectedAgents) == 0 {
		return nil
	}
	var deployList agentorcav1alpha1.AgentDeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(tool.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range deployList.Items {
		if affectedAgents[d.Spec.AgentRef] {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// agentDeploymentsForKnowledgeBase maps a KnowledgeBase change to AgentDeployments whose Agent references it.
func (r *AgentDeploymentReconciler) agentDeploymentsForKnowledgeBase(ctx context.Context, obj client.Object) []reconcile.Request {
	kb := obj.(*agentorcav1alpha1.KnowledgeBase)
	var agentList agentorcav1alpha1.AgentList
	if err := r.List(ctx, &agentList, client.InNamespace(kb.Namespace)); err != nil {
		return nil
	}
	affectedAgents := map[string]bool{}
	for _, ag := range agentList.Items {
		if slices.Contains(ag.Spec.KnowledgeBases, kb.Name) {
			affectedAgents[ag.Name] = true
		}
	}
	if len(affectedAgents) == 0 {
		return nil
	}
	var deployList agentorcav1alpha1.AgentDeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(kb.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range deployList.Items {
		if affectedAgents[d.Spec.AgentRef] {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("agentdeployment") //nolint:staticcheck
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.AgentDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Pod{}).
		Watches(&agentorcav1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForAgent)).
		Watches(&agentorcav1alpha1.ModelSelector{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForModelSelector)).
		Watches(&agentorcav1alpha1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForModelProvider)).
		Watches(&agentorcav1alpha1.Tool{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForTool)).
		Watches(&agentorcav1alpha1.KnowledgeBase{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForKnowledgeBase)).
		Complete(r)
}

// firstNonEmpty returns the first non-empty string from the provided values.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstConditionMessage returns the message of the "Ready" condition when its
// status is False or Unknown. This avoids picking up benign informational
// conditions like QdrantUpgrading=False (Reason="UpToDate"), which is a healthy
// state, not an error.
func firstConditionMessage(conditions []metav1.Condition) string {
	for _, c := range conditions {
		if c.Type == conditionReady && (c.Status == metav1.ConditionFalse || c.Status == metav1.ConditionUnknown) {
			return c.Message
		}
	}
	return ""
}

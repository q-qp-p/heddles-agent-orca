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

// Package podbuilder provides a single codepath for constructing agent pods.
// All controllers that need to create agent pods or pod templates use Build()
// to produce a *corev1.Pod with the agent container, model-router sidecar,
// volumes, and security hardening applied consistently.
package podbuilder

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/framework"
	"github.com/heddles/agent-orca/internal/security"
)

const (
	// RouterConfigKey is the key within the router config ConfigMap.
	RouterConfigKey = "router-config.json"
	// RouterConfigVolume is the volume name for the router config ConfigMap.
	RouterConfigVolume = "agentorca-router-config"
	// RouterConfigDir is the mount path for the router config directory.
	RouterConfigDir = "/etc/agentorca"

	// TokenSecretSuffix is appended to resource names for token secrets.
	TokenSecretSuffix = "-token"
	// TokenVolumeName is the volume name for the token env secret.
	TokenVolumeName = "agentorca-env"
	// TokenEnvDir is the mount path for the token env secret.
	TokenEnvDir = "/etc/agentorca-env"

	// RedisSecretSuffix is appended to resource names for the Secret holding the
	// Redis password the model-router authenticates with.
	RedisSecretSuffix = "-redis"
	// RedisSecretKey is the key within the Redis password Secret.
	RedisSecretKey = "password"
	// RedisSecretVolume is the volume name for the Redis password Secret.
	RedisSecretVolume = "agentorca-redis"
	// RedisSecretDir is the mount path for the Redis password Secret. Must NOT
	// be under RouterConfigDir for the same reason as ProviderSecretsDir.
	RedisSecretDir = "/etc/agentorca-redis"
	// RedisPasswordFile is the path the model-router reads the password from.
	RedisPasswordFile = RedisSecretDir + "/" + RedisSecretKey

	// ProviderVolPrefix is the volume name prefix for provider API key secrets.
	ProviderVolPrefix = "provider-"
	// ProviderSecretsDir is the mount path for provider secret directories.
	// Must NOT be under RouterConfigDir — the ConfigMap volume is read-only and
	// the kubelet cannot create intermediate directories inside it.
	ProviderSecretsDir = "/etc/agentorca-providers"

	// ToolSecretsDir is the base mount path for tool secret volumes.
	// Must NOT be under RouterConfigDir for the same reason as ProviderSecretsDir.
	ToolSecretsDir = "/etc/agentorca-tool-secrets"
	// ToolSecretVolPrefix is the volume name prefix for tool secret volumes.
	ToolSecretVolPrefix = "tool-secret-"

	// AgentSecretVolPrefix is the volume name prefix for agent-runtime secret volumes
	// (Agent.spec.runtime.secretRefs). Mounted into the agent container only.
	AgentSecretVolPrefix = "agent-secret-"
	// AgentSecretMountDir is the default mount path used for an agent secret ref when
	// MountPath is empty (keys are injected as files under this directory).
	AgentSecretMountDir = "/etc/agentorca-secrets"

	// MCPBinDir is the base mount path for MCP sidecar tool image volumes.
	// Each tool image is mounted at MCPBinDir/<toolName>/ via a Kubernetes
	// image volume (see ResolveMCPSidecarVolumes).
	MCPBinDir = "/mcp-img"

	// WarmCacheVolName is the volume name for the disk-backed emptyDir used as a
	// local L1 checkpoint cache on warm pods (see WarmLocalCacheEnabled in PodConfig).
	WarmCacheVolName = "agentorca-warm-cache"
	// WarmCacheMountDir is the in-container path the model-router mirrors warm-pod
	// checkpoints to. Mounted into the model-router sidecar only.
	WarmCacheMountDir = "/var/lib/agentorca/warm-cache"
)

// DefaultRouterResources are the resource requirements applied to the model-router
// sidecar when PodConfig.RouterResources is nil.
var DefaultRouterResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	},
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	},
}

// SwitchyardRouterResources are the resource requirements of the model-router
// sidecar of a pod whose ModelSelector uses strategy switchyard. Switchyard's
// engine runs a WebAssembly guest in-process: measured at about 108 MB of peak
// memory (152 MB against 44 MB without it) plus about 18 MB per extra CPU, which
// leaves little of DefaultRouterResources' 256Mi for the conversation itself.
var SwitchyardRouterResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	},
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	},
}

// routerStrategySwitchyard is the ModelSelector strategy that needs
// SwitchyardRouterResources.
const routerStrategySwitchyard = "switchyard"

// routerResourcesFor picks the model-router sidecar's resources: an explicit
// override, else the Switchyard defaults for a switchyard selector, else the
// ordinary defaults.
func routerResourcesFor(cfg PodConfig) corev1.ResourceRequirements {
	switch {
	case cfg.RouterResources != nil:
		return *cfg.RouterResources
	case cfg.RouterStrategy == routerStrategySwitchyard:
		return SwitchyardRouterResources
	}
	return DefaultRouterResources
}

// PodConfig holds all inputs needed to build an agent pod spec.
// Controllers populate this struct with their specific configuration and call Build().
type PodConfig struct {
	// --- Identity ---

	// PodName is set for AgentRun pods (e.g. "agentorca-run-<name>").
	// Empty for Deployment templates and warm pods that use GenerateName.
	PodName string
	// GenerateName is set for warm pods (e.g. "warm-<deploy>-").
	// Mutually exclusive with PodName.
	GenerateName string
	Namespace    string
	Labels       map[string]string
	Annotations  map[string]string

	// --- Agent spec ---

	// Agent is the Agent CRD that defines the runtime image, framework, tools, etc.
	Agent *agentorcav1alpha1.Agent
	// AgentEnv holds caller-specific env vars (e.g. AGENTORC_RUN_ID for runs,
	// AGENTORC_AGENT for deployments). FrameworkEnvVars are appended automatically.
	AgentEnv []corev1.EnvVar
	// TokenSecretName is the Secret containing OPENAI_BASE_URL and OPENAI_API_KEY,
	// injected via EnvFrom on the agent container.
	TokenSecretName string

	// --- Pod behavior ---

	ServiceAccount string
	RestartPolicy  corev1.RestartPolicy // Never (runs/warm) or Always (deployments)

	// --- Model router ---

	ModelRouterImage string
	// RouterConfigName is the ConfigMap name containing the router config JSON.
	RouterConfigName string
	// RedisSecretName is the Secret holding the Redis password, mounted read-only
	// at RedisPasswordFile in the model-router container only. Empty when Redis
	// needs no password.
	RedisSecretName string
	// RouterExtraPorts are additional ports on the model-router container
	// (e.g. warm-mgmt:9090 for warm pods).
	RouterExtraPorts []corev1.ContainerPort
	// RouterProbePort is the port for the model-router startup probe.
	// Defaults to 8080 if zero.
	RouterProbePort int
	// RouterResources overrides the default model-router resource requirements.
	// If nil, DefaultRouterResources is used, or SwitchyardRouterResources when
	// RouterStrategy is "switchyard".
	RouterResources *corev1.ResourceRequirements
	// RouterStrategy is the routing strategy of the ModelSelector the router
	// serves. Only "switchyard" changes anything (the router's memory).
	RouterStrategy string
	// WebhookNotifySecretRef, when set, mounts the referenced Secret key as the
	// WEBHOOK_URL env var into the model-router sidecar so the _webhook_notify
	// builtin tool can POST to a Slack incoming webhook. Not mounted to the agent
	// container. +optional
	WebhookNotifySecretRef *corev1.SecretKeySelector

	// HindsightURL is the configured hindsight API endpoint (e.g. http://hindsight.local).
	// Used to generate ipBlock egress rules in the per-run NetworkPolicies when the cluster has a default-deny egress policy.
	HindsightURL string

	// HindsightAPIKeySecretRef, when set, injects the hindsight API key into the
	// model-router container as HINDSIGHT_API_KEY so it can authenticate to a
	// hindsight API that requires a tenant API key. Never mounted to the agent
	// container. +optional
	HindsightAPIKeySecretRef *corev1.SecretKeySelector

	// --- Agent probes ---

	// AgentReadinessProbe is set for http-mode AgentRun pods.
	// If nil, no readiness probe is added to the agent container.
	AgentReadinessProbe *corev1.Probe

	// --- Pre-resolved volumes (I/O stays in the caller) ---

	ProviderVolumes   []corev1.Volume
	ProviderMounts    []corev1.VolumeMount
	ToolSecretVolumes []corev1.Volume
	ToolSecretMounts  []corev1.VolumeMount
	MCPBinVolumes     []corev1.Volume
	MCPBinMounts      []corev1.VolumeMount

	// AgentSecretVolumes and AgentSecretMounts carry secrets referenced by
	// Agent.spec.runtime.secretRefs. They are mounted into the agent container only
	// (not the model-router sidecar). Use ResolveAgentSecretRefs to build them.
	AgentSecretVolumes []corev1.Volume
	AgentSecretMounts  []corev1.VolumeMount

	// WarmLocalCacheEnabled attaches a disk-backed emptyDir to the model-router
	// sidecar as a local checkpoint cache that supplements Redis. Only set true
	// for warm pods (buildWarmPod); one-shot AgentRun pods leave it false so their
	// specs are unchanged.
	WarmLocalCacheEnabled bool
	// WarmLocalCacheSizeLimit is the SizeLimit for the warm-cache emptyDir.
	// May be nil for "no limit" (bounded by node ephemeral storage).
	WarmLocalCacheSizeLimit *resource.Quantity

	// --- Network topology ---

	// RouterBaseURL is the base URL the agent uses to reach the model-router.
	// Defaults to "http://localhost:8080" when empty (combined-pod sidecar topology).
	// Set to the router Service URL (e.g. "http://agentorca-router-<run>.<ns>:8080") in
	// split-pod topology so framework env vars point at the correct endpoint.
	RouterBaseURL string

	// --- Cloud provider ---

	// CloudProvider is used for Azure Workload Identity pod labeling.
	CloudProvider security.CloudProvider
}

// Build constructs a *corev1.Pod from the given config. The returned pod has:
//   - An agent container with the configured image, command, env, and resources
//   - A model-router native sidecar (init container with restartPolicy=Always)
//   - All provider, tool secret, and MCP binary volumes
//   - Framework injection (tier 1/2/3) applied
//   - Security hardening (PSA restricted + projected SA token + /tmp emptyDir) applied
//
// The caller is responsible for setting OwnerReferences on the returned pod.
func Build(cfg PodConfig) *corev1.Pod {
	routerBaseURL := cfg.RouterBaseURL
	if routerBaseURL == "" {
		routerBaseURL = "http://localhost:8080"
	}

	// --- Agent container ---

	agentEnv := make([]corev1.EnvVar, len(cfg.AgentEnv)) //nolint:prealloc

	copy(agentEnv, cfg.AgentEnv)
	agentEnv = append(agentEnv, FrameworkEnvVars(cfg.Agent.Spec.Runtime.Framework, routerBaseURL)...)

	agentContainer := corev1.Container{
		Name:            "agent",
		Image:           cfg.Agent.Spec.Runtime.OCIRef,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         cfg.Agent.Spec.Runtime.Command,
		Args:            cfg.Agent.Spec.Runtime.Args,
		Env:             agentEnv,
		EnvFrom: []corev1.EnvFromSource{
			{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.TokenSecretName},
				},
			},
		},
	}
	if cfg.Agent.Spec.Resources != nil {
		agentContainer.Resources = *cfg.Agent.Spec.Resources
	}
	if cfg.AgentReadinessProbe != nil {
		agentContainer.ReadinessProbe = cfg.AgentReadinessProbe
	}
	agentContainer.VolumeMounts = append(agentContainer.VolumeMounts, cfg.AgentSecretMounts...)

	// --- Router config volume ---

	routerConfigVol := corev1.Volume{
		Name: RouterConfigVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cfg.RouterConfigName},
			},
		},
	}

	// --- Model-router sidecar ---

	routerProbePort := cfg.RouterProbePort
	if routerProbePort == 0 {
		routerProbePort = 8080
	}

	routerResources := routerResourcesFor(cfg)

	routerPorts := []corev1.ContainerPort{ //nolint:prealloc

		{Name: "openai", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		{Name: "gemini", ContainerPort: 8082, Protocol: corev1.ProtocolTCP},
		// Metrics endpoint served by the model-router on :9091 (Prometheus/OpenMetrics).
		// Declared as a containerPort so a PodMonitor/ServiceMonitor can resolve
		// port: metrics and so cluster networking tooling can see it.
		{Name: "metrics", ContainerPort: 9091, Protocol: corev1.ProtocolTCP},
	}
	routerPorts = append(routerPorts, cfg.RouterExtraPorts...)

	// Volume mounts for the model-router: provider secrets + tool secrets + MCP binaries + router config.
	routerMounts := make([]corev1.VolumeMount, 0, len(cfg.ProviderMounts)+len(cfg.ToolSecretMounts)+len(cfg.MCPBinMounts)+1)
	routerMounts = append(routerMounts, cfg.ProviderMounts...)
	routerMounts = append(routerMounts, cfg.ToolSecretMounts...)
	routerMounts = append(routerMounts, cfg.MCPBinMounts...)
	routerMounts = append(routerMounts, corev1.VolumeMount{
		Name:      RouterConfigVolume,
		MountPath: RouterConfigDir,
		ReadOnly:  true,
	})
	if cfg.RedisSecretName != "" {
		routerMounts = append(routerMounts, redisSecretMount())
	}

	// Warm-pod local L1 checkpoint cache: a disk-backed emptyDir mirrored into the
	// model-router sidecar (and read by it write-through to Redis). The agent container
	// never touches this volume directly — it reaches the router over localhost.
	if cfg.WarmLocalCacheEnabled {
		routerMounts = append(routerMounts, corev1.VolumeMount{
			Name:      WarmCacheVolName,
			MountPath: WarmCacheMountDir,
		})
	}

	alwaysRestart := corev1.ContainerRestartPolicyAlways
	modelRouterContainer := corev1.Container{
		Name:            "model-router",
		Image:           cfg.ModelRouterImage,
		ImagePullPolicy: routerPullPolicy(cfg.ModelRouterImage),
		RestartPolicy:   &alwaysRestart,
		Env: []corev1.EnvVar{
			{Name: "AGENTORC_ROUTER_CONFIG", Value: RouterConfigDir + "/" + RouterConfigKey},
		},
		Ports:        routerPorts,
		VolumeMounts: routerMounts,
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt(routerProbePort),
				},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
			FailureThreshold:    30, // allow up to 61s for startup
		},
		Resources: routerResources,
	}
	if cfg.HindsightURL != "" {
		modelRouterContainer.Env = append(modelRouterContainer.Env, corev1.EnvVar{
			Name:  "HINDSIGHT_URL",
			Value: cfg.HindsightURL,
		})
	}
	if cfg.HindsightAPIKeySecretRef != nil {
		modelRouterContainer.Env = append(modelRouterContainer.Env, corev1.EnvVar{
			Name:      "HINDSIGHT_API_KEY",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.HindsightAPIKeySecretRef},
		})
	}
	if cfg.WarmLocalCacheEnabled {
		// AGENTORC_WARM_CACHE_DIR activates the local-mirror state store in the model-router.
		modelRouterContainer.Env = append(modelRouterContainer.Env, corev1.EnvVar{
			Name:  "AGENTORC_WARM_CACHE_DIR",
			Value: WarmCacheMountDir,
		})
	}
	if cfg.WebhookNotifySecretRef != nil {
		// WEBHOOK_URL: Slack incoming-webhook URL for the _webhook_notify builtin.
		modelRouterContainer.Env = append(modelRouterContainer.Env, corev1.EnvVar{
			Name:      "WEBHOOK_URL",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.WebhookNotifySecretRef},
		})
	}

	// --- Assemble volumes ---

	var volumes []corev1.Volume
	volumes = append(volumes, cfg.AgentSecretVolumes...)
	volumes = append(volumes, cfg.ProviderVolumes...)
	volumes = append(volumes, cfg.ToolSecretVolumes...)
	volumes = append(volumes, cfg.MCPBinVolumes...)
	volumes = append(volumes, routerConfigVol)
	if cfg.RedisSecretName != "" {
		volumes = append(volumes, redisSecretVolume(cfg.RedisSecretName))
	}
	if cfg.WarmLocalCacheEnabled {
		volumes = append(volumes, corev1.Volume{
			Name: WarmCacheVolName,
			VolumeSource: corev1.VolumeSource{
				// Disk-backed emptyDir (default medium): survives container/sidecar
				// restarts within the pod and in-place pod restarts, unlike the
				// memory-backed /tmp volume. Wiped on pod deletion — Redis is the
				// durable backstop.
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: cfg.WarmLocalCacheSizeLimit,
				},
			},
		})
	}

	// --- Pod ---

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:         cfg.PodName,
			GenerateName: cfg.GenerateName,
			Namespace:    cfg.Namespace,
			Labels:       cfg.Labels,
			Annotations:  cfg.Annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: cfg.ServiceAccount,
			RestartPolicy:      cfg.RestartPolicy,
			// Model-router is a native sidecar (initContainer with restartPolicy: Always).
			// MCP tool binaries are available via image volumes mounted on the model-router.
			InitContainers: []corev1.Container{modelRouterContainer},
			Containers:     []corev1.Container{agentContainer},
			Volumes:        volumes,
		},
	}

	// --- Post-processing (order matters) ---

	// Apply Azure Workload Identity label if needed.
	security.ApplyAzurePodLabel(pod, cfg.CloudProvider)

	// Inject framework-specific configuration (tier 1/2/3 adoption layer).
	// This may add init containers, env vars, or volume mounts depending on the framework.
	framework.Inject(pod, cfg.Agent.Spec.Runtime, routerBaseURL)

	// Apply pod security hardening (PSA restricted + projected SA token + /tmp emptyDir),
	// scoped-relaxed for the agent container when an override is configured.
	// Called last so it covers all containers including framework init containers.
	security.EnforcePodSecurity(pod, cfg.Agent.Spec.Runtime.SecurityContextOverride)

	return pod
}

// FrameworkEnvVars returns additional env vars for specific agent frameworks.
// routerBaseURL is the HTTP base URL to the model-router (e.g. "http://localhost:8080" for
// combined-pod topology; the router Service URL for split-pod topology).
// The framework adoption layer (tier 1/2/3) adds further per-framework config;
// this covers the common openai-compatible tier-1 defaults.
func FrameworkEnvVars(fw string, routerBaseURL string) []corev1.EnvVar {
	switch fw {
	case "langgraph":
		// LangGraph uses LANGGRAPH_CHECKPOINT_URL for its own native checkpointing.
		// Value is set by the Helm chart if a Postgres backend is configured.
		return []corev1.EnvVar{
			{Name: "OPENAI_API_BASE", Value: routerBaseURL}, // LangChain legacy env
		}
	case "autogen":
		return []corev1.EnvVar{
			// AutoGen v0.4 reads OAI_CONFIG_LIST; injected by an init container in the
			// framework adoption layer. For now expose the base URL env var.
			{Name: "OPENAI_BASE_URL", Value: routerBaseURL + "/v1"},
		}
	default:
		// "openai-compatible", "semantic-kernel", "shim", "" — all use OPENAI_BASE_URL.
		return nil
	}
}

// BuildAgentOnly constructs a *corev1.Pod containing only the agent container.
// Used in split-pod topology where the model-router runs in a separate pod.
// The agent reaches the router via the router Service URL in RouterBaseURL.
// The caller is responsible for setting OwnerReferences on the returned pod.
func BuildAgentOnly(cfg PodConfig) *corev1.Pod {
	routerBaseURL := cfg.RouterBaseURL
	if routerBaseURL == "" {
		routerBaseURL = "http://localhost:8080"
	}

	agentEnv := make([]corev1.EnvVar, len(cfg.AgentEnv)) //nolint:prealloc

	copy(agentEnv, cfg.AgentEnv)
	agentEnv = append(agentEnv, FrameworkEnvVars(cfg.Agent.Spec.Runtime.Framework, routerBaseURL)...)

	agentContainer := corev1.Container{
		Name:            "agent",
		Image:           cfg.Agent.Spec.Runtime.OCIRef,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         cfg.Agent.Spec.Runtime.Command,
		Args:            cfg.Agent.Spec.Runtime.Args,
		Env:             agentEnv,
		EnvFrom: []corev1.EnvFromSource{
			{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.TokenSecretName},
				},
			},
		},
	}
	if cfg.Agent.Spec.Resources != nil {
		agentContainer.Resources = *cfg.Agent.Spec.Resources
	}
	if cfg.AgentReadinessProbe != nil {
		agentContainer.ReadinessProbe = cfg.AgentReadinessProbe
	}
	agentContainer.VolumeMounts = append(agentContainer.VolumeMounts, cfg.AgentSecretMounts...)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:         cfg.PodName,
			GenerateName: cfg.GenerateName,
			Namespace:    cfg.Namespace,
			Labels:       cfg.Labels,
			Annotations:  cfg.Annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: cfg.ServiceAccount,
			RestartPolicy:      cfg.RestartPolicy,
			Containers:         []corev1.Container{agentContainer},
			Volumes:            cfg.AgentSecretVolumes,
		},
	}

	security.ApplyAzurePodLabel(pod, cfg.CloudProvider)
	framework.Inject(pod, cfg.Agent.Spec.Runtime, routerBaseURL)
	security.EnforcePodSecurity(pod, cfg.Agent.Spec.Runtime.SecurityContextOverride)

	return pod
}

// BuildRouterOnly constructs a *corev1.Pod containing only the model-router container.
// Used in split-pod topology where the agent runs in a separate pod and reaches the
// router via a Kubernetes Service. The router runs as a regular (non-sidecar) container.
// The caller is responsible for setting OwnerReferences on the returned pod.
func BuildRouterOnly(cfg PodConfig) *corev1.Pod {
	routerProbePort := cfg.RouterProbePort
	if routerProbePort == 0 {
		routerProbePort = 8080
	}

	routerResources := routerResourcesFor(cfg)

	routerConfigVol := corev1.Volume{
		Name: RouterConfigVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cfg.RouterConfigName},
			},
		},
	}

	routerPorts := []corev1.ContainerPort{ //nolint:prealloc

		{Name: "openai", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		{Name: "gemini", ContainerPort: 8082, Protocol: corev1.ProtocolTCP},
		// Metrics endpoint served by the model-router on :9091 (Prometheus/OpenMetrics).
		// Declared as a containerPort so a PodMonitor/ServiceMonitor can resolve
		// port: metrics and so cluster networking tooling can see it.
		{Name: "metrics", ContainerPort: 9091, Protocol: corev1.ProtocolTCP},
	}
	routerPorts = append(routerPorts, cfg.RouterExtraPorts...)

	routerMounts := make([]corev1.VolumeMount, 0, len(cfg.ProviderMounts)+len(cfg.ToolSecretMounts)+len(cfg.MCPBinMounts)+1)
	routerMounts = append(routerMounts, cfg.ProviderMounts...)
	routerMounts = append(routerMounts, cfg.ToolSecretMounts...)
	routerMounts = append(routerMounts, cfg.MCPBinMounts...)
	routerMounts = append(routerMounts, corev1.VolumeMount{
		Name:      RouterConfigVolume,
		MountPath: RouterConfigDir,
		ReadOnly:  true,
	})
	if cfg.RedisSecretName != "" {
		routerMounts = append(routerMounts, redisSecretMount())
	}

	routerContainer := corev1.Container{
		Name:            "model-router",
		Image:           cfg.ModelRouterImage,
		ImagePullPolicy: routerPullPolicy(cfg.ModelRouterImage),
		Env: []corev1.EnvVar{
			{Name: "AGENTORC_ROUTER_CONFIG", Value: RouterConfigDir + "/" + RouterConfigKey},
		},
		Ports:        routerPorts,
		VolumeMounts: routerMounts,
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt(routerProbePort),
				},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
			FailureThreshold:    30,
		},
		Resources: routerResources,
	}
	if cfg.HindsightAPIKeySecretRef != nil {
		routerContainer.Env = append(routerContainer.Env, corev1.EnvVar{
			Name:      "HINDSIGHT_API_KEY",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.HindsightAPIKeySecretRef},
		})
	}

	var volumes []corev1.Volume //nolint:prealloc

	volumes = append(volumes, cfg.ProviderVolumes...)
	volumes = append(volumes, cfg.ToolSecretVolumes...)
	volumes = append(volumes, cfg.MCPBinVolumes...)
	volumes = append(volumes, routerConfigVol)
	if cfg.RedisSecretName != "" {
		volumes = append(volumes, redisSecretVolume(cfg.RedisSecretName))
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:         cfg.PodName,
			GenerateName: cfg.GenerateName,
			Namespace:    cfg.Namespace,
			Labels:       cfg.Labels,
			Annotations:  cfg.Annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: cfg.ServiceAccount,
			RestartPolicy:      cfg.RestartPolicy,
			Containers:         []corev1.Container{routerContainer},
			Volumes:            volumes,
		},
	}

	security.ApplyAzurePodLabel(pod, cfg.CloudProvider)
	// Router-only pod: no agent container, so no override is applicable.
	security.EnforcePodSecurity(pod, nil)

	return pod
}

// redisSecretVolume is the Secret volume carrying the Redis password. Only the
// password key is projected, read-only for the owner.
func redisSecretVolume(secretName string) corev1.Volume {
	mode := int32(0o400)
	return corev1.Volume{
		Name: RedisSecretVolume,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
				Items: []corev1.KeyToPath{
					{Key: RedisSecretKey, Path: RedisSecretKey},
				},
				DefaultMode: &mode,
			},
		},
	}
}

// redisSecretMount mounts the Redis password Secret into the model-router.
func redisSecretMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      RedisSecretVolume,
		MountPath: RedisSecretDir,
		ReadOnly:  true,
	}
}

// routerPullPolicy is Always for a development build of the router and
// IfNotPresent otherwise. A development deploy pushes a new image under the same
// "-dirty" tag every time, so a node that kept the first one would run stale
// router code. A release tag or a digest never changes, so it is pulled once.
func routerPullPolicy(image string) corev1.PullPolicy {
	if strings.Contains(image, "@sha256:") || !strings.HasSuffix(image, "-dirty") {
		return corev1.PullIfNotPresent
	}
	return corev1.PullAlways
}

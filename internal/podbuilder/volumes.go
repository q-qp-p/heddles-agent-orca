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

package podbuilder

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

var sanitizeVolumeRe = regexp.MustCompile(`[^a-z0-9-]`)

// SanitizeVolumeName converts an arbitrary string to a valid Kubernetes volume name.
func SanitizeVolumeName(name string) string {
	s := strings.ToLower(name)
	s = sanitizeVolumeRe.ReplaceAllString(s, "-")
	if len(s) > 50 {
		s = s[:50]
	}
	return strings.Trim(s, "-")
}

// ToolSecretFilePath returns the mount path for a secret key used by a tool.
func ToolSecretFilePath(toolName, secretName, key string) string {
	return fmt.Sprintf("%s/%s/%s/%s", ToolSecretsDir, toolName, secretName, key)
}

// OAuthCredsMountDir returns the directory at which an MCP tool's OAuth credentials
// Secret (MCPOAuthConfig.Credentials) is mounted as files for the model-router's MCP
// client to read (client_id, client_secret, redirect_uri, refresh_token).
func OAuthCredsMountDir(toolName, secretName string) string {
	return fmt.Sprintf("%s/%s/%s", ToolSecretsDir, toolName, secretName)
}

// ResolveProviderVolumes builds volume + mount lists for provider API key secrets.
// extra names further ModelProviders to mount that the selector does not list.
func ResolveProviderVolumes(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	agent *agentorcav1alpha1.Agent,
	extra ...string,
) ([]corev1.Volume, []corev1.VolumeMount, error) {
	var selector agentorcav1alpha1.ModelSelector
	if err := reader.Get(ctx, client.ObjectKey{Name: agent.Spec.ModelSelectorRef, Namespace: namespace}, &selector); err != nil {
		return nil, nil, fmt.Errorf("getting ModelSelector: %w", err)
	}

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	seen := make(map[string]bool)

	mountProvider := func(name string, skipNotFound bool) error {
		if seen[name] {
			return nil
		}
		seen[name] = true
		var mp agentorcav1alpha1.ModelProvider
		if err := reader.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &mp); err != nil {
			if skipNotFound && apierrors.IsNotFound(err) {
				// Provider is listed in the fallback chain but not deployed (e.g. disabled
				// in the model-providers chart). Skip it rather than blocking pod creation.
				return nil
			}
			return fmt.Errorf("getting ModelProvider %q for volumes: %w", name, err)
		}
		volName := ProviderVolPrefix + SanitizeVolumeName(name)
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: mp.Spec.CredentialsRef.Name,
					Items: []corev1.KeyToPath{
						{Key: mp.Spec.CredentialsRef.Key, Path: "api-key"},
					},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: fmt.Sprintf("%s/%s", ProviderSecretsDir, name),
			ReadOnly:  true,
		})
		return nil
	}

	for _, pw := range selector.Spec.Providers {
		if err := mountProvider(pw.Name, false); err != nil {
			return nil, nil, err
		}
	}
	for _, name := range selector.Spec.FallbackChain {
		if err := mountProvider(name, true); err != nil {
			return nil, nil, err
		}
	}
	// Providers outside the selector that the pod still calls, such as a
	// benchmark's judge.
	for _, name := range extra {
		if err := mountProvider(name, false); err != nil {
			return nil, nil, err
		}
	}
	return volumes, mounts, nil
}

// ResolveToolSecretVolumes builds volumes and mounts for secrets referenced by MCP tools
// (envFrom, auth, and secretRefs). These are mounted on the model-router sidecar.
func ResolveToolSecretVolumes(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	agent *agentorcav1alpha1.Agent,
) ([]corev1.Volume, []corev1.VolumeMount, error) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	// Track (toolName, secretName, key) to avoid duplicate volumes.
	type volKey struct{ tool, secret, key string }
	seen := make(map[volKey]bool)

	addSecretKeyVolume := func(toolName string, ref *agentorcav1alpha1.SecretKeyRef, asDir bool) {
		vk := volKey{toolName, ref.Name, ref.Key}
		if seen[vk] {
			return
		}
		seen[vk] = true

		volName := ToolSecretVolPrefix + SanitizeVolumeName(toolName+"-"+ref.Name+"-"+ref.Key)
		filePath := ToolSecretFilePath(toolName, ref.Name, ref.Key)

		vol := corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: ref.Name,
					Items: []corev1.KeyToPath{
						{Key: ref.Key, Path: ref.Key},
					},
				},
			},
		}
		mount := corev1.VolumeMount{
			Name:     volName,
			ReadOnly: true,
		}
		if asDir {
			// Mount the secret as a directory (no SubPath) so the kubelet propagates
			// updates to the mounted file. Used for HTTP bearer tokens the operator
			// rotates in-place; the model-router re-reads the file per request.
			mount.MountPath = filepath.Dir(filePath)
		} else {
			// Mount a single file at the exact path (SubPath snapshot). Used for stdio
			// envFrom vars, which don't rotate.
			mount.MountPath = filePath
			mount.SubPath = ref.Key
		}
		volumes = append(volumes, vol)
		mounts = append(mounts, mount)
	}

	for _, toolName := range agent.Spec.Tools {
		var tool agentorcav1alpha1.Tool
		if err := reader.Get(ctx, client.ObjectKey{Name: toolName, Namespace: namespace}, &tool); err != nil {
			continue // already validated in buildRouterConfig
		}
		if tool.Spec.Type != agentorcav1alpha1.ToolTypeMCP || tool.Spec.MCPConfig == nil {
			continue
		}

		// EnvFrom secret refs.
		for _, ev := range tool.Spec.MCPConfig.EnvFrom {
			if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil {
				addSecretKeyVolume(toolName, ev.ValueFrom.SecretKeyRef, false)
			}
		}

		// Auth secret refs.
		if auth := tool.Spec.MCPConfig.Auth; auth != nil {
			if auth.BearerToken != nil {
				addSecretKeyVolume(toolName, auth.BearerToken, true)
			}
			if auth.APIKey != nil {
				addSecretKeyVolume(toolName, &auth.APIKey.SecretKeyRef, true)
			}
			for i := range auth.Headers {
				addSecretKeyVolume(toolName, &auth.Headers[i].SecretKeyRef, true)
			}

			if auth.OAuth != nil {
				// OAuth-managed server: mount the credentials Secret as a directory so the
				// model-router's MCP client can read client_id/client_secret/refresh_token
				// files and perform the OAuth exchange itself. No access-token Secret needed.
				credDir := OAuthCredsMountDir(toolName, auth.OAuth.Credentials.Name)
				volName := ToolSecretVolPrefix + SanitizeVolumeName(toolName+"-"+auth.OAuth.Credentials.Name+"-oauth")
				volumes = append(volumes, corev1.Volume{
					Name: volName,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: auth.OAuth.Credentials.Name},
					},
				})
				mounts = append(mounts, corev1.VolumeMount{
					Name:      volName,
					MountPath: credDir,
					ReadOnly:  true,
				})
			}
		}

		// Tool-level secretRefs for MCP sidecar tools.
		for _, sr := range tool.Spec.SecretRefs {
			key := volKey{toolName, sr.Name, ""}
			if seen[key] {
				continue
			}
			seen[key] = true

			volName := ToolSecretVolPrefix + SanitizeVolumeName(toolName+"-"+sr.Name)
			mountPath := sr.MountPath
			if mountPath == "" {
				mountPath = fmt.Sprintf("%s/%s/%s", ToolSecretsDir, toolName, sr.Name)
			}
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: sr.Name},
				},
			})
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: mountPath,
				ReadOnly:  true,
			})
		}
	}
	return volumes, mounts, nil
}

// ResolveAgentSecretRefs builds volumes and mounts for secrets referenced by an
// Agent's runtime (Agent.spec.runtime.secretRefs). These are mounted read-only into
// the agent container only — not the model-router sidecar. Each entry with a MountPath
// becomes a whole-secret volume mounted at that path; an empty MountPath defaults to
// /etc/agentorca-secrets/<secret-name> (keys as files). This is a pure transform: the
// kubelet resolves the secret at pod start.
//
// Use case: a red-team pwnbox mounting an HTB OpenVPN .ovpn config directly into the
// agent container so the entrypoint can bring up the tunnel.
func ResolveAgentSecretRefs(agent *agentorcav1alpha1.Agent) ([]corev1.Volume, []corev1.VolumeMount) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if agent == nil {
		return volumes, mounts
	}
	for _, sr := range agent.Spec.Runtime.SecretRefs {
		if sr.Name == "" {
			continue
		}
		mountPath := sr.MountPath
		if mountPath == "" {
			mountPath = fmt.Sprintf("%s/%s", AgentSecretMountDir, sr.Name)
		}
		volName := AgentSecretVolPrefix + SanitizeVolumeName(agent.Name+"-"+sr.Name)
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: sr.Name},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: mountPath,
			ReadOnly:  true,
		})
	}
	return volumes, mounts
}

// ResolveMCPSidecarVolumes inspects the agent's MCP tools and builds image
// volumes + mounts to inject MCP server binaries into the model-router.
//
// For each MCP tool with executionMode=sidecar, ociRef set, and stdio transport
// with args, the function uses a Kubernetes image volume (GA since 1.33) to
// mount the tool's OCI image filesystem directly. The model-router can then
// fork/exec the binary at its original path inside the mounted image.
//
// This avoids init containers entirely -- no cp, no busybox, no emptyDir.
// Works even with scratch/distroless images since the kubelet mounts the image
// layers directly as a read-only filesystem.
func ResolveMCPSidecarVolumes(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	agent *agentorcav1alpha1.Agent,
) ([]corev1.Volume, []corev1.VolumeMount, map[string]string, error) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	binPathRewrites := make(map[string]string) // toolName -> rewritten binary path

	for _, toolName := range agent.Spec.Tools {
		var tool agentorcav1alpha1.Tool
		if err := reader.Get(ctx, client.ObjectKey{Name: toolName, Namespace: namespace}, &tool); err != nil {
			continue
		}
		if tool.Spec.Type != agentorcav1alpha1.ToolTypeMCP || tool.Spec.MCPConfig == nil {
			continue
		}
		if tool.Spec.ExecutionMode != agentorcav1alpha1.ToolExecSidecar || tool.Spec.OCIRef == "" {
			continue
		}
		args := tool.Spec.MCPConfig.Args
		if tool.Spec.MCPConfig.Transport != "stdio" || len(args) == 0 {
			continue
		}
		// Mount the entire OCI image filesystem at /mcp-img/<toolName>.
		// The binary is then available at /mcp-img/<toolName>/<original-path>.
		mountDir := fmt.Sprintf("%s/%s", MCPBinDir, toolName)
		volName := "mcp-img-" + SanitizeVolumeName(toolName)
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Image: &corev1.ImageVolumeSource{
					Reference:  tool.Spec.OCIRef,
					PullPolicy: corev1.PullIfNotPresent,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: mountDir,
			ReadOnly:  true,
		})

		// Store the mount directory so the config builder can rewrite all absolute
		// paths in the tool's args (binary + script arguments).
		// e.g. mountDir="/mcp-img/github-mcp" rewrites /server/binary -> /mcp-img/github-mcp/server/binary
		binPathRewrites[toolName] = mountDir
	}

	return volumes, mounts, binPathRewrites, nil
}

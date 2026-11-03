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
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
)

// resolveClassCredentials reads the value of each of the class's credentials
// for a workload in namespace. A secretRef without a namespace reads the
// workload's own namespace; any other namespace must be one of the
// allowedNamespaces of the TenantConfig the workload's namespace belongs to.
// Only secretRef credentials are read here; a githubApp credential is minted per
// run as git access (see openshell.ClassGitHub). Failures wrap errSandboxClassUnusable: the class cannot serve this workload,
// and no sandbox is created without the credentials the class promises.
func resolveClassCredentials(ctx context.Context, cl client.Reader, namespace string, class *agentorcav1alpha1.SandboxClass) ([]*openshell.ClassCredential, error) {
	if class == nil || len(class.Spec.Credentials) == 0 {
		return nil, nil
	}
	creds := make([]*openshell.ClassCredential, 0, len(class.Spec.Credentials))
	var tenantNamespaces []string // loaded on first use
	for i := range class.Spec.Credentials {
		spec := &class.Spec.Credentials[i]
		ref := spec.Source.SecretRef
		if ref == nil {
			continue // githubApp credentials become the git access (openshell.ClassGitHub)
		}
		secretNamespace := ref.Namespace
		if secretNamespace == "" {
			secretNamespace = namespace
		}
		if secretNamespace != namespace {
			if tenantNamespaces == nil {
				var err error
				if tenantNamespaces, err = tenantNamespacesOf(ctx, cl, namespace); err != nil {
					return nil, err
				}
			}
			if !slices.Contains(tenantNamespaces, secretNamespace) {
				return nil, fmt.Errorf("%w: SandboxClass %q credential %q reads a Secret in namespace %q, which is not in the tenant of namespace %q",
					errSandboxClassUnusable, class.Name, spec.Name, secretNamespace, namespace)
			}
		}
		var secret corev1.Secret
		if err := cl.Get(ctx, client.ObjectKey{Namespace: secretNamespace, Name: ref.Name}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("%w: SandboxClass %q credential %q: Secret %s/%s not found",
					errSandboxClassUnusable, class.Name, spec.Name, secretNamespace, ref.Name)
			}
			return nil, fmt.Errorf("reading Secret %s/%s for credential %q: %w", secretNamespace, ref.Name, spec.Name, err)
		}
		v, ok := secret.Data[ref.Key]
		if !ok {
			return nil, fmt.Errorf("%w: SandboxClass %q credential %q: Secret %s/%s has no key %q",
				errSandboxClassUnusable, class.Name, spec.Name, secretNamespace, ref.Name, ref.Key)
		}
		c := openshell.NewClassCredential(class.Name, spec)
		// Match the router, which trims mounted secret files.
		c.Value = strings.TrimSpace(string(v))
		creds = append(creds, c)
	}
	return creds, nil
}

// tenantNamespacesOf returns the allowedNamespaces of every TenantConfig that
// lists namespace, plus namespace itself. A namespace in no TenantConfig has
// only itself.
func tenantNamespacesOf(ctx context.Context, cl client.Reader, namespace string) ([]string, error) {
	var tenants agentorcav1alpha1.TenantConfigList
	if err := cl.List(ctx, &tenants); err != nil {
		return nil, fmt.Errorf("listing TenantConfigs: %w", err)
	}
	out := []string{namespace}
	for i := range tenants.Items {
		allowed := tenants.Items[i].Spec.AllowedNamespaces
		if slices.Contains(allowed, namespace) {
			out = append(out, allowed...)
		}
	}
	return out, nil
}

// ensureClassCredentials creates or updates the profile and the owner's
// provider of each credential on the gateway and sets each credential's
// Provider. It returns the provider names to attach to the sandbox. owner
// names the sandbox's owner within the namespace and labels identify it for
// cleanup.
func ensureClassCredentials(ctx context.Context, gw openshell.Gateway, namespace, owner string, labels map[string]string, creds []*openshell.ClassCredential) ([]string, error) {
	providers := make([]string, 0, len(creds))
	for _, c := range creds {
		if err := gw.EnsureProfile(ctx, openshell.ClassCredentialProfile(namespace, c)); err != nil {
			return nil, err
		}
		p := openshell.ClassCredentialProvider(namespace, owner, c, labels)
		if err := gw.EnsureProvider(ctx, p); err != nil {
			return nil, err
		}
		c.Provider = p.Name
		providers = append(providers, p.Name)
	}
	return providers, nil
}

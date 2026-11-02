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

package apiserver

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// callSource is where a router's calls come from: an AgentRun, or an
// AgentDeployment (whose sidecar names itself after the deployment). Either way
// the work belongs to one Agent, and routing is judged per Agent.
type callSource struct {
	Kind   string
	Name   string
	Agent  string
	Tenant string
}

// resolveSource finds the AgentRun, or failing that the AgentDeployment, a
// router calls itself. Everything the operator records about a call comes from
// here, never from the router's request body.
func (s *Server) resolveSource(ctx context.Context, namespace, name string) (callSource, error) {
	key := client.ObjectKey{Name: name, Namespace: namespace}
	var run agentorcav1alpha1.AgentRun
	switch err := s.crdClient.Get(ctx, key, &run); {
	case err == nil:
		return callSource{Kind: postgresql.SourceKindRun, Name: name, Agent: run.Spec.AgentRef, Tenant: run.Labels["agentorca.io/tenant"]}, nil
	case !apierrors.IsNotFound(err):
		return callSource{}, fmt.Errorf("getting AgentRun: %w", err)
	}
	var dep agentorcav1alpha1.AgentDeployment
	if err := s.crdClient.Get(ctx, key, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return callSource{}, fmt.Errorf("no AgentRun or AgentDeployment %s/%s: %w", namespace, name, err)
		}
		return callSource{}, fmt.Errorf("getting AgentDeployment: %w", err)
	}
	return callSource{Kind: postgresql.SourceKindDeployment, Name: name, Agent: dep.Spec.AgentRef, Tenant: dep.Labels["agentorca.io/tenant"]}, nil
}

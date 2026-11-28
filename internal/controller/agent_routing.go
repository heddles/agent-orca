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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch/catalogue"
)

// routingStatus is the agent's routing card, or nil when it opted out.
func (r *AgentReconciler) routingStatus(ctx context.Context, agent *agentorcav1alpha1.Agent) (*agentorcav1alpha1.AgentRoutingStatus, error) {
	if !catalogue.Routable(agent) {
		return nil, nil
	}
	var related catalogue.Related
	for _, name := range agent.Spec.Tools {
		var tool agentorcav1alpha1.Tool
		err := r.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, &tool)
		if apierrors.IsNotFound(err) {
			continue // The card still names it.
		}
		if err != nil {
			return nil, fmt.Errorf("getting tool %s for the routing card: %w", name, err)
		}
		related.Tools = append(related.Tools, tool)
	}
	for _, name := range catalogue.KnowledgeBaseNames(agent) {
		var kb agentorcav1alpha1.KnowledgeBase
		err := r.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, &kb)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("getting knowledge base %s for the routing card: %w", name, err)
		}
		related.KnowledgeBases = append(related.KnowledgeBases, kb)
	}
	return &agentorcav1alpha1.AgentRoutingStatus{Card: catalogue.Card(agent, related)}, nil
}

// agentsReferring enqueues the agents, in an object's namespace, that refer to it
// by name, so their routing cards follow it.
func (r *AgentReconciler) agentsReferring(names func(*agentorcav1alpha1.Agent) []string) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		var agents agentorcav1alpha1.AgentList
		if err := r.List(ctx, &agents, client.InNamespace(obj.GetNamespace())); err != nil {
			logf.FromContext(ctx).Error(err, "listing agents to rebuild routing cards", "namespace", obj.GetNamespace())
			return nil
		}
		var out []reconcile.Request
		for i := range agents.Items {
			if slices.Contains(names(&agents.Items[i]), obj.GetName()) {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: agents.Items[i].Namespace, Name: agents.Items[i].Name}})
			}
		}
		return out
	})
}

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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch"
	"github.com/heddles/agent-orca/internal/inbound"
)

const sourceChat = "chat"

// SetDispatcher lets chat places pick their agent automatically.
func (s *UIServer) SetDispatcher(d AgentDispatcher) { s.dispatcher = d }

// Pick picks the agent for a chat message among the ones a place offers its
// sender, by the checks Launch would make: an agent that could not be started
// is not in the running.
func (b *InboundBridge) Pick(ctx context.Context, r inbound.PickRequest) (inbound.Picked, error) {
	var none inbound.Picked
	s := b.s
	identity, ok := s.tenantForHook(r.TenantID)
	if !ok {
		return none, inbound.ErrNoneToPick
	}
	ctx = contextWithTenant(ctx, identity)
	req := dispatch.Request{
		Tenant: dispatchTenant(identity), Text: r.Text, Source: sourceChat, Only: []dispatch.Ref{},
	}
	for _, ref := range r.Among {
		var agent agentorcav1alpha1.Agent
		if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &agent); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return none, fmt.Errorf("reading agent %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		published, err := s.agentPublished(ctx, ref.Namespace, ref.Name)
		if err != nil {
			return none, fmt.Errorf("checking that %s/%s is published: %w", ref.Namespace, ref.Name, err)
		}
		if published {
			req.Only = append(req.Only, dispatch.Ref{Namespace: ref.Namespace, Name: ref.Name})
		}
	}
	if r.Default != nil {
		req.PlaceDefault = &dispatch.Ref{Namespace: r.Default.Namespace, Name: r.Default.Name}
	}
	if s.dispatcher == nil {
		return none, inbound.ErrNoPicker
	}
	res, err := s.dispatcher.Dispatch(ctx, req)
	var undecided *dispatch.UndecidedError
	switch {
	case errors.Is(err, dispatch.ErrOff):
		return none, inbound.ErrNoPicker
	case errors.Is(err, dispatch.ErrNoAgents):
		return none, inbound.ErrNoneToPick
	case errors.As(err, &undecided):
		return none, inbound.ErrUndecided
	case err != nil:
		return none, err
	}
	return inbound.Picked{
		Agent:   inbound.AgentRef{Namespace: res.Agent.Namespace, Name: res.Agent.Name},
		Routing: mustJSON(routingInfo(res)),
	}, nil
}

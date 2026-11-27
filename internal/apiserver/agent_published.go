package apiserver

// An agent is published when people and systems may start it: from the agent
// page, a webhook trigger, a channel or a shared chat app. An AgentDeployment
// publishes the agent it runs. Without one, an agent that is not managed by the
// UI is published as soon as its spec can run, because whoever applied it (a Git
// commit, usually) has no publish step. An agent built in the UI stays a draft
// until its builder publishes it, which is where the evaluation gate applies.

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/agentrefs"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	reasonTemplate = "This is a template: make your own copy first."
	reasonDraft    = "This is a draft. Publish it from Build."
)

// agentRef is one thing an agent's spec names, without which its runs cannot start.
type agentRef struct{ kind, name string }

func refsOf(a *agentorcav1alpha1.Agent) []agentRef {
	var out []agentRef
	if a.Spec.ModelSelectorRef != "" {
		out = append(out, agentRef{"ModelSelector", a.Spec.ModelSelectorRef})
	}
	for _, t := range a.Spec.Tools {
		if !agentrefs.BuiltinTools[t] {
			out = append(out, agentRef{"Tool", t})
		}
	}
	for _, kb := range a.Spec.KnowledgeBases {
		out = append(out, agentRef{"KnowledgeBase", kb})
	}
	return out
}

// unpublishedReason says why the agent is not published, or "" when it is.
// exists reports whether a referenced object is in the agent's namespace.
func (s *UIServer) unpublishedReason(a *agentorcav1alpha1.Agent, deployed bool, exists func(agentRef) (bool, error)) (string, error) {
	switch {
	case a.Labels[LabelTemplate] == templateMarker:
		return reasonTemplate, nil
	case deployed:
		return "", nil
	case tenancy.Detect(a, s.ownDetect).Owner == tenancy.OwnerUI:
		return reasonDraft, nil
	}
	for _, ref := range refsOf(a) {
		ok, err := exists(ref)
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("It cannot run: %s %q does not exist.", ref.kind, ref.name), nil
		}
	}
	return "", nil
}

// notPublishedReason is unpublishedReason for one agent, read from the cluster.
func (s *UIServer) notPublishedReason(ctx context.Context, a *agentorcav1alpha1.Agent) (string, error) {
	deployed, err := s.agentDeployed(ctx, a.Namespace, a.Name)
	if err != nil {
		return "", err
	}
	return s.unpublishedReason(a, deployed, func(ref agentRef) (bool, error) {
		var obj client.Object
		switch ref.kind {
		case "ModelSelector":
			obj = &agentorcav1alpha1.ModelSelector{}
		case "Tool":
			obj = &agentorcav1alpha1.Tool{}
		default:
			obj = &agentorcav1alpha1.KnowledgeBase{}
		}
		err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: ref.name}, obj)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	})
}

// agentDeployed is true when an AgentDeployment in ns runs the agent. Chat and
// the chat widget need one: they talk to its running pods.
func (s *UIServer) agentDeployed(ctx context.Context, ns, agent string) (bool, error) {
	var deps agentorcav1alpha1.AgentDeploymentList
	if err := s.crdClient.List(ctx, &deps, client.InNamespace(ns)); err != nil {
		return false, err
	}
	for i := range deps.Items {
		if deps.Items[i].Spec.AgentRef == agent {
			return true, nil
		}
	}
	return false, nil
}

// refIndex is the referenced objects of a list of agents, read once.
type refIndex map[string]bool

func (x refIndex) add(kind, ns, name string) { x[kind+"/"+ns+"/"+name] = true }

// lookup answers unpublishedReason for the agents of one namespace.
func (x refIndex) lookup(ns string) func(agentRef) (bool, error) {
	return func(ref agentRef) (bool, error) { return x[ref.kind+"/"+ns+"/"+ref.name], nil }
}

// listAgentRefs indexes what agents in ns can refer to; tools are already listed.
func (s *UIServer) listAgentRefs(ctx context.Context, ns string, tools []agentorcav1alpha1.Tool) (refIndex, error) {
	var selectors agentorcav1alpha1.ModelSelectorList
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &selectors, ns); err != nil {
		return nil, err
	}
	var kbs agentorcav1alpha1.KnowledgeBaseList
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &kbs, ns); err != nil {
		return nil, err
	}
	x := refIndex{}
	for i := range selectors.Items {
		x.add("ModelSelector", selectors.Items[i].Namespace, selectors.Items[i].Name)
	}
	for i := range tools {
		x.add("Tool", tools[i].Namespace, tools[i].Name)
	}
	for i := range kbs.Items {
		x.add("KnowledgeBase", kbs.Items[i].Namespace, kbs.Items[i].Name)
	}
	return x, nil
}

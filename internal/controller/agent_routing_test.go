package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch/catalogue"
)

func TestAgentRoutingCardFollowsTheAgent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "ops"},
		Spec: agentorcav1alpha1.AgentSpec{
			SystemPrompt: "Network.", Tools: []string{"flows", "gone"},
			ServiceAccountRef: &agentorcav1alpha1.LocalObjectRef{Name: "own"},
		},
	}
	agent.Finalizers = []string{agentFinalizer}
	tool := &agentorcav1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "flows", Namespace: "ops"},
		Spec:       agentorcav1alpha1.ToolSpec{Schema: &agentorcav1alpha1.ToolSchema{Description: "Reads flows"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, tool).WithStatusSubresource(agent).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Namespace: "ops", Name: "net"}
	ctx := context.Background()

	reconcileAndCard := func() string {
		t.Helper()
		if _, err := r.Reconcile(ctx, requestFor(key)); err != nil {
			t.Fatal(err)
		}
		var got agentorcav1alpha1.Agent
		if err := c.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.ServiceAccountName != "own" {
			t.Fatalf("service account: %q", got.Status.ServiceAccountName)
		}
		if got.Status.Routing == nil {
			return ""
		}
		return got.Status.Routing.Card
	}

	if got, want := reconcileAndCard(), "net: Network.\nTools: flows (Reads flows); gone"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	tool.Spec.Schema.Description = "Reads flow logs"
	if err := c.Update(ctx, tool); err != nil {
		t.Fatal(err)
	}
	if got, want := reconcileAndCard(), "net: Network.\nTools: flows (Reads flow logs); gone"; got != want {
		t.Fatalf("after a tool change got %q, want %q", got, want)
	}

	var current agentorcav1alpha1.Agent
	if err := c.Get(ctx, key, &current); err != nil {
		t.Fatal(err)
	}
	current.Labels = map[string]string{catalogue.OptOutLabel: "false"}
	if err := c.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	if got := reconcileAndCard(); got != "" {
		t.Fatalf("an opted-out agent kept the card %q", got)
	}
}

func requestFor(key types.NamespacedName) reconcile.Request {
	return reconcile.Request{NamespacedName: key}
}

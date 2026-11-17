package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func TestTenantBackfill(t *testing.T) {
	const idA, idB = "tnt_01935c7e-aaaa-7aaa-8bbb-00000000000a", "tnt_01935c7e-bbbb-7aaa-8bbb-00000000000b"
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	tc := func(name, id string, ns ...string) *agentorcav1alpha1.TenantConfig {
		return &agentorcav1alpha1.TenantConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       agentorcav1alpha1.TenantConfigSpec{TenantID: id, AllowedNamespaces: ns},
		}
	}
	agent := func(ns, name string, uid string, labels map[string]string) *agentorcav1alpha1.Agent {
		return &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + uid), Labels: labels}}
	}
	podOf := func(name string, owner *agentorcav1alpha1.Agent) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.Namespace, OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "agentorca.agentorca.io/v1alpha1", Kind: "Agent", Name: owner.Name, UID: owner.UID, Controller: new(true)},
		}}}
	}

	solo := agent("a-ns", "solo", "1", nil)                                                 // single claimant: stamped
	named := agent("shared", "named", "2", map[string]string{tenancy.LabelTenantName: "b"}) // name label picks the tenant
	ambiguous := agent("shared", "ambiguous", "3", nil)                                     // shared, no hint: left alone
	orphan := agent("nobody", "orphan", "4", nil)                                           // no tenant: left alone
	done := agent("a-ns", "done", "5", map[string]string{tenancy.LabelTenantID: idB})       // already labelled: untouched
	wrongName := agent("a-ns", "wrong-name", "6", map[string]string{tenancy.LabelTenantName: "b"})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tc("a", idA, "a-ns", "shared"), tc("b", idB, "shared"),
		solo, named, ambiguous, orphan, done, wrongName,
		podOf("solo-pod", solo), podOf("named-pod", named), podOf("ambiguous-pod", ambiguous),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "free", Namespace: "a-ns"}},                   // no owner: left alone
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hindsightKeySecretName, Namespace: "shared"}}, // operator-shared: platform
	).Build()

	stats, err := (&TenantBackfill{Client: c}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Stamped != 6 || stats.Unattributed != 2 {
		t.Errorf("stats = %+v, want 5 stamped (3 agents, 2 pods, the hindsight Secret) and 2 unattributed", stats)
	}

	label := func(obj client.Object) string {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		return obj.GetLabels()[tenancy.LabelTenantID]
	}
	checks := []struct {
		obj  client.Object
		want string
	}{
		{&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "a-ns"}}, idA},
		{&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "named", Namespace: "shared"}}, idB},
		{&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "ambiguous", Namespace: "shared"}}, ""},
		{&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "nobody"}}, ""},
		{&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "done", Namespace: "a-ns"}}, idB},
		// The name label points at a tenant that does not claim the namespace, so
		// the namespace's single claimant decides.
		{&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "wrong-name", Namespace: "a-ns"}}, idA},
		{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "solo-pod", Namespace: "a-ns"}}, idA},
		{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "named-pod", Namespace: "shared"}}, idB},
		{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ambiguous-pod", Namespace: "shared"}}, ""},
		{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "free", Namespace: "a-ns"}}, ""},
	}
	for _, ck := range checks {
		if got := label(ck.obj); got != ck.want {
			t.Errorf("%T %s/%s: tenant-id = %q, want %q", ck.obj, ck.obj.GetNamespace(), ck.obj.GetName(), got, ck.want)
		}
	}

	var hk corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: hindsightKeySecretName}, &hk); err != nil {
		t.Fatal(err)
	}
	if hk.Labels[tenancy.LabelScope] != tenancy.ScopePlatform || hk.Labels[tenancy.LabelTenantID] != "" {
		t.Errorf("hindsight key labels = %v, want scope=platform", hk.Labels)
	}

	// A second pass changes nothing.
	again, err := (&TenantBackfill{Client: c}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again.Stamped != 0 {
		t.Errorf("second pass stamped %d objects", again.Stamped)
	}
}

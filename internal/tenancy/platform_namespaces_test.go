package tenancy

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestPlatformNamespaceAccess(t *testing.T) {
	SetPlatformNamespaces([]string{"platform-ns"})
	t.Cleanup(func() { SetPlatformNamespaces(nil) })

	obj := func(ns string, labels map[string]string) *agentorcav1alpha1.Agent {
		return &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns, Labels: labels}}
	}
	caller := Caller{TenantID: "tnt_b", TenantName: "b", Namespaces: []string{"team-b"}}
	platform := map[string]string{LabelScope: ScopePlatform}
	tests := []struct {
		name string
		obj  *agentorcav1alpha1.Agent
		want Access
	}{
		{"platform object in a platform namespace", obj("platform-ns", platform), AccessPlatform},
		{"tenant object in a platform namespace", obj("platform-ns", map[string]string{LabelTenantID: "tnt_a"}), AccessNone},
		{"platform object in another foreign namespace", obj("other-ns", platform), AccessNone},
		{"platform object in the caller's namespace", obj("team-b", platform), AccessPlatform},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CheckAccess(caller, tt.obj, "Agent"); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlatformNamespacesFromEnv(t *testing.T) {
	t.Cleanup(func() { SetPlatformNamespaces(nil) })
	t.Setenv(podNamespaceEnvVar, "operator-ns")
	t.Setenv(PlatformNamespacesEnvVar, "")
	SetPlatformNamespacesFromEnv()
	if !IsPlatformNamespace("operator-ns") {
		t.Error("the operator namespace is the default")
	}
	t.Setenv(PlatformNamespacesEnvVar, " a, b ,,a")
	SetPlatformNamespacesFromEnv()
	if got := PlatformNamespaces(); len(got) != 2 || IsPlatformNamespace("operator-ns") || !IsPlatformNamespace("b") {
		t.Errorf("got %v", got)
	}
}

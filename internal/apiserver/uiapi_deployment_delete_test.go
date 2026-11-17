package apiserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func TestDeleteDeploymentNeedsUIOwnership(t *testing.T) {
	dep := func(name string, annotations map[string]string) *agentorcav1alpha1.AgentDeployment {
		return &agentorcav1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shared", Labels: map[string]string{tenancy.LabelTenantID: isoA}, Annotations: annotations,
		}}
	}
	f := newWriteFixture(t,
		dep("ui-made", map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}),
		dep("git-made", map[string]string{tenancy.AnnotationOwner: "git"}),
		dep("unowned", nil))
	del := func(name string) int {
		req := asTenant(httptest.NewRequest(http.MethodDelete, "/api/deployments/shared/"+name, nil), f.as(tenancy.RoleBuilder))
		rec := httptest.NewRecorder()
		f.s.handleDeployment(rec, req)
		return rec.Code
	}
	for _, name := range []string{"git-made", "unowned"} {
		if code := del(name); code != http.StatusForbidden {
			t.Errorf("delete %s = %d, want 403", name, code)
		}
	}
	if code := del("ui-made"); code != http.StatusOK {
		t.Errorf("delete ui-made = %d, want 200", code)
	}
	if len(f.audit.events) != 1 || f.audit.events[0].Name != "ui-made" {
		t.Errorf("audit = %+v", f.audit.events)
	}
}

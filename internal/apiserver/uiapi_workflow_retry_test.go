package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestRetryWorkflowStepRequestsRetryOfAFailedStep(t *testing.T) {
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Status: agentorcav1alpha1.AgentWorkflowStatus{Steps: []agentorcav1alpha1.WorkflowStepStatus{
			{Name: "bad", Phase: agentorcav1alpha1.WorkflowStepPhaseFailed},
			{Name: "ok", Phase: agentorcav1alpha1.WorkflowStepPhaseSucceeded},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(wf).Build()
	s := &UIServer{crdClient: c}
	post := func(path string) int {
		rec := httptest.NewRecorder()
		s.handleGetWorkflow(rec, httptest.NewRequest(http.MethodPost, path, nil))
		return rec.Code
	}

	if code := post("/api/workflows/default/wf/steps/ok/retry"); code != http.StatusConflict {
		t.Fatalf("retry of a step that did not fail = %d, want 409", code)
	}
	if code := post("/api/workflows/default/wf/steps/bad/retry"); code != http.StatusOK {
		t.Fatalf("retry of a failed step = %d, want 200", code)
	}
	var got agentorcav1alpha1.AgentWorkflow
	if err := c.Get(context.Background(), client.ObjectKey{Name: "wf", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[agentorcav1alpha1.RetryStepAnnotation] != "bad" {
		t.Fatalf("annotations = %v", got.Annotations)
	}
	rec := httptest.NewRecorder()
	s.handleGetWorkflow(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/default/wf/steps/bad/retry", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
}

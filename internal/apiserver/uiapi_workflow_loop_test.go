package apiserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestHandleGetWorkflowReportsLoopsAndPasses(t *testing.T) {
	const ns, name, gate = "default", "wf", "check"
	phase := agentorcav1alpha1.WorkflowStepPhaseSucceeded
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentorcav1alpha1.AgentWorkflowSpec{Steps: []agentorcav1alpha1.WorkflowStep{
			{Name: "plan", AgentRef: "a", Input: "x"},
			{Name: "build", AgentRef: "a", Input: "x", DependsOn: []string{"plan"}},
			{Name: gate, AgentRef: "a", Input: "x", DependsOn: []string{"build"},
				Loop: &agentorcav1alpha1.WorkflowLoop{From: "build", Until: `steps["check"].output.contains("PASS")`, MaxIterations: 4}},
		}},
		Status: agentorcav1alpha1.AgentWorkflowStatus{Steps: []agentorcav1alpha1.WorkflowStepStatus{
			{Name: "plan", Phase: phase},
			{Name: "build", Phase: agentorcav1alpha1.WorkflowStepPhasePending, Iteration: 1, Output: "built v1"},
			{Name: gate, Phase: agentorcav1alpha1.WorkflowStepPhasePending, Iteration: 1, Output: "FAIL"},
		}},
	}
	run := func(n, step, out string) *agentorcav1alpha1.AgentRun {
		r := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns,
			Labels: map[string]string{"agentorca.io/workflow": name, "agentorca.io/workflow-step": step}}}
		r.Status.Phase, r.Status.Output = agentorcav1alpha1.AgentRunPhaseSucceeded, out
		return r
	}
	c := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(wf,
		run("wf-build", "build", "built v1"), run("wf-check", "check", "FAIL"), run("wf-plan", "plan", "p")).Build()
	s := &UIServer{crdClient: c}

	rec := httptest.NewRecorder()
	s.handleGetWorkflow(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/default/wf", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Loops []struct {
			Gate  string   `json:"gate"`
			Steps []string `json:"steps"`
			Pass  int32    `json:"pass"`
			Max   int32    `json:"maxIterations"`
			State string   `json:"state"`
		} `json:"loops"`
		Steps []struct {
			Name        string `json:"name"`
			Loop        string `json:"loop"`
			Iteration   int32  `json:"iteration"`
			StaleOutput bool   `json:"staleOutput"`
			Passes      []struct {
				Pass   int32  `json:"pass"`
				Output string `json:"output"`
			} `json:"passes"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Loops) != 1 || got.Loops[0].Gate != gate || len(got.Loops[0].Steps) != 2 ||
		got.Loops[0].Pass != 2 || got.Loops[0].Max != 4 || got.Loops[0].State != "repeating" {
		t.Fatalf("loops = %+v", got.Loops)
	}
	for _, st := range got.Steps {
		switch st.Name {
		case "plan":
			if st.Loop != "" || len(st.Passes) != 0 {
				t.Errorf("plan is outside the loop: %+v", st)
			}
		case "build", "check":
			if st.Loop != gate || !st.StaleOutput || st.Iteration != 1 || len(st.Passes) != 1 || st.Passes[0].Pass != 0 {
				t.Errorf("%s = %+v", st.Name, st)
			}
		}
	}
}

func TestHandleGetWorkflowListsEveryStepBeforeTheWorkflowReachesThem(t *testing.T) {
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentWorkflowSpec{Steps: []agentorcav1alpha1.WorkflowStep{
			{Name: "plan", AgentRef: "a", Input: "x"},
			{Name: "build", AgentRef: "a", Input: "x", DependsOn: []string{"plan"}},
		}},
		// The sandbox is still provisioning, so no step has a status yet.
		Status: agentorcav1alpha1.AgentWorkflowStatus{
			Phase:   agentorcav1alpha1.AgentWorkflowPhaseRunning,
			Sandbox: &agentorcav1alpha1.WorkflowSandboxStatus{Phase: agentorcav1alpha1.WorkflowSandboxPhaseProvisioning},
		},
	}
	s := &UIServer{crdClient: fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(wf).Build()}
	rec := httptest.NewRecorder()
	s.handleGetWorkflow(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/default/wf", nil))
	var got struct {
		SandboxPhase string `json:"sandboxPhase"`
		Steps        []struct {
			Name  string `json:"name"`
			Phase string `json:"phase"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 2 || got.Steps[0].Name != "plan" || got.Steps[1].Phase != "Pending" || got.SandboxPhase != "Provisioning" {
		t.Fatalf("got %+v", got)
	}
}

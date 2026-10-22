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

package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// childRunServer fakes the operator internal API: it accepts the child run and
// then reports it in the given terminal status.
func childRunServer(t *testing.T, status agentorcav1alpha1.AgentRunStatus) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(agentorcav1alpha1.AgentRun{
				ObjectMeta: metav1.ObjectMeta{Name: strings.TrimPrefix(r.URL.Path, "/agentrun/default/")},
				Status:     status,
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func childTestExecutor(t *testing.T, operatorURL string) *Executor {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Executor{namespace: "default", runName: "parent-run", operatorAPIURL: operatorURL, saTokenFile: tokenFile}
}

// Regression for #76: a failed child still cost money, and its spend has to
// reach the parent's total. The tool call reports the error, but the spend
// rides along with it.
func TestExecuteAgentTool_FailedChildStillReportsSpend(t *testing.T) {
	srv := childRunServer(t, agentorcav1alpha1.AgentRunStatus{
		Phase: agentorcav1alpha1.AgentRunPhaseFailed, SpendUSD: "0.420000", FailureReason: "boom",
	})
	e := childTestExecutor(t, srv.URL)

	out, spend, err := e.executeAgentTool(context.Background(), ToolExecuteRequest{Tool: "child-agent", Arguments: `{"task":"x"}`})

	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the child's failure reason as the error, got %v", err)
	}
	if out != "" {
		t.Fatalf("output = %q, want empty for a failed child", out)
	}
	if spend != "0.420000" {
		t.Fatalf("child spend = %q, want 0.420000 even though the child failed", spend)
	}
}

// Through Dispatch the failure is reported in the response, not as a Go error,
// so the router's spend aggregation still runs for failed children.
func TestDispatch_FailedChildSpendInResponse(t *testing.T) {
	srv := childRunServer(t, agentorcav1alpha1.AgentRunStatus{
		Phase: agentorcav1alpha1.AgentRunPhaseFailed, SpendUSD: "1.000000", FailureReason: "crashed",
	})
	e := childTestExecutor(t, srv.URL)

	resp, err := e.Dispatch(context.Background(), ToolExecuteRequest{BackendType: "agent", Tool: "child-agent", Arguments: `{"task":"x"}`})

	if err != nil {
		t.Fatalf("Dispatch returned a Go error: %v", err)
	}
	if resp.Error == "" {
		t.Fatal("expected the child failure in resp.Error")
	}
	if resp.ChildSpendUSD != "1.000000" {
		t.Fatalf("ChildSpendUSD = %q, want 1.000000", resp.ChildSpendUSD)
	}
}

func TestExecuteAgentTool_SucceededChildReportsSpend(t *testing.T) {
	srv := childRunServer(t, agentorcav1alpha1.AgentRunStatus{
		Phase: agentorcav1alpha1.AgentRunPhaseSucceeded, SpendUSD: "0.050000", Output: "done",
	})
	e := childTestExecutor(t, srv.URL)

	out, spend, err := e.executeAgentTool(context.Background(), ToolExecuteRequest{Tool: "child-agent", Arguments: `{"task":"x"}`})

	if err != nil || out != "done" || spend != "0.050000" {
		t.Fatalf("got out=%q spend=%q err=%v, want done/0.050000/nil", out, spend, err)
	}
}

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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const testNamespace = "tenant-acme"

func TestUIRouting(t *testing.T) {
	cl := newACPServer(t, routingFixture(routedBilling)...).crdClient
	s := &UIServer{crdClient: cl}
	s.SetDispatcher(&dispatch.Dispatcher{Client: cl, Local: wordDecider{new(int)}})
	mux := http.NewServeMux()
	s.registerRoutingRoutes(mux)
	serve := func(tenant *TenantIdentity, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if tenant != nil {
			req = asTenant(req, tenant)
		}
		mux.ServeHTTP(rec, req)
		return rec
	}
	const run = `{"input":[{"role":"user","parts":[{"content":"roll back the deploys"}]}]}`

	var view routingView
	rec := serve(routedTenant(), http.MethodGet, "/api/routing", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || !view.Auto || !view.HasDefault || view.DefaultAgent != routedBilling {
		t.Fatalf("routing: %d %s", rec.Code, rec.Body)
	}
	// Nobody signed in: nothing is offered and nothing runs.
	if rec = serve(nil, http.MethodGet, "/api/routing", ""); !strings.Contains(rec.Body.String(), `"auto":false`) {
		t.Errorf("anonymous routing: %s", rec.Body)
	}
	if rec = serve(nil, http.MethodPost, "/api/routing/run", run); rec.Code != http.StatusBadRequest {
		t.Errorf("anonymous run: %d %s", rec.Code, rec.Body)
	}
	viewer := routedTenant()
	viewer.Roles = tenancy.RoleViewer.Expand()
	if rec = serve(viewer, http.MethodPost, "/api/routing/run", run); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer started a run: %d", rec.Code)
	}
	// A namespace with none of the caller's agents offers nothing, the default
	// agent included.
	if rec = serve(routedTenant(), http.MethodPost, "/api/routing/run?namespace=elsewhere", run); rec.Code != http.StatusNotFound {
		t.Errorf("run limited elsewhere: %d %s", rec.Code, rec.Body)
	}

	rec = serve(routedTenant(), http.MethodPost, "/api/routing/run?namespace="+testNamespace, run)
	var started ACPRun
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || rec.Code != http.StatusCreated ||
		started.AgentName != routedDeploys || started.Namespace != testNamespace || started.Routing == nil || started.Routing.Method != decided {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	var created agentorcav1alpha1.AgentRun
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: started.RunID}, &created); err != nil {
		t.Fatal(err)
	}
	if routingOf(&created) == nil {
		t.Errorf("annotations = %v", created.Annotations)
	}
}

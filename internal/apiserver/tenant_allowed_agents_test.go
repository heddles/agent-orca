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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// A tenant's allowedAgents means the same on every API: with no names, every
// agent in the tenant's namespaces is allowed; with names, only those.
func TestTenantAllowedAgentsAcrossAPIs(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{}
	agent.Name, agent.Namespace = "bot-a", testTenant().Namespace

	cases := []struct {
		name    string
		allowed []string
		denied  bool
	}{
		{"omitted allows all", nil, false},
		{"empty allows all", []string{}, false},
		{"listed agent is allowed", []string{"bot-a"}, false},
		{"unlisted agent is denied", []string{"bot-b"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant := testTenant()
			tenant.AllowedAgents = tc.allowed

			t.Run("external task API", func(t *testing.T) {
				// No such agent exists, so a request that passes the allow list ends
				// in 404 before any run is created.
				s := newAdminServer(t)
				req := httptest.NewRequest(http.MethodPost, "/v1/tasks",
					strings.NewReader(`{"agent":"bot-a","input":"x"}`))
				req = req.WithContext(context.WithValue(req.Context(), tenantIdentityKey, tenant))
				rec := httptest.NewRecorder()
				s.handleTasks(rec, req)
				want := http.StatusNotFound
				if tc.denied {
					want = http.StatusForbidden
				}
				if rec.Code != want {
					t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body.String())
				}
			})

			t.Run("ACP agent list", func(t *testing.T) {
				rec := httptest.NewRecorder()
				newACPServer(t, agent.DeepCopy()).withTenant(tenant).
					ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
				}
				if listed := strings.Contains(rec.Body.String(), "bot-a"); listed == tc.denied {
					t.Fatalf("listed = %v with allowedAgents %v: %s", listed, tc.allowed, rec.Body.String())
				}
			})

			t.Run("ACP run", func(t *testing.T) {
				rec := httptest.NewRecorder()
				newACPServer(t).withTenant(tenant).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs",
					strings.NewReader(`{"agent_name":"bot-a","input":[{"role":"user","parts":[{"content_type":"text/plain","content":"x"}]}]}`)))
				if forbidden := rec.Code == http.StatusForbidden; forbidden != tc.denied {
					t.Fatalf("status = %d with allowedAgents %v: %s", rec.Code, tc.allowed, rec.Body.String())
				}
			})
		})
	}
}

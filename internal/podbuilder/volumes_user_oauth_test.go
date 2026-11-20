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

package podbuilder

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// No user credential is ever mounted into a pod: a per-user (maintainer) MCP server
// gets no Secret volume at all, while a shared OAuth server still gets its credentials.
func TestResolveToolSecretVolumes_PerUserOAuthMountsNothing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, oauth *agentorcav1alpha1.MCPOAuthConfig) *agentorcav1alpha1.Tool {
		return &agentorcav1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: agentorcav1alpha1.ToolSpec{
				Type: agentorcav1alpha1.ToolTypeMCP,
				MCPConfig: &agentorcav1alpha1.MCPConfig{
					Transport: "http", URL: "https://mcp.example/mcp",
					Auth: &agentorcav1alpha1.MCPAuthConfig{OAuth: oauth},
				},
			},
		}
	}
	perUser := mk("per-user", &agentorcav1alpha1.MCPOAuthConfig{ConnectionMode: "maintainer", AppRef: "gh"})
	shared := mk("shared", &agentorcav1alpha1.MCPOAuthConfig{Credentials: &agentorcav1alpha1.LocalObjectRef{Name: "slack-creds"}})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(perUser, shared).Build()

	resolve := func(tools ...string) ([]string, []string) {
		agent := &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}, Spec: agentorcav1alpha1.AgentSpec{Tools: tools}}
		vols, mounts, err := ResolveToolSecretVolumes(context.Background(), cl, "default", agent)
		if err != nil {
			t.Fatal(err)
		}
		v := make([]string, 0, len(vols))
		m := make([]string, 0, len(mounts))
		for _, x := range vols {
			if x.Secret != nil {
				v = append(v, x.Secret.SecretName)
			}
		}
		for _, x := range mounts {
			m = append(m, x.MountPath)
		}
		return v, m
	}
	vols, mounts := resolve("per-user")
	if len(vols) != 0 || len(mounts) != 0 {
		t.Errorf("a per-user server mounted %v at %v", vols, mounts)
	}
	vols, mounts = resolve("shared")
	if len(vols) != 1 || vols[0] != "slack-creds" || len(mounts) != 1 || !strings.Contains(mounts[0], "shared") {
		t.Errorf("a shared server must still mount its credentials: %v at %v", vols, mounts)
	}
}

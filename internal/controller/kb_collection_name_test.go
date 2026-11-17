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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func TestResolveCollectionName(t *testing.T) {
	kb := func(tenant, spec, status string) *agentorcav1alpha1.KnowledgeBase {
		k := &agentorcav1alpha1.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Name: "docs"}}
		if tenant != "" {
			k.Labels = map[string]string{tenancy.LabelTenantID: tenant}
		}
		k.Spec.VectorStore.CollectionName = spec
		k.Status.CollectionName = status
		return k
	}
	cases := []struct {
		name string
		kb   *agentorcav1alpha1.KnowledgeBase
		want string
	}{
		{"new KB gets a tenant-qualified name", kb("tnt-a", "", ""), "tnt-a-docs"},
		{"two tenants never share a default name", kb("tnt-b", "", ""), "tnt-b-docs"},
		{"existing KB keeps its recorded collection", kb("tnt-a", "", "docs"), "docs"},
		{"explicit spec name wins", kb("tnt-a", "custom", "docs"), "custom"},
		{"unlabelled KB falls back to the bare name", kb("", "", ""), "docs"},
	}
	for _, c := range cases {
		if got := resolveCollectionName(c.kb); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

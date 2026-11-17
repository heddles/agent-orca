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

package security

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const tenantA = "tnt_01935c7e-0000-7000-8000-00000000000a"

func tenantRun(tid string) *agentorcav1alpha1.AgentRun {
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns", Labels: map[string]string{}}}
	if tid != "" {
		run.Labels[LabelTenantID] = tid
	}
	return run
}

// peerRules returns the egress rules that select pods rather than ports.
func peerRules(np *networkingv1.NetworkPolicy) []networkingv1.NetworkPolicyEgressRule {
	var out []networkingv1.NetworkPolicyEgressRule
	for _, r := range np.Spec.Egress {
		if len(r.To) > 0 && r.To[0].PodSelector != nil && r.To[0].PodSelector.MatchLabels[LabelTenantID] != "" {
			out = append(out, r)
		}
	}
	return out
}

func TestRunPoliciesSelectTenantWorkloadsAndPlatformOnly(t *testing.T) {
	for name, np := range map[string]*networkingv1.NetworkPolicy{
		"combined": BuildNetworkPolicy(tenantRun(tenantA), "ns", nil, false, "", "op"),
		"router":   BuildRouterPodNetworkPolicy(tenantRun(tenantA), "ns", nil, false, "", "op"),
	} {
		rules := peerRules(np)
		if len(rules) != 1 {
			t.Fatalf("%s: %d tenant peer rules, want 1", name, len(rules))
		}
		to := rules[0].To
		if len(to) != 2 {
			t.Fatalf("%s: peers = %+v", name, to)
		}
		own := to[0].PodSelector
		if own.MatchLabels[LabelTenantID] != tenantA {
			t.Errorf("%s: own-tenant peer selects %v", name, own.MatchLabels)
		}
		if len(own.MatchExpressions) != 1 || own.MatchExpressions[0].Key != LabelComponent {
			t.Errorf("%s: own-tenant peer must be limited to workload components: %+v", name, own.MatchExpressions)
		}
		if to[1].PodSelector.MatchLabels[LabelScope] != ScopePlatform {
			t.Errorf("%s: second peer is not platform: %+v", name, to[1])
		}
		if np.Labels[LabelTenantID] != tenantA {
			t.Errorf("%s: policy not labelled with the tenant: %v", name, np.Labels)
		}
		// No peer selects another tenant: every tenant peer names tenantA only.
		for _, p := range to {
			if v := p.PodSelector.MatchLabels[LabelTenantID]; v != "" && v != tenantA {
				t.Errorf("%s: peer names tenant %q", name, v)
			}
		}
	}
}

func TestSplitPodPeersCarryTheTenant(t *testing.T) {
	router := BuildRouterPodNetworkPolicy(tenantRun(tenantA), "ns", nil, false, "", "op")
	from := router.Spec.Ingress[0].From[0].PodSelector.MatchLabels
	if from[LabelTenantID] != tenantA || from[LabelComponent] != LabelComponentAgent {
		t.Errorf("router ingress peer = %v", from)
	}
	agent := BuildAgentPodNetworkPolicy(tenantRun(tenantA), "ns")
	to := agent.Spec.Egress[0].To[0].PodSelector.MatchLabels
	if to[LabelTenantID] != tenantA || to[LabelComponent] != LabelComponentRouter {
		t.Errorf("agent egress peer = %v", to)
	}
	// The agent pod stays limited to its router and DNS: no workload peers.
	if len(agent.Spec.Egress) != 3 {
		t.Errorf("agent pod egress = %+v, want router and DNS only", agent.Spec.Egress)
	}
}

// A run with no tenant ID gets the old policy: no tenant selectors, so nothing
// breaks while labels are being stamped.
func TestUnlabelledRunKeepsThePortBasedPolicy(t *testing.T) {
	np := BuildNetworkPolicy(tenantRun(""), "ns", nil, false, "", "op")
	if len(peerRules(np)) != 0 {
		t.Errorf("unlabelled run got tenant peer rules: %+v", np.Spec.Egress)
	}
	if _, ok := np.Labels[LabelTenantID]; ok {
		t.Error("unlabelled run's policy carries a tenant label")
	}
}

func TestTenantWorkloadIngressPolicyAdmitsOnlyOwnTenantAndPlatform(t *testing.T) {
	np := BuildTenantWorkloadIngressPolicy(tenantA, "ns", "agent-orca-system")
	if np.Spec.PodSelector.MatchLabels[LabelTenantID] != tenantA {
		t.Errorf("selects %v", np.Spec.PodSelector.MatchLabels)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policy types = %v", np.Spec.PolicyTypes)
	}
	from := np.Spec.Ingress[0].From
	if len(from) != 3 {
		t.Fatalf("from = %+v", from)
	}
	if from[0].PodSelector.MatchLabels[LabelTenantID] != tenantA {
		t.Errorf("first peer = %+v", from[0])
	}
	if from[1].PodSelector.MatchLabels[LabelScope] != ScopePlatform {
		t.Errorf("second peer = %+v", from[1])
	}
	if from[2].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "agent-orca-system" {
		t.Errorf("third peer = %+v", from[2])
	}
}

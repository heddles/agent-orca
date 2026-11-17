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
	"k8s.io/apimachinery/pkg/labels"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const tenantB = "tnt_01935c7e-0000-7000-8000-00000000000b"

// dest is an in-cluster pod a run might dial.
type dest struct {
	ns        string
	podLabels map[string]string
	port      int32
}

func selects(t *testing.T, sel *metav1.LabelSelector, l map[string]string) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		t.Fatal(err)
	}
	return s.Matches(labels.Set(l))
}

// admits evaluates the policy's egress rules for a connection to d, the way a
// CNI does: a rule admits it if its port matches and it has no peers, or a peer
// selects the pod.
func admits(t *testing.T, np *networkingv1.NetworkPolicy, d dest) bool {
	t.Helper()
	nsLabels := map[string]string{"kubernetes.io/metadata.name": d.ns}
	for _, r := range np.Spec.Egress {
		portOK := len(r.Ports) == 0
		for _, p := range r.Ports {
			if p.Port != nil && p.Port.IntVal == d.port {
				portOK = true
			}
		}
		if !portOK {
			continue
		}
		if len(r.To) == 0 {
			return true
		}
		for _, peer := range r.To {
			if peer.IPBlock != nil {
				continue
			}
			if peer.NamespaceSelector != nil && !selects(t, peer.NamespaceSelector, nsLabels) {
				continue
			}
			if peer.PodSelector != nil && !selects(t, peer.PodSelector, d.podLabels) {
				continue
			}
			return true
		}
	}
	return false
}

func toolPod(tenant string) map[string]string {
	return map[string]string{LabelTenantID: tenant, LabelComponent: LabelComponentTool}
}

func TestRunPolicyDoesNotAdmitAnotherTenantsToolPod(t *testing.T) {
	tools := []agentorcav1alpha1.EgressRule{{Host: "search-tool.tenants.svc.cluster.local", Port: 9000, Protocol: "TCP"}}
	for name, np := range map[string]*networkingv1.NetworkPolicy{
		"combined": BuildNetworkPolicy(tenantRun(tenantA), "ns", tools, true, "", "agent-orca-system"),
		"router":   BuildRouterPodNetworkPolicy(tenantRun(tenantA), "ns", tools, true, "", "agent-orca-system"),
	} {
		if !admits(t, np, dest{"tenants", toolPod(tenantA), 9000}) {
			t.Errorf("%s: own tenant's tool pod is not reachable", name)
		}
		if admits(t, np, dest{"tenants", toolPod(tenantB), 9000}) {
			t.Errorf("%s: another tenant's tool pod is reachable on the tool port", name)
		}
		if admits(t, np, dest{"tenants", map[string]string{"app": "anything"}, 9000}) {
			t.Errorf("%s: an unlabelled pod is reachable on the tool port", name)
		}
		// No rule opens the tool port to every destination.
		for _, r := range np.Spec.Egress {
			for _, p := range r.Ports {
				if p.Port != nil && p.Port.IntVal == 9000 && len(r.To) == 0 {
					t.Errorf("%s: port-only rule for the in-cluster tool port: %+v", name, r)
				}
			}
		}
	}
}

func TestRunPolicyAdmitsPlatformToolPods(t *testing.T) {
	tools := []agentorcav1alpha1.EgressRule{{Host: "shared-mcp", Port: 9000}}
	np := BuildNetworkPolicy(tenantRun(tenantA), "ns", tools, false, "", "agent-orca-system")
	platform := map[string]string{LabelScope: ScopePlatform}
	if !admits(t, np, dest{"shared", platform, 9000}) {
		t.Error("a platform pod is not reachable on the tool port")
	}
}

// External hosts and 443 cannot be selected by a pod or namespace selector and
// stay port-only (documented in network_policy_peers.go).
func TestExternalToolEndpointsStayPortOnly(t *testing.T) {
	tools := []agentorcav1alpha1.EgressRule{{Host: "api.example.com", Port: 8443}}
	np := BuildNetworkPolicy(tenantRun(tenantA), "ns", tools, false, "", "agent-orca-system")
	if !admits(t, np, dest{"anywhere", map[string]string{"x": "y"}, 8443}) {
		t.Error("an external tool endpoint rule was narrowed to selectors")
	}
	if !admits(t, np, dest{"anywhere", map[string]string{"x": "y"}, 443}) {
		t.Error("the 443 rule must stay open")
	}
}

func TestRedisAndOperatorPortsSelectTheirPods(t *testing.T) {
	const op = "agent-orca-system"
	redis := map[string]string{"app.kubernetes.io/name": "agent-orca", "app.kubernetes.io/component": "redis"}
	operator := map[string]string{"app.kubernetes.io/name": "agent-orca", "app.kubernetes.io/component": "operator"}
	for name, np := range map[string]*networkingv1.NetworkPolicy{
		"combined": BuildNetworkPolicy(tenantRun(tenantA), "ns", nil, true, "", op),
		"router":   BuildRouterPodNetworkPolicy(tenantRun(tenantA), "ns", nil, true, "", op),
	} {
		if !admits(t, np, dest{op, redis, 6379}) {
			t.Errorf("%s: the chart's Redis is not reachable", name)
		}
		if admits(t, np, dest{"tenants", toolPod(tenantB), 6379}) {
			t.Errorf("%s: another tenant's pod is reachable on the Redis port", name)
		}
		if admits(t, np, dest{op, operator, 6379}) {
			t.Errorf("%s: a non-Redis pod is reachable on the Redis port", name)
		}
		if !admits(t, np, dest{op, operator, 8082}) {
			t.Errorf("%s: the operator API is not reachable", name)
		}
		if admits(t, np, dest{"tenants", toolPod(tenantB), 8082}) {
			t.Errorf("%s: another tenant's pod is reachable on the operator API port", name)
		}
	}
}

func TestRedisEgressFollowsTheConfiguredURL(t *testing.T) {
	const op = "agent-orca-system"
	other := map[string]string{"app": "redis"}
	// A Service in another namespace opens that namespace on the Redis port.
	np := BuildNetworkPolicy(tenantRun(tenantA), "ns", nil, true, "", op, WithRedisURL("redis://:pw@cache.data.svc.cluster.local:6380"))
	if !admits(t, np, dest{"data", other, 6380}) {
		t.Error("a Redis Service in another namespace is not reachable")
	}
	if admits(t, np, dest{"tenants", toolPod(tenantB), 6380}) {
		t.Error("another namespace is reachable on the Redis port")
	}
	// An external Redis cannot be selected and stays port-only.
	np = BuildNetworkPolicy(tenantRun(tenantA), "ns", nil, true, "", op, WithRedisURL("rediss://redis.example.com:6379"))
	if !admits(t, np, dest{"anywhere", other, 6379}) {
		t.Error("an external Redis rule was narrowed to selectors")
	}
}

func TestUnknownOperatorNamespaceKeepsPortOnlyRules(t *testing.T) {
	np := BuildNetworkPolicy(tenantRun(tenantA), "ns", nil, true, "", "")
	if !admits(t, np, dest{"x", map[string]string{"a": "b"}, 8082}) || !admits(t, np, dest{"x", map[string]string{"a": "b"}, 6379}) {
		t.Error("without an operator namespace the rules cannot be selectors")
	}
}

func TestHindsightInClusterServiceIsPinnedToItsNamespace(t *testing.T) {
	np := BuildNetworkPolicy(tenantRun(tenantA), "ns", nil, false, "http://hindsight-api.hindsight.svc.cluster.local:8888", "op")
	if !admits(t, np, dest{"hindsight", map[string]string{"a": "b"}, 8888}) {
		t.Error("hindsight is not reachable")
	}
	if admits(t, np, dest{"tenants", toolPod(tenantB), 8888}) {
		t.Error("another namespace is reachable on the hindsight port")
	}
}

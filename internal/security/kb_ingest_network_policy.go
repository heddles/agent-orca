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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// LabelKBIngest marks pods belonging to KnowledgeBase ingestion Jobs. The
// KnowledgeBase controller sets it on Job pods and selects it in the
// ingestion NetworkPolicy.
const LabelKBIngest = "agentorca.io/kb-mcp-ingest"

// BuildIngestNetworkPolicy constructs a NetworkPolicy for KnowledgeBase
// ingestion Job pods. It is additive — NetworkPolicy rules from multiple
// policies are unioned — so creating it is safe even in namespaces without
// default-deny policies, and required in namespaces that have them.
//
// Allowed egress:
//   - DNS (UDP/TCP 53)
//   - Kubernetes API (443/6443) — the ingester reads source ConfigMaps
//   - Same-namespace pods (Qdrant, MCP servers, in-cluster embedding providers)
//   - HTTP/HTTPS (80/443) — URL sources, external embedding APIs, S3
//   - Any extra egress rules from KnowledgeBase.spec.ingestion.networkEgress
//
// All ingress is denied; ingestion Jobs never accept inbound connections.
func BuildIngestNetworkPolicy(
	kbName, namespace string,
	extraEgress []agentorcav1alpha1.EgressRule,
) *networkingv1.NetworkPolicy {
	safeName := SafeLabelValue(kbName)

	// Preallocate: 443+6443, port 80, same-namespace catch-all, DNS UDP+TCP,
	// then the KnowledgeBase's extra egress rules.
	egressRules := make([]networkingv1.NetworkPolicyEgressRule, 0, 4+len(extraEgress)+2)

	// Kubernetes API server (ConfigMap reads) and external HTTPS.
	egressRules = append(egressRules,
		egressToPort(443),
		egressToPort(6443),
	)

	// Plain HTTP for URL sources served over http://.
	egressRules = append(egressRules, egressToPort(80))

	// Same-namespace pods: Qdrant, MCP servers, in-cluster embedding
	// providers. An empty podSelector matches every pod in the policy's
	// namespace, on any port.
	egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{PodSelector: &metav1.LabelSelector{}},
		},
	})

	// Extra rules from the KnowledgeBase spec (ports only — NetworkPolicy
	// cannot filter by hostname, mirroring the AgentRun egress convention).
	for _, rule := range extraEgress {
		port := intstr.FromInt32(rule.Port)
		proto := corev1Protocol(rule.Protocol)
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Port: &port, Protocol: &proto},
			},
		})
	}

	// DNS egress (UDP/TCP port 53) — required for hostname resolution.
	egressRules = append(egressRules,
		egressToPortProto(53, "UDP"),
		egressToPortProto(53, "TCP"),
	)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      IngestNetworkPolicyName(kbName),
			Namespace: namespace,
			Labels: map[string]string{
				LabelKBIngest:  safeName,
				LabelManagedBy: ManagedByValue,
			},
			// Owner reference is set by the controller.
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelKBIngest: safeName,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Egress: egressRules,
		},
	}
}

// IngestNetworkPolicyName returns the NetworkPolicy name for a KnowledgeBase's
// ingestion Jobs.
func IngestNetworkPolicyName(kbName string) string {
	return "agentorca-kb-ingest-" + SafeLabelValue(kbName)
}

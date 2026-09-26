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
	"crypto/sha256"
	"encoding/hex"

	"net"
	"net/url"

	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const (
	// LabelAgentRunName is the pod label used to associate a pod with its AgentRun.
	LabelAgentRunName = "agentorca.io/run"
	// LabelManagedBy is the standard managed-by label.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the value for LabelManagedBy.
	ManagedByValue = "agent-orca"

	// LabelComponent identifies the role of a pod within a run (agent or router).
	// Only set when split-pod topology is enabled (Agent.spec.networkIsolation.splitPod=true).
	LabelComponent = "agentorca.io/component"
	// LabelComponentAgent is the LabelComponent value for the agent pod in split-pod topology.
	LabelComponentAgent = "agent"
	// LabelComponentRouter is the LabelComponent value for the router pod in split-pod topology.
	LabelComponentRouter = "router"

	// maxLabelValueLen is the maximum length of a Kubernetes label value.
	maxLabelValueLen = 63
)

// SafeLabelValue returns s unchanged if it fits within the 63-byte Kubernetes
// label value limit. Otherwise it truncates and appends a short hash suffix to
// keep the value unique and within bounds.
func SafeLabelValue(s string) string {
	if len(s) <= maxLabelValueLen {
		return s
	}
	h := sha256.Sum256([]byte(s))
	suffix := hex.EncodeToString(h[:4]) // 8 hex chars
	// Truncate to leave room for "-" + 8-char hash = 9 chars.
	return s[:maxLabelValueLen-9] + "-" + suffix
}

// BuildNetworkPolicy constructs a per-run NetworkPolicy that:
//   - Denies all ingress by default
//   - Allows egress to the Kubernetes API server (for SA TokenReview)
//   - Allows egress to all model provider endpoints declared in the selector
//   - Allows egress to tool-specific endpoints declared in the Tool specs
//   - Allows egress to Redis on port 6379 when stateRedisEnabled is true
//   - Allows egress to the operator's internal API on port 8082 (always — required for built-in router ops)
//   - Denies all other egress
func BuildNetworkPolicy(
	run *agentorcav1alpha1.AgentRun,
	namespace string,
	toolEgressRules []agentorcav1alpha1.EgressRule,
	stateRedisEnabled bool,
	hindsightURL string,
) *networkingv1.NetworkPolicy {
	name := networkPolicyName(run.Name)

	// Build egress rules.
	var egressRules []networkingv1.NetworkPolicyEgressRule

	// Allow egress to the Kubernetes API server on port 443 and 6443.
	// Required for the model-router sidecar to perform TokenReview calls.
	egressRules = append(egressRules,
		egressToPort(443),
		egressToPort(6443),
	)

	// Allow egress to tool-specific endpoints.
	for _, rule := range toolEgressRules {
		port := intstr.FromInt32(rule.Port)
		proto := corev1Protocol(rule.Protocol)
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Port: &port, Protocol: &proto},
			},
		})
	}

	// Allow egress to Redis on port 6379 for token streaming and state checkpointing.
	if stateRedisEnabled {
		egressRules = append(egressRules, egressToPort(6379))
	}

	// Always allow egress to the operator's internal API on port 8082.
	// The model-router sidecar requires this for built-in operations on every run:
	// _clarify (WaitingForInput), loop-detected, handoff, RAG search/ingest.
	egressRules = append(egressRules, egressToPort(8082))

	// Allow egress to hindsight memory service (self-hosted POC).
	// The model-router calls hindsight directly for retain/recall on every LLM turn.
	// Port is derived from the configured URL (scheme default or explicit port).
	hindsightPort := hindsightPortFromURL(hindsightURL, 8888)
	egressRules = append(egressRules, egressToPort(hindsightPort))

	// If a hindsight URL is configured, resolve its hostname to IPs and add
	// ipBlock egress rules so agent-orca pods can reach hindsight even when
	// the cluster has a default-deny egress policy.
	for _, cidr := range resolveHindsightCIDRs(hindsightURL) {
		egressRules = append(egressRules, egressToCIDR(cidr))
	}

	// DNS egress (UDP/TCP port 53) — required for hostname resolution.
	egressRules = append(egressRules,
		egressToPortProto(53, "UDP"),
		egressToPortProto(53, "TCP"),
	)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelAgentRunName: SafeLabelValue(run.Name),
				LabelManagedBy:    ManagedByValue,
			},
			// Owner reference is set by the controller.
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelAgentRunName: SafeLabelValue(run.Name),
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Ingress open only to the model-router metrics port (9091), so an
			// in-cluster Prometheus/Datadog (or the operator itself; see
			// internal/apiserver/uiapi.go scrapeModelRouterTokenRate) can scrape
			// token throughput. Every other ingress is denied.
			Ingress: []networkingv1.NetworkPolicyIngressRule{metricsIngressFromScraper()},
			Egress:  egressRules,
		},
	}
}

// BuildRouterPodNetworkPolicy constructs the NetworkPolicy for the router pod in split-pod
// topology. It applies the same egress rules as BuildNetworkPolicy (provider HTTPS, K8s API,
// Redis, tools, DNS) and adds ingress from the agent pod on ports 8080/8082, plus the model-router
// metrics port (9091) from namespaces labeled metrics: enabled (Prometheus/Datadog).
//
// The PodSelector targets pods labeled agentorca.io/component=router for this run, so the
// router and agent pods receive independent egress allowances.
func BuildRouterPodNetworkPolicy(
	run *agentorcav1alpha1.AgentRun,
	namespace string,
	toolEgressRules []agentorcav1alpha1.EgressRule,
	stateRedisEnabled bool,
	hindsightURL string,
) *networkingv1.NetworkPolicy {
	name := "agentorca-router-" + run.Name
	safeRunName := SafeLabelValue(run.Name)

	// Egress rules identical to combined-pod policy.
	var egressRules []networkingv1.NetworkPolicyEgressRule
	egressRules = append(egressRules, egressToPort(443), egressToPort(6443))
	for _, rule := range toolEgressRules {
		port := intstr.FromInt32(rule.Port)
		proto := corev1Protocol(rule.Protocol)
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Port: &port, Protocol: &proto}},
		})
	}
	if stateRedisEnabled {
		egressRules = append(egressRules, egressToPort(6379))
	}
	egressRules = append(egressRules, egressToPort(8082))
	egressRules = append(egressRules, egressToPortProto(53, "UDP"), egressToPortProto(53, "TCP"))

	// Allow egress to hindsight memory service (self-hosted POC).
	// The model-router calls hindsight directly for retain/recall on every LLM turn.
	// Port is derived from the configured URL (scheme default or explicit port).
	hindsightPort := hindsightPortFromURL(hindsightURL, 8888)
	egressRules = append(egressRules, egressToPort(hindsightPort))

	// If a hindsight URL is configured, resolve its hostname to IPs and add
	// ipBlock egress rules so agent-orca pods can reach hindsight even when
	// the cluster has a default-deny egress policy.
	for _, cidr := range resolveHindsightCIDRs(hindsightURL) {
		egressRules = append(egressRules, egressToCIDR(cidr))
	}

	// Ingress from the agent pod (same run, component=agent) on router ports.
	port8080 := intstr.FromInt32(8080)
	port8082 := intstr.FromInt32(8082)
	protoTCP := corev1Protocol("TCP")
	agentPeerSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			LabelAgentRunName: safeRunName,
			LabelComponent:    LabelComponentAgent,
		},
	}
	ingressFromAgent := networkingv1.NetworkPolicyIngressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Port: &port8080, Protocol: &protoTCP},
			{Port: &port8082, Protocol: &protoTCP},
		},
		From: []networkingv1.NetworkPolicyPeer{
			{PodSelector: agentPeerSelector},
		},
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelAgentRunName: safeRunName,
				LabelManagedBy:    ManagedByValue,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelAgentRunName: safeRunName,
					LabelComponent:    LabelComponentRouter,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{ingressFromAgent, metricsIngressFromScraper()},
			Egress:  egressRules,
		},
	}
}

// BuildAgentPodNetworkPolicy constructs the NetworkPolicy for the agent pod in split-pod
// topology. The agent pod's egress is restricted to the router pod (ports 8080/8082) and
// DNS — it cannot open arbitrary outbound connections to the internet.
//
// The PodSelector targets pods labeled agentorca.io/component=agent for this run.
func BuildAgentPodNetworkPolicy(
	run *agentorcav1alpha1.AgentRun,
	namespace string,
) *networkingv1.NetworkPolicy {
	name := "agentorca-agent-" + run.Name
	safeRunName := SafeLabelValue(run.Name)

	// Egress to the router pod (same run, component=router) on ports 8080/8082.
	port8080 := intstr.FromInt32(8080)
	port8082 := intstr.FromInt32(8082)
	protoTCP := corev1Protocol("TCP")
	routerPeerSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			LabelAgentRunName: safeRunName,
			LabelComponent:    LabelComponentRouter,
		},
	}
	egressToRouter := networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Port: &port8080, Protocol: &protoTCP},
			{Port: &port8082, Protocol: &protoTCP},
		},
		To: []networkingv1.NetworkPolicyPeer{
			{PodSelector: routerPeerSelector},
		},
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelAgentRunName: safeRunName,
				LabelManagedBy:    ManagedByValue,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelAgentRunName: safeRunName,
					LabelComponent:    LabelComponentAgent,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Empty ingress = deny all ingress to the agent pod.
			Ingress: []networkingv1.NetworkPolicyIngressRule{},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				egressToRouter,
				// DNS egress required for resolving the router Service hostname.
				egressToPortProto(53, "UDP"),
				egressToPortProto(53, "TCP"),
			},
		},
	}
}

func networkPolicyName(runName string) string {
	return "agentorca-run-" + runName
}

func egressToPort(port int32) networkingv1.NetworkPolicyEgressRule {
	p := intstr.FromInt32(port)
	proto := corev1Protocol("TCP")
	return networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Port: &p, Protocol: &proto},
		},
	}
}

func egressToPortProto(port int32, protocol string) networkingv1.NetworkPolicyEgressRule { //nolint:unparam

	p := intstr.FromInt32(port)
	proto := corev1Protocol(protocol)
	return networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Port: &p, Protocol: &proto},
		},
	}
}

func hindsightPortFromURL(urlStr string, defaultPort int32) int32 {
	if urlStr == "" {
		return defaultPort
	}
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return defaultPort
	}
	// If there's an explicit port in the URL, use it
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err == nil {
			return int32(port)
		}
	}
	// If there's no explicit port, derive from scheme
	switch u.Scheme {
	case "https":
		return 443
	case "http":
		return 80
	}
	// Fallback to defaultPort (though this should rarely be reached)
	return defaultPort
}

func egressToCIDR(cidr string) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{IPBlock: &networkingv1.IPBlock{CIDR: cidr}},
		},
	}
}

func resolveHindsightCIDRs(urlStr string) []string {
	if urlStr == "" {
		return nil
	}
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil
	}
	cidrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip.To4() != nil {
			cidrs = append(cidrs, ip.String()+"/32")
		} else {
			cidrs = append(cidrs, ip.String()+"/128")
		}
	}
	return cidrs
}

func corev1Protocol(p string) corev1.Protocol {
	if p == "UDP" {
		return corev1.ProtocolUDP
	}
	return corev1.ProtocolTCP
}

// metricsIngressFromScraper returns an ingress rule that opens the model-router metrics
// port (9091) to namespaces labeled `metrics: enabled` (where Prometheus/Datadog typically
// run). This mirrors the operator's own metrics NetworkPolicy convention
// (charts/agent-orca/templates/operator-network-policy.yaml allows the operator's :8080
// metrics from `metrics: enabled` namespaces).
func metricsIngressFromScraper() networkingv1.NetworkPolicyIngressRule {
	port := intstr.FromInt32(9091)
	proto := corev1Protocol("TCP")
	return networkingv1.NetworkPolicyIngressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Port: &port, Protocol: &proto},
		},
		From: []networkingv1.NetworkPolicyPeer{
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"metrics": "enabled"},
			}},
		},
	}
}

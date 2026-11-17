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
	"github.com/heddles/agent-orca/internal/tenancy"
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

	// LabelTenantID selects a tenant's pods. It is stamped on every pod a
	// controller creates for a tenant, and on the tenant's MCP server, tool and
	// sandbox workloads.
	LabelTenantID = tenancy.LabelTenantID
	// LabelScope marks workloads every tenant may reach (the operator, Redis,
	// Hindsight, shared MCP servers): agentorca.io/scope=platform.
	LabelScope = "agentorca.io/scope"
	// ScopePlatform is the LabelScope value of platform services.
	ScopePlatform = "platform"
	// LabelComponentMCPServer, LabelComponentTool and LabelComponentSandbox are
	// the LabelComponent values of a tenant's reachable workloads.
	LabelComponentMCPServer = "mcp-server"
	LabelComponentTool      = "tool"
	LabelComponentSandbox   = "sandbox"

	// LabelPolicyKind marks policies that belong to a tenant, not a run, so the
	// TenantConfig reconciler can list them and remove the ones it no longer owes.
	LabelPolicyKind = "agentorca.io/policy"
	// PolicyKindTenantWorkloadIngress is the LabelPolicyKind value of
	// BuildTenantWorkloadIngressPolicy.
	PolicyKindTenantWorkloadIngress = "tenant-workload-ingress"

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
	operatorNamespace string,
	opts ...PolicyOption,
) *networkingv1.NetworkPolicy {
	o := newPolicyOptions(opts)
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
		egressRules = append(egressRules, toolEgressRule(rule, runTenantID(run)))
	}

	// Allow egress to Redis on port 6379 for token streaming and state checkpointing.
	if stateRedisEnabled {
		egressRules = append(egressRules, redisEgressRule(operatorNamespace, o.redisURL))
	}

	// Always allow egress to the operator's internal API on port 8082.
	// The model-router sidecar requires this for built-in operations on every run:
	// _clarify (WaitingForInput), loop-detected, handoff, RAG search/ingest.
	egressRules = append(egressRules, operatorAPIEgressRule(operatorNamespace))

	// Allow egress to hindsight memory service (self-hosted POC).
	// The model-router calls hindsight directly for retain/recall on every LLM turn.
	// Port is derived from the configured URL (scheme default or explicit port).
	hindsightPort := hindsightPortFromURL(hindsightURL, 8888)
	egressRules = append(egressRules, hindsightEgressRule(hindsightURL, hindsightPort))

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
	// The tenant's own MCP servers, tools and sandboxes, plus platform services.
	egressRules = append(egressRules, tenantEgressRules(runTenantID(run))...)

	return withTenantLabel(&networkingv1.NetworkPolicy{
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
			Ingress: []networkingv1.NetworkPolicyIngressRule{metricsIngressFromScraper(operatorNamespace)},
			Egress:  egressRules,
		},
	}, runTenantID(run))
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
	operatorNamespace string,
	opts ...PolicyOption,
) *networkingv1.NetworkPolicy {
	o := newPolicyOptions(opts)
	name := "agentorca-router-" + run.Name
	safeRunName := SafeLabelValue(run.Name)

	// Egress rules identical to combined-pod policy.
	var egressRules []networkingv1.NetworkPolicyEgressRule
	egressRules = append(egressRules, egressToPort(443), egressToPort(6443))
	for _, rule := range toolEgressRules {
		egressRules = append(egressRules, toolEgressRule(rule, runTenantID(run)))
	}
	if stateRedisEnabled {
		egressRules = append(egressRules, redisEgressRule(operatorNamespace, o.redisURL))
	}
	egressRules = append(egressRules, operatorAPIEgressRule(operatorNamespace))
	egressRules = append(egressRules, egressToPortProto(53, "UDP"), egressToPortProto(53, "TCP"))

	// Allow egress to hindsight memory service (self-hosted POC).
	// The model-router calls hindsight directly for retain/recall on every LLM turn.
	// Port is derived from the configured URL (scheme default or explicit port).
	hindsightPort := hindsightPortFromURL(hindsightURL, 8888)
	egressRules = append(egressRules, hindsightEgressRule(hindsightURL, hindsightPort))

	// If a hindsight URL is configured, resolve its hostname to IPs and add
	// ipBlock egress rules so agent-orca pods can reach hindsight even when
	// the cluster has a default-deny egress policy.
	for _, cidr := range resolveHindsightCIDRs(hindsightURL) {
		egressRules = append(egressRules, egressToCIDR(cidr))
	}

	egressRules = append(egressRules, tenantEgressRules(runTenantID(run))...)

	// Ingress from the agent pod (same run, component=agent) on router ports.
	port8080 := intstr.FromInt32(8080)
	port8082 := intstr.FromInt32(8082)
	protoTCP := corev1Protocol("TCP")
	agentPeerSelector := &metav1.LabelSelector{
		MatchLabels: withTenantSelector(map[string]string{
			LabelAgentRunName: safeRunName,
			LabelComponent:    LabelComponentAgent,
		}, runTenantID(run)),
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

	return withTenantLabel(&networkingv1.NetworkPolicy{
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
			Ingress: []networkingv1.NetworkPolicyIngressRule{ingressFromAgent, metricsIngressFromScraper(operatorNamespace)},
			Egress:  egressRules,
		},
	}, runTenantID(run))
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
		MatchLabels: withTenantSelector(map[string]string{
			LabelAgentRunName: safeRunName,
			LabelComponent:    LabelComponentRouter,
		}, runTenantID(run)),
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

	return withTenantLabel(&networkingv1.NetworkPolicy{
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
	}, runTenantID(run))
}

// withTenantSelector adds the tenant ID to a peer selector's labels when the run
// has one, so a peer pod of another tenant never matches.
func withTenantSelector(labels map[string]string, tenantID string) map[string]string {
	if tenantID != "" {
		labels[LabelTenantID] = tenantID
	}
	return labels
}

// tenantWorkloadComponents are the components of a tenant's workloads a run may
// reach.
var tenantWorkloadComponents = []string{LabelComponentMCPServer, LabelComponentTool, LabelComponentSandbox}

// runTenantID is the tenant ID on the run, or "" while the run is unlabelled.
func runTenantID(run *agentorcav1alpha1.AgentRun) string {
	return run.Labels[LabelTenantID]
}

// tenantPeers are the in-cluster pods a tenant's run may open connections to:
// its own tenant's MCP servers, tools and sandboxes (any namespace, since a
// tenant can own several), and platform services. Other tenants' workloads are
// not selected, so with a default-deny egress baseline they are unreachable.
func tenantPeers(tenantID string) []networkingv1.NetworkPolicyPeer {
	allNamespaces := &metav1.LabelSelector{} // every namespace
	return []networkingv1.NetworkPolicyPeer{
		{
			NamespaceSelector: allNamespaces,
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{LabelTenantID: tenantID},
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: LabelComponent, Operator: metav1.LabelSelectorOpIn, Values: tenantWorkloadComponents,
				}},
			},
		},
		{
			NamespaceSelector: allNamespaces,
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelScope: ScopePlatform}},
		},
	}
}

// tenantEgressRules is the egress rule that opens tenantPeers, or nil for a run
// with no tenant ID (it keeps only its port-based rules).
func tenantEgressRules(tenantID string) []networkingv1.NetworkPolicyEgressRule {
	if tenantID == "" {
		return nil
	}
	return []networkingv1.NetworkPolicyEgressRule{{To: tenantPeers(tenantID)}}
}

// withTenantLabel stamps the tenant ID on a policy's own labels so policies can
// be listed per tenant.
func withTenantLabel(np *networkingv1.NetworkPolicy, tenantID string) *networkingv1.NetworkPolicy {
	if tenantID != "" {
		np.Labels[LabelTenantID] = tenantID
	}
	return np
}

// BuildTenantWorkloadIngressPolicy is the other half of tenant network
// isolation: it admits connections to a tenant's MCP server, tool and sandbox
// pods only from the same tenant's pods, platform services and the operator's
// namespace. The TenantConfig reconciler creates one per namespace the tenant
// claims and removes it when the namespace is released or the tenant deleted. It selects by
// agentorca.io/tenant-id, so another tenant's agent pods (even in a shared
// namespace) cannot reach these workloads. Policies are additive, so it narrows
// nothing for pods it does not select.
func BuildTenantWorkloadIngressPolicy(tenantID, namespace, operatorNamespace string) *networkingv1.NetworkPolicy {
	from := []networkingv1.NetworkPolicyPeer{
		{
			NamespaceSelector: &metav1.LabelSelector{},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelTenantID: tenantID}},
		},
		{
			NamespaceSelector: &metav1.LabelSelector{},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelScope: ScopePlatform}},
		},
	}
	if operatorNamespace != "" {
		from = append(from, networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": operatorNamespace}}})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentorca-tenant-" + SafeLabelValue(tenantID),
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy:  ManagedByValue,
				LabelTenantID:   tenantID,
				LabelPolicyKind: PolicyKindTenantWorkloadIngress,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{LabelTenantID: tenantID},
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: LabelComponent, Operator: metav1.LabelSelectorOpIn, Values: tenantWorkloadComponents,
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: from}},
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
// run), and, when operatorNamespace is set, to the operator's own pods. The operator
// scrapes 9091 itself to show in-flight spend and token throughput in the UI
// (internal/apiserver: scrapeInFlightSpend, scrapeModelRouterTokenRate), and its
// namespace is not necessarily labeled for monitoring. This mirrors the operator's
// own metrics NetworkPolicy convention (charts/agent-orca/templates/operator-network-policy.yaml
// allows the operator's :8080 metrics from `metrics: enabled` namespaces).
func metricsIngressFromScraper(operatorNamespace string) networkingv1.NetworkPolicyIngressRule {
	port := intstr.FromInt32(9091)
	proto := corev1Protocol("TCP")
	peers := []networkingv1.NetworkPolicyPeer{
		{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"metrics": "enabled"},
		}},
	}
	if operatorNamespace != "" {
		// namespaceSelector AND podSelector in one peer: only the operator's pods,
		// not everything in its namespace.
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": operatorNamespace},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":      "agent-orca",
					"app.kubernetes.io/component": "operator",
				},
			},
		})
	}
	return networkingv1.NetworkPolicyIngressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Port: &port, Protocol: &proto},
		},
		From: peers,
	}
}

// BuildRouterSandboxIngressPolicy admits a run's sandbox supervisor to the router
// pod on ports 8080/8082. In sandbox mode the agent workload runs in the sandbox
// runtime's namespace and its outbound connections are opened by the supervisor
// pod, so the supervisor — not an agent pod — is the router's client.
//
// The policy is ingress-only and additive to BuildRouterPodNetworkPolicy.
// supervisorLabels must identify exactly this run's supervisor (the runtime's
// per-sandbox ID label), so other sandboxes in the shared namespace stay out.
func BuildRouterSandboxIngressPolicy(
	run *agentorcav1alpha1.AgentRun,
	namespace string,
	sandboxNamespace string,
	supervisorLabels map[string]string,
) *networkingv1.NetworkPolicy {
	safeRunName := SafeLabelValue(run.Name)
	port8080 := intstr.FromInt32(8080)
	port8082 := intstr.FromInt32(8082)
	protoTCP := corev1Protocol("TCP")
	return withTenantLabel(&networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentorca-router-" + run.Name + "-sandbox",
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
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{
					{Port: &port8080, Protocol: &protoTCP},
					{Port: &port8082, Protocol: &protoTCP},
				},
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": sandboxNamespace},
					},
					PodSelector: &metav1.LabelSelector{MatchLabels: supervisorLabels},
				}},
			}},
		},
	}, runTenantID(run))
}

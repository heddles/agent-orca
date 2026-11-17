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
	"net"
	"net/url"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// A NetworkPolicy port rule with no "to" matches every destination, so a run
// pod that may reach "port 6379" may reach every pod listening on 6379, whoever
// owns it. The helpers here pin each in-cluster destination to a pod or
// namespace selector instead.
//
// What stays port-only, and why: 443 and 6443 (provider APIs and the
// kube-apiserver) and tool or Hindsight endpoints that resolve outside the
// cluster. The API server and external APIs are not pods, so no pod or
// namespace selector can name them; an ipBlock would need their addresses,
// which change. Those rules are the only egress a run has to the internet and
// to the control plane. A tenant's own tool, MCP and sandbox pods are also
// closed to other tenants from the destination side, by
// BuildTenantWorkloadIngressPolicy, whatever port the source dials.
//
// A rule also stays port-only when its destination cannot be named at all:
// an unknown operator namespace (OperatorNamespace empty) or a run with no
// tenant ID yet. Those keep today's behaviour so nothing breaks while labels
// and configuration are rolled out.

const (
	operatorAPIPort  = 8082
	defaultRedisPort = 6379

	labelAppName      = "app.kubernetes.io/name"
	labelAppComponent = "app.kubernetes.io/component"
	labelNamespace    = "kubernetes.io/metadata.name"
	appNameOperator   = "agent-orca"
)

// PolicyOption tunes how a run's egress rules name in-cluster destinations.
type PolicyOption func(*policyOptions)

type policyOptions struct {
	redisURL string
}

// WithRedisURL gives the state store's URL, so the Redis egress rule selects the
// Redis the run really uses. Without it Redis is the chart's own, in the
// operator namespace.
func WithRedisURL(u string) PolicyOption {
	return func(o *policyOptions) { o.redisURL = u }
}

func newPolicyOptions(opts []PolicyOption) policyOptions {
	var o policyOptions
	for _, f := range opts {
		f(&o)
	}
	return o
}

// nsPeer selects every pod in one namespace.
func nsPeer(ns string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelNamespace: ns}},
	}
}

// nsPodPeer selects labelled pods in one namespace.
func nsPodPeer(ns string, podLabels map[string]string) networkingv1.NetworkPolicyPeer {
	p := nsPeer(ns)
	p.PodSelector = &metav1.LabelSelector{MatchLabels: podLabels}
	return p
}

func portRule(port int32, peers ...networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicyEgressRule {
	r := egressToPort(port)
	r.To = peers
	return r
}

// operatorAPIEgressRule opens the operator's internal API (8082) to the operator
// pods only.
func operatorAPIEgressRule(operatorNamespace string) networkingv1.NetworkPolicyEgressRule {
	if operatorNamespace == "" {
		return egressToPort(operatorAPIPort)
	}
	return portRule(operatorAPIPort, nsPodPeer(operatorNamespace, map[string]string{
		labelAppName:      appNameOperator,
		labelAppComponent: "operator",
	}))
}

// redisEgressRule opens Redis to the Redis pods only. With no URL, or a URL that
// names the chart's Service, that is the chart's Redis pods in the operator
// namespace. A Service in another namespace opens that namespace's port only
// (its pod labels are not known). A bare IP becomes an ipBlock. A host outside
// the cluster stays port-only.
func redisEgressRule(operatorNamespace, redisURL string) networkingv1.NetworkPolicyEgressRule {
	port, host := int32(defaultRedisPort), ""
	if redisURL != "" {
		if u, err := url.Parse(redisURL); err == nil {
			host = u.Hostname()
			if p := u.Port(); p != "" {
				if n, ok := parsePort(p); ok {
					port = n
				}
			}
		}
	}
	chartRedis := map[string]string{labelAppName: appNameOperator, labelAppComponent: "redis"}
	if host != "" {
		if ip := net.ParseIP(host); ip != nil {
			return networkingv1.NetworkPolicyEgressRule{
				Ports: egressToPort(port).Ports,
				To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: hostCIDR(ip)}}},
			}
		}
		ns, ok := serviceNamespace(host)
		switch {
		case !ok:
			return egressToPort(port) // outside the cluster: not selectable
		case ns != operatorNamespace:
			return portRule(port, nsPeer(ns))
		}
	}
	if operatorNamespace == "" {
		return egressToPort(port)
	}
	return portRule(port, nsPodPeer(operatorNamespace, chartRedis))
}

// hindsightEgressRule opens the Hindsight port. An in-cluster Service URL pins
// it to that Service's namespace; an external or unparseable host stays
// port-only (the resolved ipBlock rules sit beside it).
func hindsightEgressRule(hindsightURL string, port int32) networkingv1.NetworkPolicyEgressRule {
	if u, err := url.Parse(hindsightURL); err == nil && u.Hostname() != "" {
		if ns, ok := serviceNamespace(u.Hostname()); ok {
			return portRule(port, nsPeer(ns))
		}
	}
	return egressToPort(port)
}

// toolEgressRule opens one tool endpoint. A cluster-internal host (a Service
// name) can only be one of the tenant's own tool or MCP pods, or a platform
// pod, so the rule selects them and never another tenant's. A host outside the
// cluster, or a run with no tenant ID, keeps the port-only rule.
func toolEgressRule(rule agentorcav1alpha1.EgressRule, tenantID string) networkingv1.NetworkPolicyEgressRule {
	port := intstr.FromInt32(rule.Port)
	proto := corev1Protocol(rule.Protocol)
	r := networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{{Port: &port, Protocol: &proto}}}
	if tenantID != "" && isClusterInternalHost(rule.Host) {
		r.To = tenantPeers(tenantID)
	}
	return r
}

// isClusterInternalHost reports whether host names something inside the
// cluster: a bare Service name, or a name under .svc or .cluster.local.
func isClusterInternalHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || strings.Contains(host, "*") || net.ParseIP(host) != nil {
		return false
	}
	return !strings.Contains(host, ".") ||
		strings.HasSuffix(host, ".svc") || strings.Contains(host, ".svc.") ||
		strings.HasSuffix(host, ".cluster.local")
}

// serviceNamespace extracts the namespace of "<svc>.<ns>.svc[.<domain>]".
func serviceNamespace(host string) (string, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSuffix(host, ".")), ".")
	if len(parts) >= 3 && parts[2] == "svc" && parts[1] != "" {
		return parts[1], true
	}
	return "", false
}

func hostCIDR(ip net.IP) string {
	if ip.To4() != nil {
		return ip.String() + "/32"
	}
	return ip.String() + "/128"
}

func parsePort(s string) (int32, bool) {
	var n int32
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + (c - '0')
		if n > 65535 {
			return 0, false
		}
	}
	return n, n > 0
}

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
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// setCondition upserts a metav1.Condition into a conditions slice.
// If a condition with the same Type already exists, it is replaced only if
// the Status or Message changed (to avoid unnecessary status updates).
func setCondition(conditions *[]metav1.Condition, desired metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == desired.Type {
			if c.Status == desired.Status && c.Message == desired.Message {
				return // no change
			}
			(*conditions)[i] = desired
			return
		}
	}
	*conditions = append(*conditions, desired)
}

// ipAddrRe matches IPv4 addresses with optional port (e.g. 10.96.0.10:53).
var ipAddrRe = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?\b`)

// k8sDNSToRe matches Kubernetes internal DNS names (e.g. ollama-embed.agent-orc-system.svc
// or ollama-embed.agent-orc-system.svc.cluster.local).
var k8sDNSToRe = regexp.MustCompile(`[a-zA-Z0-9-]+(\.[a-zA-Z0-9-]+)*\.svc(\.cluster\.local)?`)

// sanitizeKBError strips internal infrastructure details (IPs, Kubernetes DNS
// names, service URLs) from a raw error string so that error text shown to users
// in the UI doesn't leak cluster-internal networking. Common failure patterns are
// simplified to user-friendly descriptions; everything else has IPs and DNS names
// redacted as a safety net.
//
// This function operates on the *raw error* only — the operation context
// (e.g. "discovering embedding dimension") should be added by the caller:
//
//	fmt.Sprintf("discovering embedding dimension: %s", sanitizeKBError(err.Error()))
func sanitizeKBError(rawErr string) string {
	if rawErr == "" {
		return ""
	}

	// Each entry checks for an error keyword and provides a user-friendly
	// replacement that doesn't leak internal details.
	errorPatterns := []struct {
		keyword     string
		replacement string
	}{
		// DNS resolution failures — the embedding model provider service name
		// doesn't resolve. Covers "no such host" and "lookup <name> on <ip>".
		{"no such host", "embedding model provider is unreachable (DNS resolution failed)"},
		{"name resolution failure", "embedding model provider is unreachable (DNS resolution failed)"},
		{"cannot resolve", "embedding model provider is unreachable (DNS resolution failed)"},
		// Connection refused — hostname resolved but nothing is listening.
		{"connection refused", "embedding model provider is unreachable (connection refused)"},
		// I/O timeouts — the provider is too slow to respond.
		{"i/o timeout", "embedding model provider is unreachable (timed out)"},
		{"deadline exceeded", "embedding model provider is unreachable (timed out)"},
		// HTTP-level errors from the provider.
		{"HTTP 401", "embedding model provider authentication failed"},
		{"HTTP 403", "embedding model provider access denied"},
		{"HTTP 404", "embedding model provider endpoint not found"},
		{"HTTP 5", "embedding model provider returned a server error"},
		// K8s API errors.
		{"forbidden", "access denied: operator lacks permissions to read a required secret or resource"},
		{"is forbidden", "access denied: operator lacks permissions to read a required secret or resource"},
		{"not found", "a required Kubernetes resource was not found"},
	}

	for _, p := range errorPatterns {
		if strings.Contains(rawErr, p.keyword) {
			return p.replacement
		}
	}

	// Fallback: strip remaining IPs and internal DNS names from the raw error
	// so no cluster-internal networking details leak to the UI.
	rawErr = ipAddrRe.ReplaceAllString(rawErr, "<host>")
	rawErr = k8sDNSToRe.ReplaceAllString(rawErr, "<host>")
	rawErr = strings.ReplaceAll(rawErr, "\"", "")
	return rawErr
}

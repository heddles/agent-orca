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

package rag

import (
	"net"
	"net/url"
	"strings"
)

// Limits describes the per-provider request shaping policy applied by
// EmbeddingClient: how many requests may fly in parallel, the per-minute
// request and token budgets, and how large a single batch may be.
//
// A zero RPM or TPM means "unlimited" (no pacing for that dimension).
type Limits struct {
	// RPM is the request-per-minute budget the client stays under.
	RPM int
	// TPM is the token-per-minute budget the client stays under. Token counts
	// are estimated from character counts (≈4 chars/token) and reconciled
	// against the API's reported usage when available.
	TPM int
	// MaxBatchChars caps the total characters sent in one embedding request.
	MaxBatchChars int
	// MaxBatchChunks caps the number of texts sent in one embedding request.
	MaxBatchChunks int
	// Concurrency caps the number of embedding requests in flight.
	Concurrency int
}

// Preset defaults for known providers. Values are deliberately conservative —
// sized for the tier a typical API key gets out of the box — so a fresh key
// never trips RESOURCE_EXHAUSTED on first use. The adaptive limiter backs off
// further on 429s and grows back to these values on sustained success; a
// higher-tier key needs no configuration, it just gets throttle corrections
// it rarely needs.
//
// These tables cover the *account-level* budgets that apply to raw embedding
// API keys (Google AI studio free tier, OpenAI pay-as-you-go). Higher tiers
// only relax the limits, so staying under the smallest published tier is
// always safe.
//
// Note on scope: each process holds its own limiter. The operator, the
// apiserver's query path (_rag_search), and every mcp-ingester Job pod pace
// independently against the same API key, so combined dispatch can briefly
// exceed the account budget until AIMD observes 429s and shrinks. Each
// fresh Job intentionally starts with a full bucket; EMBEDDING_CONCURRENCY
// is the operator's knob for dampening that, not an account-level guarantee.
var providerPresets = map[string]Limits{
	// Gemini: free-tier gemini-embedding-001 budgets (RPM 100 / TPM 30k).
	// Batches are kept far below TPM so one request can never exhaust the
	// per-minute token bucket on its own.
	"generativelanguage.googleapis.com": {
		RPM:            100,
		TPM:            30_000,
		MaxBatchChars:  16_000,
		MaxBatchChunks: 8,
		Concurrency:    2,
	},
	// OpenAI: tier-1 budgets are generous (text-embedding-3-*: ≥3k RPM,
	// ≥1M TPM); cap requests below the smallest tier.
	"api.openai.com": {
		RPM:            1_000,
		TPM:            1_000_000,
		MaxBatchChars:  60_000,
		MaxBatchChunks: maxBatchChunks,
		Concurrency:    4,
	},
}

// unknownRemoteLimits applies to hosted endpoints without a curated preset
// (Mistral, Together, LiteLLM proxies in front of unknown providers, ...).
// Request volume is paced conservatively; token pacing is skipped because a
// fabricated TPM would be a guess — the adaptive limiter shrinks batches on
// real 429 feedback instead.
var unknownRemoteLimits = Limits{
	RPM:            240,
	TPM:            0,
	MaxBatchChars:  maxBatchCharacterLimit,
	MaxBatchChunks: maxBatchChunks,
	Concurrency:    2,
}

// localLimits applies to self-hosted backends (Ollama, vLLM, LiteLLM in
// cluster): no artificial pacing, matching the previous hardcoded behavior.
var localLimits = Limits{
	RPM:            0,
	TPM:            0,
	MaxBatchChars:  maxBatchCharacterLimit,
	MaxBatchChunks: maxBatchChunks,
	Concurrency:    defaultEmbedConcurrency,
}

// ResolveLimits returns the rate-limit policy for an embeddings endpoint,
// derived from the endpoint host. It deliberately errs on the side of fewer
// requests for remote hosts and full speed for anything local.
func ResolveLimits(endpoint string) Limits {
	host := endpointHost(endpoint)

	// Local and in-cluster backends saturate CPU/GPU without remote-quota
	// concerns: localhost, bare IPs, RFC1918 ranges, and k8s service names
	// ("*.svc", "*.svc.cluster.local", dotless names like "ollama").
	if isLocalHost(host) {
		return localLimits
	}
	if preset, ok := providerPresets[host]; ok {
		return preset
	}
	return unknownRemoteLimits
}

// ApplyOverrides layers explicit operator configuration (from
// ModelProvider.spec.constraints rpm/tpm) over the derived preset. Values <= 0
// leave the preset untouched.
func ApplyOverrides(base Limits, rpm, tpm int) Limits {
	if rpm > 0 {
		base.RPM = rpm
	}
	if tpm > 0 {
		base.TPM = tpm
	}
	return base
}

// endpointHost extracts the hostname from an endpoint URL, tolerating
// scheme-less values like "http://localhost:11434" or "api.openai.com".
func endpointHost(endpoint string) string {
	s := strings.TrimSpace(endpoint)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	if u, err := url.Parse(s); err == nil {
		return strings.ToLower(u.Hostname())
	}
	return ""
}

// isLocalHost reports whether the host refers to the local machine, a private
// network, or an in-cluster service — none of which have provider quotas.
func isLocalHost(host string) bool {
	switch {
	case host == "":
		return false
	case host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "host.docker.internal":
		return true
	case !strings.Contains(host, "."):
		// k8s service short name ("ollama", "qdrant").
		return true
	case strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal"):
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return false
}

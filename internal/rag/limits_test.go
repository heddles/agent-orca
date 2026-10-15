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

import "testing"

func TestResolveLimits(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		wantRPM    int
		wantTPM    int
		wantChars  int
		wantChunks int
		wantConc   int
	}{
		{
			name:       "gemini openai-compat endpoint gets free-tier-safe preset",
			endpoint:   "https://generativelanguage.googleapis.com/v1beta/openai",
			wantRPM:    100,
			wantTPM:    30000,
			wantChars:  16000,
			wantChunks: 8,
			wantConc:   2,
		},
		{
			name:       "openai gets tier-1 preset",
			endpoint:   "https://api.openai.com",
			wantRPM:    1000,
			wantTPM:    1000000,
			wantChars:  60000,
			wantChunks: 32,
			wantConc:   4,
		},
		{
			name:       "localhost is unlimited (Ollama)",
			endpoint:   "http://localhost:11434",
			wantRPM:    0,
			wantTPM:    0,
			wantChars:  250000,
			wantChunks: 32,
			wantConc:   4,
		},
		{
			name:       "httptest 127.0.0.1 is unlimited",
			endpoint:   "http://127.0.0.1:54321",
			wantRPM:    0,
			wantTPM:    0,
			wantChars:  250000,
			wantChunks: 32,
			wantConc:   4,
		},
		{
			name:       "in-cluster service name is unlimited",
			endpoint:   "http://embed-service.default.svc.cluster.local:80",
			wantRPM:    0,
			wantTPM:    0,
			wantChars:  250000,
			wantChunks: 32,
			wantConc:   4,
		},
		{
			name:       "private subnet is unlimited",
			endpoint:   "http://10.0.0.7:8000",
			wantRPM:    0,
			wantTPM:    0,
			wantChars:  250000,
			wantChunks: 32,
			wantConc:   4,
		},
		{
			name:       "unknown remote host is paced conservatively, no fake TPM",
			endpoint:   "https://api.mistral.ai",
			wantRPM:    240,
			wantTPM:    0,
			wantChars:  250000,
			wantChunks: 32,
			wantConc:   2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveLimits(tc.endpoint)
			if got.RPM != tc.wantRPM || got.TPM != tc.wantTPM ||
				got.MaxBatchChars != tc.wantChars || got.MaxBatchChunks != tc.wantChunks ||
				got.Concurrency != tc.wantConc {
				t.Errorf("ResolveLimits(%q) = %+v, want rpm=%d tpm=%d chars=%d chunks=%d conc=%d",
					tc.endpoint, got, tc.wantRPM, tc.wantTPM, tc.wantChars, tc.wantChunks, tc.wantConc)
			}
		})
	}
}

func TestApplyOverrides(t *testing.T) {
	base := ResolveLimits("https://api.openai.com")

	// Zero values leave the preset untouched.
	same := ApplyOverrides(base, 0, 0)
	if same != base {
		t.Errorf("ApplyOverrides with no overrides changed the preset: %+v", same)
	}

	overridden := ApplyOverrides(base, 10, 1234)
	if overridden.RPM != 10 || overridden.TPM != 1234 {
		t.Errorf("overrides not applied: %+v", overridden)
	}
	// Other preset fields carry over.
	if overridden.MaxBatchChars != base.MaxBatchChars || overridden.Concurrency != base.Concurrency {
		t.Errorf("non-overridden fields changed: %+v", overridden)
	}
}

// TestClientUsesPresetAndOverrides verifies the client constructor derives its
// policy from the endpoint and that WithRateLimits layers the CRD overrides.
func TestClientUsesPresetAndOverrides(t *testing.T) {
	c := NewEmbeddingClientWithKey("https://generativelanguage.googleapis.com/v1beta/openai", "k", "gemini-embedding-001", "", "")
	if c.limits.RPM != 100 || c.limits.TPM != 30000 {
		t.Errorf("gemini preset not applied: %+v", c.limits)
	}

	// CRD-level override wins.
	c.WithRateLimits(500, 60000)
	if c.limits.RPM != 500 || c.limits.TPM != 60000 {
		t.Errorf("WithRateLimits overrides not applied: %+v", c.limits)
	}

	// Zero overrides are a no-op.
	other := NewEmbeddingClientWithKey("https://api.openai.com", "k", "text-embedding-3-small", "", "")
	other.WithRateLimits(0, 0)
	if other.limits.RPM != 1000 {
		t.Errorf("zero override changed preset: %+v", other.limits)
	}
}

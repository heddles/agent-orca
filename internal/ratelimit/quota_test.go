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

package ratelimit

import "testing"

func TestClassifyQuotaError(t *testing.T) {
	tests := []struct {
		name string
		body string
		want QuotaScope
	}{
		{
			name: "google daily quota violation",
			body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[
				{"@type":"type.googleapis.com/google.rpc.QuotaFailure",
				 "violations":[{"quotaMetric":"generativelanguage.googleapis.com/embed_content_free_tier_requests","quotaId":"EmbedRequestsPerDayPerProjectPerModel-FreeTier"}]}]}}`,
			want: QuotaScopeDay,
		},
		{
			name: "google per-minute quota violation",
			body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[
				{"@type":"type.googleapis.com/google.rpc.QuotaFailure",
				 "violations":[{"quotaMetric":"generativelanguage.googleapis.com/embed_content_free_tier_requests","quotaId":"EmbedRequestsPerMinutePerProjectPerModel-FreeTier"}]}]}}`,
			want: QuotaScopeMinute,
		},
		{
			name: "day wins over minute when both present",
			body: `{"error":{"details":[
				{"@type":"type.googleapis.com/google.rpc.QuotaFailure",
				 "violations":[{"quotaId":"EmbedRequestsPerMinutePerProject"},{"quotaId":"EmbedRequestsPerDayPerProject"}]}]}}`,
			want: QuotaScopeDay,
		},
		{
			name: "generic RESOURCE_EXHAUSTED without quota details",
			body: `{"error":{"code":429,"message":"You exceeded your current quota","status":"RESOURCE_EXHAUSTED"}}`,
			want: QuotaScopeNone,
		},
		{
			name: "non-quota details ignored",
			body: `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.Help","links":[]}]}}`,
			want: QuotaScopeNone,
		},
		{
			name: "malformed body",
			body: `garbage`,
			want: QuotaScopeNone,
		},
		{
			name: "empty body",
			body: ``,
			want: QuotaScopeNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyQuotaError([]byte(tc.body)); got != tc.want {
				t.Errorf("ClassifyQuotaError() = %v, want %v", got, tc.want)
			}
		})
	}
}

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

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryDelay(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		body    string
		want    time.Duration
	}{
		{
			name: "google RetryInfo in body wins",
			body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"31s"}]}}`,
			want: 31 * time.Second,
		},
		{
			name:    "Retry-After header used when no RetryInfo",
			headers: http.Header{"Retry-After": []string{"8"}},
			want:    8 * time.Second,
		},
		{
			name:    "Retry-After float seconds",
			headers: http.Header{"Retry-After": []string{"1.5"}},
			want:    1500 * time.Millisecond,
		},
		{
			name:    "RetryInfo beats Retry-After",
			headers: http.Header{"Retry-After": []string{"2"}},
			body:    `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"45s"}]}}`,
			want:    45 * time.Second,
		},
		{
			name: "non-RetryInfo details ignored",
			body: `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.Help","links":[]}]}}`,
			want: 0,
		},
		{
			name: "zero delay rejected so backoff applies",
			body: `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0s"}]}}`,
			want: 0,
		},
		{
			name: "malformed body ignored",
			body: `not json`,
			want: 0,
		},
		{
			name: "nothing present",
			want: 0,
		},
		{
			name:    "negative Retry-After rejected",
			headers: http.Header{"Retry-After": []string{"-3"}},
			want:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseRetryDelay(tc.headers, []byte(tc.body)); got != tc.want {
				t.Errorf("ParseRetryDelay() = %v, want %v", got, tc.want)
			}
		})
	}
}

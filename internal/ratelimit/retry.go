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

// Package ratelimit holds provider-agnostic rate-limiting helpers shared by
// clients that talk to LLM/embedding APIs directly.
package ratelimit

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// googleErrorBody mirrors the JSON error envelope returned by Google APIs
// (Gemini, Vertex) and openly adopted elsewhere. Transient 429s carry a
// google.rpc.RetryInfo detail with a retryDelay that tells the caller exactly
// how long to wait; Retry-After headers are often absent on those responses.
type googleErrorBody struct {
	Error struct {
		Details []struct {
			Type       string `json:"@type"`
			RetryDelay string `json:"retryDelay"`
		} `json:"details"`
	} `json:"error"`
}

// ParseRetryDelay extracts a server-declared retry delay for a throttled
// response, preferring the most authoritative source available:
//
//  1. A google.rpc.RetryInfo retryDelay in the JSON error body (e.g. "31s").
//  2. The HTTP Retry-After header (seconds, integer or float).
//
// Returns 0 when neither is present or parseable; callers should fall back to
// their own exponential backoff.
func ParseRetryDelay(headers http.Header, body []byte) time.Duration {
	if d := parseRetryInfo(body); d > 0 {
		return d
	}
	return parseRetryAfterHeader(headers)
}

// parseRetryInfo scans error.details for a RetryInfo entry and parses its
// retryDelay (any duration accepted by time.ParseDuration, e.g. "31s").
func parseRetryInfo(body []byte) time.Duration {
	if len(body) == 0 {
		return 0
	}
	var ge googleErrorBody
	if err := json.Unmarshal(body, &ge); err != nil {
		return 0
	}
	for _, d := range ge.Error.Details {
		if !strings.HasSuffix(d.Type, "RetryInfo") || d.RetryDelay == "" {
			continue
		}
		if dur, err := time.ParseDuration(d.RetryDelay); err == nil && dur > 0 {
			return dur
		}
	}
	return 0
}

// parseRetryAfterHeader parses a numeric Retry-After header. HTTP-date
// Retry-After values are rare on rate-limited API responses and unsupported
// here (returns 0).
func parseRetryAfterHeader(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	v := strings.TrimSpace(headers.Get("Retry-After"))
	if v == "" {
		return 0
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

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
	"encoding/json"
	"strings"
)

// QuotaScope classifies which quota dimension a 429 response exhausted.
// The distinction drives fundamentally different reactions:
//
//   - Minute-scoped exhaustion (RPM/TPM) is transient: back off, shrink the
//     request budget, retry within seconds-to-minutes.
//   - Day-scoped exhaustion (RPD) is terminal for the window: every retry
//     until the provider's daily reset is guaranteed to fail and — on
//     metered APIs — costs money. Callers must stop and reschedule.
type QuotaScope int

const (
	// QuotaScopeNone means the body carries no recognizable quota details.
	QuotaScopeNone QuotaScope = iota
	// QuotaScopeMinute means a per-minute budget (RPM or TPM) was exceeded.
	QuotaScopeMinute
	// QuotaScopeDay means a per-day budget (RPD) was exceeded.
	QuotaScopeDay
)

// googleQuotaBody mirrors google.rpc.QuotaFailure details embedded in 429
// bodies. Google names quota dimensions in quotaId / quotaMetric strings such
// as "GenerateRequestsPerDayPerProjectPerModel-FreeTier" or
// "EmbedRequestsPerMinutePerProjectPerModel"; the PerDay/PerMinute infix is
// the stable signal for the scope.
type googleQuotaBody struct {
	Error struct {
		Details []struct {
			Type       string `json:"@type"`
			Violations []struct {
				QuotaID     string `json:"quotaId"`
				QuotaMetric string `json:"quotaMetric"`
			} `json:"violations"`
		} `json:"details"`
	} `json:"error"`
}

// ClassifyQuotaError inspects a 429 response body for google.rpc.QuotaFailure
// details and reports the strictest (longest) quota scope it names. Bodies
// without recognizable details classify as QuotaScopeNone — callers should
// treat that as minute-scoped (transient) since per-day exhaustion is
// practically always annotated.
func ClassifyQuotaError(body []byte) QuotaScope {
	if len(body) == 0 {
		return QuotaScopeNone
	}
	var q googleQuotaBody
	if err := json.Unmarshal(body, &q); err != nil {
		return QuotaScopeNone
	}
	scope := QuotaScopeNone
	for _, d := range q.Error.Details {
		if !strings.HasSuffix(d.Type, "QuotaFailure") {
			continue
		}
		for _, v := range d.Violations {
			id := strings.ToLower(v.QuotaID + " " + v.QuotaMetric)
			switch {
			case strings.Contains(id, "perday"):
				return QuotaScopeDay
			case strings.Contains(id, "perminute"):
				scope = QuotaScopeMinute
			}
		}
	}
	return scope
}

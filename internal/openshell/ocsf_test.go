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

package openshell

import "testing"

func TestParseDecision(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Decision
	}{
		{
			// Seen on pnw-dev (SP2): a connection allowed by an egress rule.
			name: "allowed connection",
			line: "NET:OPEN [INFO] ALLOWED /usr/bin/curl(0) -> pypi.org:443 [policy:agentorca-egress-0 engine:opa]",
			want: Decision{Kind: "net", Activity: "OPEN", Action: "allowed", Severity: "info",
				Binary: "/usr/bin/curl", Destination: "pypi.org:443", Policy: "agentorca-egress-0", Engine: "opa"},
		},
		{
			// Seen on pnw-dev (SP2): a write to a read-only host.
			name: "denied request",
			line: "HTTP:POST [MED] DENIED POST https://pypi.org/simple/ [policy:agentorca-egress-0 engine:l7] " +
				"[reason:L7_REQUEST deny POST pypi.org:443/simple/ reason=POST /simple/ not permitted by policy]",
			want: Decision{Kind: "http", Action: "denied", Severity: "med", Method: "POST", URL: "https://pypi.org/simple/",
				Policy: "agentorca-egress-0", Engine: "l7",
				Reason: "L7_REQUEST deny POST pypi.org:443/simple/ reason=POST /simple/ not permitted by policy"},
		},
		{
			name: "denied connection without a rule",
			line: "NET:OPEN [MED] DENIED /usr/bin/curl(412) -> example.com:443 [reason:no policy allows example.com:443]",
			want: Decision{Kind: "net", Activity: "OPEN", Action: "denied", Severity: "med",
				Binary: "/usr/bin/curl", Destination: "example.com:443", Reason: "no policy allows example.com:443"},
		},
		{
			name: "allowed request with an actor",
			line: "HTTP:GET [INFO] ALLOWED /usr/bin/git(77) -> GET https://github.com/heddles/agent-orca/info/refs [policy:agentorca-git-github engine:l7]",
			want: Decision{Kind: "http", Action: "allowed", Severity: "info", Binary: "/usr/bin/git", Method: "GET",
				URL: "https://github.com/heddles/agent-orca/info/refs", Policy: "agentorca-git-github", Engine: "l7"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseDecision(tt.line)
			if !ok {
				t.Fatalf("ParseDecision(%q) not ok", tt.line)
			}
			tt.want.Raw = tt.line
			if got != tt.want {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseDecisionIgnoresOtherEvents(t *testing.T) {
	for _, line := range []string{
		"CONFIG:LOADED [INFO] Acknowledged initial policy revision as loaded [version:2]",
		"NET:LISTEN [INFO] 127.0.0.1:3128",         // no decision
		"SSH:LISTEN [INFO]",                        // other class
		"Starting sandbox supervision",             // plain log line
		"PROC:LAUNCH [INFO] LAUNCHED /bin/bash(1)", // process event
	} {
		if d, ok := ParseDecision(line); ok {
			t.Errorf("ParseDecision(%q) = %+v, want not ok", line, d)
		}
	}
}

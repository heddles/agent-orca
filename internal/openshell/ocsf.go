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

// The sandbox supervisor reports each network decision as an OCSF event. It
// pushes them to the gateway's log store as lines of level "OCSF" whose message
// is the event's shorthand, e.g.
//
//	NET:OPEN [INFO] ALLOWED /usr/bin/curl(0) -> pypi.org:443 [policy:agentorca-egress-0 engine:opa]
//	HTTP:POST [MED] DENIED POST https://pypi.org/simple/ [policy:agentorca-egress-0 engine:l7] [reason:...]
//
// The shorthand is OpenShell's display format, not a stable schema, so the
// parser is tolerant: anything it cannot read keeps the raw line.

import (
	"regexp"
	"strings"
	"time"
)

// OCSFLogLevel is the level of the gateway log lines that carry OCSF events.
const OCSFLogLevel = "OCSF"

// Decision is one network decision the sandbox supervisor made.
type Decision struct {
	Time time.Time `json:"time"`
	// Kind is "net" (a connection) or "http" (a request on an allowed connection).
	Kind string `json:"kind"`
	// Activity is the OCSF activity, e.g. OPEN for a connection.
	Activity string `json:"activity,omitempty"`
	// Action is "allowed" or "denied" (lower-cased OCSF action).
	Action   string `json:"action"`
	Severity string `json:"severity,omitempty"`
	// Binary is the process that connected (without its pid).
	Binary string `json:"binary,omitempty"`
	// Destination is host:port for connections.
	Destination string `json:"destination,omitempty"`
	Method      string `json:"method,omitempty"`
	URL         string `json:"url,omitempty"`
	// Policy and Engine name the rule that decided.
	Policy string `json:"policy,omitempty"`
	Engine string `json:"engine,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Raw is the original shorthand line.
	Raw string `json:"raw"`
}

var (
	shorthandHead = regexp.MustCompile(`^(NET|HTTP):(\S+) \[(\w+)\](.*)$`)
	contextBlock  = regexp.MustCompile(`\[((?:\\.|[^\]\\])*)\]`)
	actorPattern  = regexp.MustCompile(`^(\S+)\((\d+)\)$`)
)

// ParseDecision reads a NET or HTTP shorthand event. ok is false for other
// lines (process, SSH, config and inference events).
func ParseDecision(line string) (d Decision, ok bool) {
	line = strings.TrimSpace(line)
	m := shorthandHead.FindStringSubmatch(line)
	if m == nil {
		return d, false
	}
	d.Raw = line
	d.Kind = strings.ToLower(m[1])
	d.Severity = strings.ToLower(m[3])
	rest := m[4]
	if d.Kind == "net" {
		d.Activity = m[2]
	} else {
		d.Method = m[2]
	}

	// Context blocks ([policy:... engine:...], [reason:...]) trail the detail.
	detail := rest
	if i := strings.Index(rest, " ["); i >= 0 {
		detail = rest[:i]
		for _, b := range contextBlock.FindAllStringSubmatch(rest[i:], -1) {
			parseContext(&d, b[1])
		}
	}

	fields := strings.Fields(detail)
	if len(fields) > 0 && (fields[0] == "ALLOWED" || fields[0] == "DENIED") {
		d.Action = strings.ToLower(fields[0])
		fields = fields[1:]
	}
	// "<actor> -> <target>" or just "<target>".
	if len(fields) >= 2 && fields[1] == "->" {
		d.Binary = actorBinary(fields[0])
		fields = fields[2:]
	}
	switch d.Kind {
	case "net":
		if len(fields) > 0 {
			d.Destination = fields[0]
		}
	case "http":
		if len(fields) >= 2 {
			d.Method, d.URL = fields[0], fields[1]
		} else if len(fields) == 1 {
			d.URL = fields[0]
		}
	}
	return d, d.Action != ""
}

func actorBinary(actor string) string {
	if m := actorPattern.FindStringSubmatch(actor); m != nil {
		return m[1]
	}
	return actor
}

// parseContext reads one bracketed block: "policy:x engine:y" or "reason:text".
func parseContext(d *Decision, block string) {
	if reason, ok := strings.CutPrefix(block, "reason:"); ok {
		d.Reason = reason
		return
	}
	for kv := range strings.FieldsSeq(block) {
		k, v, ok := strings.Cut(kv, ":")
		if !ok {
			continue
		}
		switch k {
		case "policy":
			d.Policy = v
		case "engine":
			d.Engine = v
		case "reason":
			d.Reason = v
		}
	}
}

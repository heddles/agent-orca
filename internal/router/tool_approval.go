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

package router

// Tool side-effect gate (Tool.spec.sideEffect, GuardrailPolicy.spec.toolApproval).
// Before an operator-defined tool runs, the router looks up what its class
// needs: nothing, an approval, or a refusal. An approval is requested from the
// operator, which stores it in the approval engine (inbox, notification, audit
// row) and takes the approver groups and timeout from the policy, never from
// this request. The router polls until the approval is decided. A rejection, a
// timeout or an unreachable approval service means the tool does not run (it
// fails closed) and the model gets an error it can act on; the run goes on.
// Built-in tools and tools the agent runs itself (localTools) are not gated.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// traceEventTypeToolApproval is the trace event for a gated tool call.
const traceEventTypeToolApproval = "toolApproval"

// Requirements a tool class can map to (GuardrailPolicy.spec.toolApproval).
const (
	toolRequirementNone    = "none"
	toolRequirementApprove = "approve"
	toolRequirementDeny    = "deny"
)

// Tool side-effect classes.
const toolSideEffectActuate = "actuate"

const (
	// defaultToolApprovalPoll is how often a waiting call asks for the decision.
	defaultToolApprovalPoll = 3 * time.Second
	// toolApprovalMaxPollErrors is how many polls in a row may fail before the
	// call is refused.
	toolApprovalMaxPollErrors = 20
	// toolApprovalArgsLimit caps the arguments sent for the approver to read.
	toolApprovalArgsLimit = 4000
)

// ToolApprovalConfig maps tool side-effect classes to requirements. An empty
// field takes the default: approve for actuate, none for the rest.
type ToolApprovalConfig struct {
	Read         string `json:"read,omitempty"`
	Write        string `json:"write,omitempty"`
	Actuate      string `json:"actuate,omitempty"`
	Unclassified string `json:"unclassified,omitempty"`
}

// requirement returns what a tool of the given class needs. A nil config uses
// the defaults.
func (c *ToolApprovalConfig) requirement(class string) string {
	var set string
	if c != nil {
		switch class {
		case "read":
			set = c.Read
		case "write":
			set = c.Write
		case toolSideEffectActuate:
			set = c.Actuate
		default:
			set = c.Unclassified
		}
	}
	switch set {
	case toolRequirementNone, toolRequirementApprove, toolRequirementDeny:
		return set
	}
	if class == toolSideEffectActuate {
		return toolRequirementApprove
	}
	return toolRequirementNone
}

// toolApprovalState is the operator's answer about one approval.
type toolApprovalState struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	DecidedBy string `json:"decidedBy,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (r *Router) findToolDefinition(name string) *ToolDefinition {
	for i := range r.cfg.ToolDefinitions {
		if r.cfg.ToolDefinitions[i].Name == name {
			return &r.cfg.ToolDefinitions[i]
		}
	}
	return nil
}

// gateToolCall decides whether tc may run. It returns "" when it may, or the
// result to hand the model instead of running the tool.
func (r *Router) gateToolCall(ctx context.Context, tc ToolCall) string {
	td := r.findToolDefinition(tc.Function.Name)
	if td == nil {
		return ""
	}
	switch r.cfg.ToolApproval.requirement(td.SideEffect) {
	case toolRequirementNone:
		return ""
	case toolRequirementDeny:
		r.emitToolApproval("denied", td, "", "", "the run's guardrail policy refuses "+classLabel(td)+" tools")
		return toolRefusal(td.Name, fmt.Sprintf("the guardrail policy does not allow %s tools in this run", classLabel(td)))
	}
	return r.waitForToolApproval(ctx, td, tc)
}

func classLabel(td *ToolDefinition) string {
	if td.SideEffect == "" {
		return "unclassified"
	}
	return td.SideEffect
}

// toolRefusal is the tool result for a call that did not run.
func toolRefusal(tool, why string) string {
	b, _ := json.Marshal(map[string]string{
		"error": fmt.Sprintf("tool %q was not run: %s. Do not repeat the same call; choose another approach or ask for guidance.", tool, why),
	})
	return string(b)
}

func (r *Router) emitToolApproval(state string, td *ToolDefinition, by, id, reason string) {
	ev, _ := json.Marshal(map[string]string{
		"type": traceEventTypeToolApproval, "state": state, "tool": td.Name,
		"sideEffect": td.SideEffect, "by": by, "approvalId": id, "reason": reason,
	})
	r.emitTraceEventBoth(string(ev))
}

// waitForToolApproval requests an approval and blocks until it is decided.
func (r *Router) waitForToolApproval(ctx context.Context, td *ToolDefinition, tc ToolCall) string {
	args := tc.Function.Arguments
	if len(args) > toolApprovalArgsLimit {
		args = args[:toolApprovalArgsLimit] + "…"
	}
	callKey := randomHex(8)
	body, _ := json.Marshal(map[string]string{
		"callKey": callKey, "tool": td.Name, "sideEffect": td.SideEffect, "arguments": args,
	})
	raw, err := r.operatorCall(ctx, http.MethodPost, "tool-approval", body)
	var st toolApprovalState
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	if err != nil || st.ID == "" {
		slog.Warn("requesting a tool approval", "run", r.cfg.RunName, "tool", td.Name, "err", err)
		r.emitToolApproval("unavailable", td, "", "", "the approval service could not be reached")
		return toolRefusal(td.Name, "it needs an approver and the approval service is unavailable")
	}
	slog.Info("tool call waiting for approval", "run", r.cfg.RunName, "tool", td.Name, "approval", st.ID)
	r.emitToolApproval("requested", td, "", st.ID, "")

	poll := time.NewTicker(r.toolApprovalPollInterval())
	defer poll.Stop()
	failures := 0
	for {
		switch st.Status {
		case "approved":
			r.emitToolApproval("approved", td, st.DecidedBy, st.ID, st.Reason)
			return ""
		case "rejected":
			r.emitToolApproval("rejected", td, st.DecidedBy, st.ID, st.Reason)
			return toolRefusal(td.Name, rejectionText("an approver rejected it", st))
		case "expired":
			r.emitToolApproval("expired", td, "", st.ID, st.Reason)
			return toolRefusal(td.Name, "no approver decided in time")
		case "withdrawn":
			r.emitToolApproval("withdrawn", td, "", st.ID, st.Reason)
			return toolRefusal(td.Name, "the request was withdrawn")
		}
		select {
		case <-ctx.Done():
			return toolRefusal(td.Name, "the run ended before an approver decided")
		case <-r.cancelCtx.Done():
			return toolRefusal(td.Name, "the run ended before an approver decided")
		case <-poll.C:
		}
		raw, err := r.operatorCall(ctx, http.MethodGet, "tool-approval/"+st.ID, nil)
		if err == nil {
			var next toolApprovalState
			if err = json.Unmarshal(raw, &next); err == nil {
				st, failures = next, 0
				continue
			}
		}
		if failures++; failures >= toolApprovalMaxPollErrors {
			slog.Warn("polling a tool approval", "run", r.cfg.RunName, "approval", st.ID, "err", err)
			r.emitToolApproval("unavailable", td, "", st.ID, "the approval service stopped answering")
			return toolRefusal(td.Name, "the approval service stopped answering")
		}
	}
}

func rejectionText(base string, st toolApprovalState) string {
	if st.DecidedBy != "" {
		base += " (" + st.DecidedBy + ")"
	}
	if st.Reason != "" {
		base += ": " + st.Reason
	}
	return base
}

func (r *Router) toolApprovalPollInterval() time.Duration {
	if r.toolApprovalPoll > 0 {
		return r.toolApprovalPoll
	}
	return defaultToolApprovalPoll
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

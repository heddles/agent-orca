package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// Tool-call approvals in the thread.
//
// A tool call that needs an approver (Tool.spec.sideEffect with a
// GuardrailPolicy) is held by the router and recorded as an approval. For a run
// that a channel started, the pipeline also says so in the thread, and an
// approver's reply there ("approve" or "deny") decides it. The approval is the
// same record the console shows, so a decision made in either place is the only
// one: the second is told the request is no longer waiting.
//
// Who may decide is the connection's `approvers` list of exact sender ids. A
// channel sender has no console identity, so a request that is limited to
// approver groups (GuardrailPolicy.spec.toolApproval.approverGroups) is never
// decided from a channel.

// ApprovalStore is the part of the approval engine the pipeline uses
// (postgresql.Store).
type ApprovalStore interface {
	ListApprovals(ctx context.Context, q postgresql.ApprovalQuery) ([]postgresql.Approval, error)
	DecideApproval(ctx context.Context, id, status, by, reason string) (postgresql.Approval, error)
}

// RunMessages finds the message a running run answers. The store of the
// pipeline may implement it (postgresql.Store does).
type RunMessages interface {
	FindRunningInboundMessage(ctx context.Context, namespace, runName string) (postgresql.InboundMessage, bool, error)
}

// OutcomeDecided: the message was a decision on a waiting tool call.
const OutcomeDecided Outcome = "decided"

const (
	decidedByPrefix = "channel:"
	maxPromptArgs   = 300
)

// parseDecision reads "approve", "deny" and their plain synonyms, with an
// optional approval id (or its first characters). Anything else, including a
// sentence that merely starts with one of those words, is not a decision.
func parseDecision(text string) (approve bool, id string, ok bool) {
	f := strings.Fields(strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!`*")))
	if len(f) == 0 || len(f) > 2 {
		return false, "", false
	}
	switch f[0] {
	case "approve", "approved", "yes", "allow":
		approve = true
	case "deny", "denied", "reject", "no":
	default:
		return false, "", false
	}
	if len(f) == 2 {
		id = strings.Trim(f[1], "`.*")
		if len(id) < 4 {
			return false, "", false
		}
	}
	return approve, id, true
}

// decide handles a message that decides a tool call waiting in the thread's run.
// It returns what to tell the sender, and false when the message is not a
// decision on anything (it is then an ordinary message).
func (p *Pipeline) decide(ctx context.Context, c Connection, m Message, run string) (string, bool) {
	if p.Approvals == nil || run == "" {
		return "", false
	}
	approve, prefix, ok := parseDecision(m.Text)
	if !ok {
		return "", false
	}
	pending, err := p.Approvals.ListApprovals(ctx, postgresql.ApprovalQuery{
		TenantID: c.TenantID, Namespace: c.Namespace, RunName: run, Status: postgresql.ApprovalPending,
	})
	if err != nil {
		slog.Warn("listing the approvals of a thread", "connection", c.ID, "err", err)
		return "", false
	}
	var tools []postgresql.Approval
	for _, a := range pending {
		if a.Kind == postgresql.ApprovalKindTool && (prefix == "" || strings.HasPrefix(strings.ToLower(a.ID), prefix)) {
			tools = append(tools, a)
		}
	}
	switch {
	case len(tools) == 0:
		return "", false
	case len(tools) > 1:
		return "More than one action is waiting. Reply \"approve <id>\" or \"deny <id>\" with the first characters of its id:\n" + listWaiting(tools), true
	}
	a := tools[0]
	if !c.Settings.CanApprove(m.Sender) {
		return "You are not set up to decide this from here. An approver can reply in this thread, or decide it in the console.", true
	}
	if len(a.ApproverGroups) > 0 {
		return "This action is limited to an approver group, so it can only be decided in the console.", true
	}
	status, word := postgresql.ApprovalRejected, "Denied"
	if approve {
		status, word = postgresql.ApprovalApproved, "Approved"
	}
	by := decidedByPrefix + c.Kind + ":" + normalize(m.Sender.ID)
	if _, err := p.Approvals.DecideApproval(ctx, a.ID, status, by, "decided in the "+c.Kind+" thread"); err != nil {
		if errors.Is(err, postgresql.ErrApprovalNotPending) {
			return "That request is no longer waiting.", true
		}
		slog.Warn("deciding an approval from a channel", "connection", c.ID, "approval", a.ID, "err", err)
		return "The decision could not be recorded. Try again, or decide it in the console.", true
	}
	slog.Info("approval decided from a channel", "approval", a.ID, "status", status, "by", by, "run", run, "namespace", a.Namespace)
	return word + ": " + a.Summary + ".", true
}

func listWaiting(as []postgresql.Approval) string {
	var b strings.Builder
	for _, a := range as {
		fmt.Fprintf(&b, "- %s: %s\n", shortID(a.ID), a.Summary)
	}
	return strings.TrimSpace(b.String())
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// NotifyApproval says in the thread that a tool call waits, when the run was
// started by a channel message. It satisfies the notifier of the tool-approval
// endpoint, which calls it once, when the approval is first created. Failing
// to say so is not an error for the approval: the console still has it.
func (p *Pipeline) NotifyApproval(ctx context.Context, a postgresql.Approval) error {
	if a.Kind != postgresql.ApprovalKindTool {
		return nil
	}
	finder, ok := p.Store.(RunMessages)
	if !ok {
		return nil
	}
	m, found, err := finder.FindRunningInboundMessage(ctx, a.Namespace, a.RunName)
	if err != nil || !found || len(m.ReplyTarget) == 0 {
		return err
	}
	sc, err := p.Store.GetInboundConnection(ctx, m.ConnectionID)
	if err != nil || !sc.Enabled {
		return err
	}
	c, err := p.Resolve(ctx, sc)
	ad, ok := p.Registry.Get(sc.Kind)
	if err != nil || !ok {
		return err
	}
	// On a shared app, who may approve is the run's agent's list.
	c = p.routeOfRun(ctx, c, a.Namespace, a.RunName)
	res := p.Prepare(c, Result{Text: approvalPrompt(a, c.Settings), Notice: true})
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ad.Reply(rctx, c, m.ReplyTarget, res); err != nil {
		return fmt.Errorf("asking in the thread: %s", safeErr(err, c))
	}
	return nil
}

func approvalPrompt(a postgresql.Approval, s Settings) string {
	var d struct {
		Tool       string `json:"tool"`
		SideEffect string `json:"sideEffect"`
		Arguments  string `json:"arguments"`
	}
	_ = json.Unmarshal(a.Details, &d)
	var b strings.Builder
	fmt.Fprintf(&b, "Approval needed: the agent wants to run %q", d.Tool)
	if d.SideEffect != "" {
		fmt.Fprintf(&b, " (%s)", d.SideEffect)
	}
	b.WriteString(".")
	if d.Arguments != "" {
		fmt.Fprintf(&b, "\nWith: %s", clip(oneLine(d.Arguments, maxPromptArgs), maxPromptArgs))
	}
	if len(a.ApproverGroups) == 0 && len(s.Approvers) > 0 {
		fmt.Fprintf(&b, "\nAn approver can reply \"approve\" or \"deny\" here (id %s).", shortID(a.ID))
	} else {
		b.WriteString("\nAn approver needs to decide it in the console.")
	}
	return b.String()
}

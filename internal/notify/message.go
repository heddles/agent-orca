package notify

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/heddles/agent-orca/internal/redact"
)

// MaxOutput is the most characters of a run's output a message carries.
const MaxOutput = 2000

// Message is what a notification says.
type Message struct {
	Schedule  string
	Namespace string
	Agent     string
	// Outcome is Succeeded, Failed, Skipped or BreakerOpen.
	Outcome string
	// Run is empty for Skipped and BreakerOpen, which are about the schedule.
	Run      string
	Duration time.Duration
	// CostUSD is the run's spend, as the run reports it.
	CostUSD string
	// Link opens the run in the UI. Empty when the operator does not know the
	// UI's address.
	Link string
	// Reason is why a run failed, or why the schedule stopped.
	Reason string
	// Output is the run's output. Set it only when the schedule asks for it.
	Output string
	Time   time.Time
}

// Scrub returns m with Reason and Output made safe to send: known secrets
// (the channel's own and anything registered with internal/redact) masked, then
// cut to MaxOutput characters. Masking comes first so a secret cut in half
// cannot slip past it.
func (m Message) Scrub(extra *redact.Registry) Message {
	clean := func(s string) string {
		s = redact.String(s)
		if extra != nil {
			s = extra.String(s)
		}
		return truncate(s, MaxOutput)
	}
	m.Reason = clean(m.Reason)
	m.Output = clean(m.Output)
	return m
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func (m Message) subject() string {
	what := m.Outcome
	switch m.Outcome {
	case "BreakerOpen":
		what = "stopped after repeated failures"
	case "Skipped":
		what = "skipped: spend cap reached"
	}
	return oneLine(fmt.Sprintf("Schedule %s: %s", m.Schedule, what))
}

// lines is the message as plain text.
func (m Message) lines() []string {
	out := []string{m.subject()}
	add := func(k, v string) {
		if v != "" {
			out = append(out, k+": "+oneLine(v))
		}
	}
	add("Agent", m.Agent)
	add("Run", m.Run)
	if m.Duration > 0 {
		add("Duration", m.Duration.Round(time.Second).String())
	}
	if m.CostUSD != "" {
		add("Cost", "$"+m.CostUSD)
	}
	add("Reason", m.Reason)
	add("Link", m.Link)
	return out
}

func (m Message) text() string {
	t := strings.Join(m.lines(), "\n")
	if m.Output != "" {
		t += "\n\nOutput:\n" + m.Output
	}
	return t
}

// slackText is the message in Slack's mrkdwn: &, < and > are escaped, so output
// cannot make a mention or a link.
func (m Message) slackText() string {
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	lines := m.lines()
	for i := range lines {
		lines[i] = esc(lines[i])
	}
	if m.Link != "" {
		lines[len(lines)-1] = fmt.Sprintf("<%s|Open the run>", m.Link)
	}
	t := strings.Join(lines, "\n")
	if m.Output != "" {
		t += "\n```" + strings.ReplaceAll(esc(m.Output), "```", "'''") + "```"
	}
	return t
}

// payload is the webhook's JSON body.
func (m Message) payload() map[string]any {
	p := map[string]any{
		"schedule": m.Schedule, "namespace": m.Namespace, "agent": m.Agent, "outcome": m.Outcome,
		"time": m.Time.UTC().Format(time.RFC3339),
	}
	set := func(k, v string) {
		if v != "" {
			p[k] = v
		}
	}
	set("run", m.Run)
	set("costUSD", m.CostUSD)
	set("link", m.Link)
	set("reason", m.Reason)
	set("output", m.Output)
	if m.Duration > 0 {
		p["durationSeconds"] = int64(m.Duration.Seconds())
	}
	return p
}

func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

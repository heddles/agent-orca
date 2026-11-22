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

// Package inbound is the vendor-neutral core of the channels that let people
// start a run of an agent from outside agent-orca and get the result back in
// the same conversation (docs/inbound-channels.md).
//
// An Adapter speaks one channel's protocol and turns what arrives into a
// Message; everything else is shared and lives here: de-duplication, the
// sender allow-list, rate limits, mapping to an AgentRun (as a trigger actor,
// never as a person), threading, delivery of the result with the channel's
// limits, redaction and failure handling. Nothing in this package knows a
// channel by name; the adapters are registered by the binary that wires them.
package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Capabilities describe what a channel can carry, so the core can shape a reply.
type Capabilities struct {
	// Threads: the channel has conversations, so a message can continue one.
	Threads bool
	// Attachments: the channel can carry files in either direction.
	Attachments bool
	// Markdown: the channel renders Markdown. Plain text is always safe.
	Markdown bool
	// MaxReplyChars caps a reply, in characters. Zero means no cap. For an adapter
	// with MaxReplyParts above 1 it is the cap of one message.
	MaxReplyChars int

	// The rest is for chat platforms (docs/inbound-channels.md, "Chat contract").

	// Dialect is the markup the channel renders (see Dialect). Empty is DialectNone.
	// The adapter formats its replies with internal/inbound/chat.
	Dialect Dialect
	// MaxReplyParts is how many messages a long reply may be split into. Zero or
	// one means the reply is cut at MaxReplyChars instead.
	MaxReplyParts int
	// Modes are the ways a message can be addressed to the agent that the adapter
	// can deliver. Empty for a channel without that notion (email).
	Modes []Mode
	// AckFirst: the platform wants an answer to its request within seconds and
	// retries on silence, so the receive endpoint answers at once and lets the
	// pipeline run in the background (see Receiver).
	AckFirst bool
	// WorkingIndicator: the adapter implements Indicator.
	WorkingIndicator bool
}

// Dialect is the markup a channel renders.
type Dialect string

// Dialects.
const (
	DialectNone     Dialect = ""         // plain text
	DialectMrkdwn   Dialect = "mrkdwn"   // *bold* _italic_ <url|label>, & < > escaped
	DialectMarkdown Dialect = "markdown" // CommonMark-like
	// DialectMrkdwnRaw is mrkdwn on a platform that does not decode entities:
	// *bold* _italic_ ~strike~ <url|label>, and & < > are sent as they are.
	DialectMrkdwnRaw Dialect = "mrkdwn-raw"
	DialectHTML      Dialect = "html" // a small HTML subset
)

// Mode is how a message was addressed to the agent.
type Mode string

// Modes. A connection enables some of what its adapter supports (Settings.Modes).
const (
	// ModeDirect: a private conversation with the bot.
	ModeDirect Mode = "direct"
	// ModeMention: the bot was named in a shared space (@mention, trigger word).
	ModeMention Mode = "mention"
	// ModeCommand: a slash command.
	ModeCommand Mode = "command"
	// ModeChannel: any message in a space, without naming the bot. Off unless
	// enabled, and only for the spaces on the connection's channel allow-list.
	ModeChannel Mode = "channel"
)

// KnownMode reports whether m is one of the modes above.
func KnownMode(m Mode) bool {
	switch m {
	case ModeDirect, ModeMention, ModeCommand, ModeChannel:
		return true
	}
	return false
}

// Sender is who wrote a message, as the adapter established it.
type Sender struct {
	// ID is the normalised identity the allow-list is matched against and the
	// reply cap is counted by: an address, a user id. Required.
	ID string
	// Group is the larger body the sender belongs to (an email domain, the
	// workspace, tenant or organisation of a chat platform). The allow-list can
	// admit a whole group. May be empty.
	Group string
	// Display is a human label for the input; it is untrusted.
	Display string
}

// AttachmentInfo describes a file that came with a message.
type AttachmentInfo struct {
	Name        string
	ContentType string
	Size        int
	// Text is the content, set by the adapter only for a file its own policy
	// lets through. Empty for every other file: the agent sees its name and size.
	Text string
}

// ReplyTarget is whatever an adapter needs to answer one message. The core
// stores it (JSON) only while a reply is pending and never reads it.
type ReplyTarget = json.RawMessage

// Message is one inbound message, normalised.
type Message struct {
	// Kind is the adapter's kind; ConnectionID and TenantID are those of the
	// connection it arrived on (set by the core, an adapter cannot choose them).
	Kind         string
	ConnectionID string
	TenantID     string

	Sender Sender
	// UserID is the agent-orca user the sender was matched to, when the adapter
	// or the core could. Informational only: a run is never started as that user.
	UserID string

	Text  string
	Title string
	// Attachments describes the files that came along.
	Attachments []AttachmentInfo

	// ThreadID groups messages of one conversation; empty when the channel has none.
	// A chat adapter builds it from the workspace, the space and the platform's
	// thread so that two spaces never share a conversation.
	ThreadID string
	// Channel is the space a chat message was written in (channel, room, space,
	// conversation id), matched against the connection's channel allow-list.
	Channel string
	// Mode is how the message was addressed to the agent (chat adapters).
	Mode Mode
	// FromBot: the author is a bot, an app or the connection's own bot. The core
	// ignores the message; this is the loop prevention of chat, as auto-replies
	// are for email.
	FromBot bool
	// MessageID is the channel's id of the message, unique per connection. It is
	// the idempotency key.
	MessageID string
	// Size is the size of the whole message in bytes, for the ledger.
	Size int
	// Reply is where the answer goes.
	Reply      ReplyTarget
	ReceivedAt time.Time
}

// Rejection is returned by an adapter for a message it will not pass on. The
// message it returns alongside carries the identity fields (MessageID,
// Sender.ID, Size) so the core can record the refusal.
type Rejection struct {
	// Reason is a short machine-readable code (no message content).
	Reason string
	// Ignore marks a message nobody should be told about (an automatic reply, a
	// loop) as opposed to one refused by policy.
	Ignore bool
}

func (r *Rejection) Error() string { return "message not accepted: " + r.Reason }

// Result is what is sent back.
type Result struct {
	// Text is the agent's answer. Empty when Failed.
	Text string
	// Failed: the run did not produce an answer. The sender is told so in
	// general terms; Reason is for the logs and never reaches the sender.
	Failed bool
	Reason string
	// Notice is set instead of Text for answers the core writes itself.
	Notice bool
	// Subject is the title of the conversation the reply belongs to, if known.
	Subject string
}

// Secret is the data of a connection's Secret. It never prints.
type Secret map[string][]byte

func (Secret) String() string   { return "[secret]" }
func (Secret) GoString() string { return "[secret]" }

// Get returns the value of a key as a string.
func (s Secret) Get(k string) string { return string(s[k]) }

// Connection is a stored connection with its Secret read.
type Connection struct {
	ID        string
	TenantID  string
	Kind      string
	Name      string
	Namespace string
	Agent     string
	// Settings are the adapter-independent settings; Config is the adapter's own
	// (the "adapter" object of the stored config).
	Settings Settings
	Config   json.RawMessage
	Secret   Secret
}

// Adapter speaks one channel's protocol.
type Adapter interface {
	// Kind is the adapter's identifier, as stored in a connection.
	Kind() string
	Capabilities() Capabilities
	// ValidateConfig checks the adapter's settings when a connection is saved.
	ValidateConfig(cfg json.RawMessage) error
	// Reply delivers the result to the target. The core has already truncated,
	// redacted and (for failures) worded it.
	Reply(ctx context.Context, c Connection, to ReplyTarget, res Result) error
}

// Receiver is an adapter that is called by the channel (a signed HTTP request).
type Receiver interface {
	Adapter
	// Receive verifies the request and returns the message it carries. A bad
	// signature is an *AuthError. A message to refuse is a *Rejection, returned
	// with the identity fields filled.
	Receive(ctx context.Context, c Connection, r *http.Request) (Message, error)
}

// Response is an answer an adapter wants written to the channel's own HTTP
// request instead of the default status: a verification challenge, or the
// immediate text of a slash command. Returned as the error of Receive it ends the
// request without reaching the pipeline; returned by an Acknowledger it replaces
// the default body.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
}

func (r *Response) Error() string { return "answered directly" }

// Acknowledger is a Receiver that shapes what the channel's request is answered
// with once the message is recorded (a slash command shows the sender a line at
// once). nil means the default.
type Acknowledger interface {
	AckResponse(m Message, o Outcome) *Response
}

// Enricher is a Receiver that completes a message with a lookup that must not
// hold up the answer to the platform (the thread a post belongs to). It runs in
// the same background step as the pipeline. A failure is not fatal: the adapter
// leaves the message as it was.
type Enricher interface {
	Enrich(ctx context.Context, c Connection, m *Message)
}

// Indicator is an adapter that can show that work has begun (a reaction, a typing
// hint). Called once after the run started, best effort, bounded in time.
type Indicator interface {
	Working(ctx context.Context, c Connection, to ReplyTarget) error
}

// Handler takes a message into the pipeline.
type Handler interface {
	// Handle records and routes one message. A nil error means the message is
	// safely recorded (whatever became of it), so the adapter may mark it done in
	// the channel. recvErr is the error Receive-like code returned with the message.
	Handle(ctx context.Context, c Connection, m Message, recvErr error) (Outcome, error)
}

// Poller is an adapter that fetches messages itself.
type Poller interface {
	Adapter
	// PollOnce connects, hands every new message to h, marks each one done in the
	// channel only after h returned nil for it, and disconnects.
	PollOnce(ctx context.Context, c Connection, h Handler) error
}

// TestReport is what a connection test found. It never holds message content.
type TestReport struct {
	OK      bool              `json:"ok"`
	Details map[string]string `json:"details,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// Tester is an adapter that can check a connection without sending anything.
type Tester interface {
	Adapter
	Test(ctx context.Context, c Connection) TestReport
}

// AuthError is a request the adapter could not authenticate.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// Outcome is what became of a message.
type Outcome string

// Outcomes, also the "outcome" label of agentorca_inbound_messages_total.
const (
	OutcomeStarted Outcome = "started"
	// OutcomeAccepted: acknowledged to the platform before the pipeline finished
	// (ack-first); the outcome of the message is recorded by the pipeline.
	OutcomeAccepted  Outcome = "accepted"
	OutcomeDuplicate Outcome = "duplicate"
	OutcomeRejected  Outcome = "rejected"
	OutcomeIgnored   Outcome = "ignored"
	OutcomeThrottled Outcome = "throttled"
	OutcomeFailed    Outcome = "failed"
	OutcomeReplied   Outcome = "replied"
	// OutcomeReplyFailed: the run finished but the answer could not be delivered.
	OutcomeReplyFailed Outcome = "reply_failed"
)

// Registry maps kinds to adapters.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]Adapter{}} }

// Register adds an adapter; a second adapter of a kind is a programming error.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[a.Kind()]; dup {
		panic("inbound: adapter registered twice: " + a.Kind())
	}
	r.m[a.Kind()] = a
}

// Get returns the adapter of a kind.
func (r *Registry) Get(kind string) (Adapter, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.m[kind]
	return a, ok
}

// Kinds lists the registered kinds.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	return out
}

// ErrNotReceiving is returned by a Receiver for a connection that does not take
// requests (an email connection that polls a mailbox): the endpoint answers 404.
var ErrNotReceiving = errors.New("this connection does not take requests")

// PollSelector is implemented by a Poller that polls only for some settings (an
// adapter with a polling mode and a push mode): the runner starts no poller for
// the rest.
type PollSelector interface {
	Polls(adapterConfig json.RawMessage) bool
}

// ErrUnknownKind: no adapter is registered for a connection's kind.
var ErrUnknownKind = errors.New("no adapter for this kind of connection")

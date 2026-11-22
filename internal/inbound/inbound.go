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
	// MaxReplyChars caps a reply, in characters. Zero means no cap.
	MaxReplyChars int
}

// Sender is who wrote a message, as the adapter established it.
type Sender struct {
	// ID is the normalised identity the allow-list is matched against and the
	// reply cap is counted by: an address, a user id. Required.
	ID string
	// Group is the larger body the sender belongs to (an email domain, a
	// workspace). The allow-list can admit a whole group. May be empty.
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
	ThreadID string
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
	OutcomeStarted   Outcome = "started"
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

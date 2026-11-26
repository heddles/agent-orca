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

package inbound

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Settings are the adapter-independent settings of a connection, stored in the
// connection's config next to the adapter's own ("adapter").
type Settings struct {
	// AllowSenders are the exact sender ids that may start a run.
	AllowSenders []string `json:"allowSenders,omitempty"`
	// AllowGroups admit every sender of a group (an email domain).
	AllowGroups []string `json:"allowGroups,omitempty"`
	// AllowChannels are the exact ids of the spaces (channels, rooms) a chat
	// message may come from. When present a message from any other space is
	// refused, whoever wrote it. With no sender or group on the list it admits
	// everyone who can write in those spaces.
	AllowChannels []string `json:"allowChannels,omitempty"`
	// Approvers are the exact sender ids that may approve or deny a tool call of
	// a run of this connection by replying in the thread. They must also be
	// admitted by the allow-list. Empty: approvals are decided in the console.
	Approvers []string `json:"approvers,omitempty"`
	// Modes are the ways of addressing the agent this connection answers (chat).
	// Empty means the adapter's modes except ModeChannel, which must be named.
	Modes []Mode `json:"modes,omitempty"`
	// RatePerMin caps the messages a connection accepts per minute (default 30).
	RatePerMin int `json:"ratePerMin,omitempty"`
	// SenderPerHour caps the runs one sender can start through one connection per
	// hour (default 10), so two automated senders cannot answer each other forever.
	SenderPerHour int `json:"senderPerHour,omitempty"`
	// PollSeconds is how often a polling adapter looks for messages (default 60, at least 30).
	PollSeconds int `json:"pollSeconds,omitempty"`
	// NewThreadPerMessage turns threading off: every message starts a new run.
	NewThreadPerMessage bool `json:"newThreadPerMessage,omitempty"`
	// Adapter is the adapter's own settings.
	Adapter json.RawMessage `json:"adapter,omitempty"`
}

// refusedSender is the ledger reason for a sender who is not allowed.
const refusedSender = "sender_not_allowed"

// Defaults and bounds.
const (
	DefaultRatePerMin    = 30
	MaxRatePerMin        = 600
	DefaultSenderPerHour = 10
	MaxSenderPerHour     = 120
	DefaultPollSeconds   = 60
	MinPollSeconds       = 30
	MaxPollSeconds       = 3600
	maxAllowEntries      = 200
)

// ParseSettings reads and validates a stored config. The allow-list is
// required: an empty one admits nobody (default deny).
func ParseSettings(raw json.RawMessage) (Settings, error) {
	var s Settings
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("config is not valid: %w", err)
		}
	}
	if len(s.AllowSenders)+len(s.AllowGroups)+len(s.AllowChannels) > maxAllowEntries {
		return s, fmt.Errorf("the allow-list has at most %d entries", maxAllowEntries)
	}
	for i, v := range s.AllowSenders {
		s.AllowSenders[i] = normalize(v)
		if s.AllowSenders[i] == "" || strings.ContainsAny(s.AllowSenders[i], "*?, \t\r\n") {
			return s, fmt.Errorf("allowSenders holds exact ids only: %q is not one", v)
		}
	}
	for i, v := range s.AllowGroups {
		s.AllowGroups[i] = normalize(strings.TrimPrefix(strings.TrimSpace(v), "@"))
		if s.AllowGroups[i] == "" || strings.ContainsAny(s.AllowGroups[i], "*?, \t\r\n") {
			return s, fmt.Errorf("allowGroups holds exact names only (no wildcards): %q is not one", v)
		}
	}
	for i, v := range s.AllowChannels {
		s.AllowChannels[i] = normalize(strings.TrimPrefix(strings.TrimSpace(v), "#"))
		if s.AllowChannels[i] == "" || strings.ContainsAny(s.AllowChannels[i], "*?, \t\r\n") {
			return s, fmt.Errorf("allowChannels holds exact ids only (no wildcards): %q is not one", v)
		}
	}
	if len(s.Approvers) > maxAllowEntries {
		return s, fmt.Errorf("approvers has at most %d entries", maxAllowEntries)
	}
	for i, v := range s.Approvers {
		s.Approvers[i] = normalize(v)
		if s.Approvers[i] == "" || strings.ContainsAny(s.Approvers[i], "*?, \t\r\n") {
			return s, fmt.Errorf("approvers holds exact ids only: %q is not one", v)
		}
	}
	for _, m := range s.Modes {
		if !KnownMode(m) {
			return s, fmt.Errorf("modes: %q is not one of direct, mention, command, channel", m)
		}
	}
	if s.RatePerMin == 0 {
		s.RatePerMin = DefaultRatePerMin
	}
	if s.RatePerMin < 1 || s.RatePerMin > MaxRatePerMin {
		return s, fmt.Errorf("ratePerMin must be between 1 and %d", MaxRatePerMin)
	}
	if s.SenderPerHour == 0 {
		s.SenderPerHour = DefaultSenderPerHour
	}
	if s.SenderPerHour < 1 || s.SenderPerHour > MaxSenderPerHour {
		return s, fmt.Errorf("senderPerHour must be between 1 and %d", MaxSenderPerHour)
	}
	if s.PollSeconds == 0 {
		s.PollSeconds = DefaultPollSeconds
	}
	if s.PollSeconds < MinPollSeconds || s.PollSeconds > MaxPollSeconds {
		return s, fmt.Errorf("pollSeconds must be between %d and %d", MinPollSeconds, MaxPollSeconds)
	}
	return s, nil
}

// CanApprove reports whether the sender is one of the connection's approvers.
func (s Settings) CanApprove(from Sender) bool {
	id := normalize(from.ID)
	return id != "" && slices.Contains(s.Approvers, id)
}

// PollEvery is the polling interval.
func (s Settings) PollEvery() time.Duration { return time.Duration(s.PollSeconds) * time.Second }

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Allowed reports whether the sender may start a run: an exact id on the list,
// or a member of an admitted group. An empty list admits nobody.
func (s Settings) Allowed(from Sender) bool {
	id, group := normalize(from.ID), normalize(from.Group)
	if id == "" {
		return false
	}
	for _, a := range s.AllowSenders {
		if normalize(a) == id {
			return true
		}
	}
	if group != "" {
		for _, g := range s.AllowGroups {
			if normalize(strings.TrimPrefix(g, "@")) == group {
				return true
			}
		}
	}
	return false
}

// modeOn reports whether a connection answers messages addressed in mode m: the
// modes it lists, or by default those of the adapter except ModeChannel.
func (s Settings) modeOn(caps Capabilities, m Mode) bool {
	if len(s.Modes) > 0 {
		return slices.Contains(s.Modes, m)
	}
	if m == ModeChannel {
		return false
	}
	return slices.Contains(caps.Modes, m)
}

// ModeEnabled is modeOn for adapters: whether the connection answers m.
func (s Settings) ModeEnabled(caps Capabilities, m Mode) bool { return s.modeOn(caps, m) }

// Refusal is why a chat message is not admitted, as a ledger reason code; empty
// when it is. The sender lists are an OR (an id or a member of a group, when
// either is set); the channel list is an AND on top of them.
func (s Settings) Refusal(caps Capabilities, m Message) string {
	if len(caps.Modes) > 0 && !s.modeOn(caps, m.Mode) {
		return "mode_not_enabled"
	}
	if len(s.AllowSenders)+len(s.AllowGroups) > 0 && !s.Allowed(m.Sender) {
		return refusedSender
	}
	if len(s.AllowSenders)+len(s.AllowGroups) == 0 && len(s.AllowChannels) == 0 {
		return refusedSender
	}
	if len(s.AllowChannels) > 0 {
		ch := normalize(m.Channel)
		ok := false
		for _, a := range s.AllowChannels {
			if ch != "" && a == ch {
				ok = true
			}
		}
		if !ok {
			return "channel_not_allowed"
		}
	}
	return ""
}

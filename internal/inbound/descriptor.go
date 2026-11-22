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

import "fmt"

// Descriptor is how an adapter describes itself to the API and the UI: the
// settings it takes, the keys its Secret needs and the steps to set it up, so
// the Channels form is drawn from it and a new adapter needs no UI work.
type Descriptor struct {
	// Title is the human name of the channel ("Email", the platform's name).
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	// Fields are the adapter's own settings (the "adapter" object of a connection).
	Fields []Field `json:"fields,omitempty"`
	// SecretKeys are the keys of the Kubernetes Secret an administrator prepares.
	SecretKeys []SecretKey `json:"secretKeys,omitempty"`
	// Allow explains, in the platform's words, what the three allow-lists hold
	// (a list with no text is not offered for this channel).
	Allow AllowHelp `json:"allow,omitempty"`
	// Setup is the list of steps to do on the platform and in the cluster. The
	// text may hold {{url}}, which the UI replaces with the connection's receive URL.
	Setup []string `json:"setup,omitempty"`
}

// AllowHelp are the help texts of the allow-lists.
type AllowHelp struct {
	Users    string `json:"users,omitempty"`
	Groups   string `json:"groups,omitempty"`
	Channels string `json:"channels,omitempty"`
}

// Field is one setting of an adapter.
type Field struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // string, bool, int, enum
	Required    bool     `json:"required,omitempty"`
	Help        string   `json:"help,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Options     []string `json:"options,omitempty"` // enum
	Default     any      `json:"default,omitempty"`
}

// SecretKey is a key of the connection's Secret.
type SecretKey struct {
	Key      string `json:"key"`
	Required bool   `json:"required,omitempty"`
	Help     string `json:"help,omitempty"`
}

// Describer is an adapter that describes itself. Every chat adapter should.
type Describer interface {
	Describe() Descriptor
}

// KindInfo is what the API lists for a registered kind.
type KindInfo struct {
	Kind         string           `json:"kind"`
	Descriptor   Descriptor       `json:"descriptor"`
	Capabilities KindCapabilities `json:"capabilities"`
	Testable     bool             `json:"testable"`
	Receives     bool             `json:"receives"`
}

// KindCapabilities is Capabilities as the API shows it.
type KindCapabilities struct {
	Threads          bool    `json:"threads"`
	Dialect          Dialect `json:"dialect,omitempty"`
	MaxReplyChars    int     `json:"maxReplyChars,omitempty"`
	MaxReplyParts    int     `json:"maxReplyParts,omitempty"`
	Modes            []Mode  `json:"modes,omitempty"`
	AckFirst         bool    `json:"ackFirst,omitempty"`
	WorkingIndicator bool    `json:"workingIndicator,omitempty"`
}

// Info describes one adapter.
func Info(a Adapter) KindInfo {
	c := a.Capabilities()
	k := KindInfo{
		Kind: a.Kind(),
		Capabilities: KindCapabilities{
			Threads: c.Threads, Dialect: c.Dialect, MaxReplyChars: c.MaxReplyChars, MaxReplyParts: c.MaxReplyParts,
			Modes: c.Modes, AckFirst: c.AckFirst, WorkingIndicator: c.WorkingIndicator,
		},
	}
	if d, ok := a.(Describer); ok {
		k.Descriptor = d.Describe()
	}
	if k.Descriptor.Title == "" {
		k.Descriptor.Title = a.Kind()
	}
	_, k.Testable = a.(Tester)
	_, k.Receives = a.(Receiver)
	return k
}

// CheckSecret reports the required keys of the descriptor that the Secret lacks.
// It names keys, never values.
func CheckSecret(a Adapter, s map[string][]byte) error {
	d, ok := a.(Describer)
	if !ok {
		return nil
	}
	for _, k := range d.Describe().SecretKeys {
		if k.Required && len(s[k.Key]) == 0 {
			return fmt.Errorf("the Secret has no %q key", k.Key)
		}
	}
	return nil
}

// CheckSettings is the part of validating a connection that needs both the
// adapter-independent settings and the adapter: the chat allow-lists and modes
// only make sense for an adapter that has modes, and a channel without them
// needs a sender or group on the list.
func CheckSettings(a Adapter, s Settings) error {
	caps := a.Capabilities()
	if len(caps.Modes) == 0 {
		if len(s.AllowChannels) > 0 || len(s.Modes) > 0 {
			return fmt.Errorf("allowChannels and modes are for chat channels; %s has none", a.Kind())
		}
		if len(s.AllowSenders)+len(s.AllowGroups) == 0 {
			return fmt.Errorf("allowSenders or allowGroups is required: a connection admits nobody by default")
		}
		return nil
	}
	have := map[Mode]bool{}
	for _, m := range caps.Modes {
		have[m] = true
	}
	for _, m := range s.Modes {
		if !have[m] {
			return fmt.Errorf("mode %q is not supported by %s", m, a.Kind())
		}
	}
	if s.modeOn(caps, ModeChannel) && len(s.AllowChannels) == 0 {
		return fmt.Errorf("the %q mode needs allowChannels: the agent answers every message of the spaces you list, so list them", ModeChannel)
	}
	return nil
}

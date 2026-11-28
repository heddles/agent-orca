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
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
)

func testRoutes() []postgresql.InboundRoute {
	place := func(id string, def bool) postgresql.InboundPlace {
		return postgresql.InboundPlace{ID: id, Default: def}
	}
	return []postgresql.InboundRoute{
		{Namespace: "a", Agent: "ops-lead", Places: []postgresql.InboundPlace{place("C1", true), place("C2", false)}},
		{Namespace: "a", Agent: "net-chat", Places: []postgresql.InboundPlace{place("C2", true), place("C1", false), place("", true)}},
	}
}

func TestRoute(t *testing.T) {
	none := func() (string, string) { return "", "" }
	thread := func(agent string) func() (string, string) { return func() (string, string) { return "a", agent } }
	for _, tc := range []struct {
		name, channel, text string
		mode                Mode
		thread              func() (string, string)
		agent, rest         string
	}{
		{"the default of the channel", "c1", "why is it slow", ModeMention, none, "ops-lead", "why is it slow"},
		{"each channel has its own default", "C2", "why", ModeMention, none, "net-chat", "why"},
		{"by name", "C1", "net-chat why is it slow", ModeMention, none, "net-chat", "why is it slow"},
		{"by name with punctuation and case", "C1", "  Net-Chat:  why", ModeMention, none, "net-chat", "why"},
		{"a thread stays with its agent", "C1", "and now?", ModeMention, thread("net-chat"), "net-chat", "and now?"},
		{"a name beats the thread", "C1", "ops-lead, look", ModeMention, thread("net-chat"), "ops-lead", "look"},
		{"a thread of an agent no longer here falls back", "C1", "hm", ModeMention, thread("gone"), "ops-lead", "hm"},
		{"the name alone is not an address", "C1", "net-chat", ModeMention, none, "ops-lead", "net-chat"},
		{"a longer word is not the name", "C1", "net-chatty ping", ModeMention, none, "ops-lead", "net-chatty ping"},
		{"direct messages", "D9", "hi", ModeDirect, none, "net-chat", "hi"},
		{"a name does not reach an agent that is not in the place", "D9", "ops-lead hi", ModeDirect, none, "net-chat", "ops-lead hi"},
		{"nobody in that channel", "C3", "net-chat hi", ModeMention, none, "", "net-chat hi"},
		{"no channel at all", "", "hi", ModeMention, none, "", "hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Channel: tc.channel, Text: tc.text, Mode: tc.mode}
			r, rest, by := route(candidates(testRoutes(), m), m, tc.thread)
			ok := by != routedNowhere
			if r.Agent != tc.agent || rest != tc.rest || ok != (tc.agent != "") {
				t.Errorf("route = %q %q %v, want %q %q", r.Agent, rest, ok, tc.agent, tc.rest)
			}
		})
	}
}

func TestASharedConnectionTakesTheRoutesIdentityAndLists(t *testing.T) {
	c := Connection{Settings: Settings{AllowSenders: []string{"boss"}, AllowGroups: []string{"T1"}, AllowChannels: []string{"c9"}, Approvers: []string{"boss"}}}
	got := c.as(postgresql.InboundRoute{Namespace: "a", Agent: "x", AllowSenders: []string{"U1"}})
	if got.Agent != "x" || got.Namespace != "a" || len(got.Settings.AllowGroups)+len(got.Settings.AllowChannels)+len(got.Settings.Approvers) != 0 {
		t.Fatalf("as = %+v", got)
	}
	// Only the route's people: the app's own lists admit and approve nobody.
	if got.Settings.Allowed(Sender{ID: "boss", Group: "T1"}) || !got.Settings.Allowed(Sender{ID: "u1"}) || got.Settings.CanApprove(Sender{ID: "boss"}) {
		t.Error("the app's lists leaked into the route")
	}
}

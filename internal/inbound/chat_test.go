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
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// chatAdapter is a made-up chat platform: it has modes and a working indicator.
type chatAdapter struct {
	fakeAdapter
	mu2     sync.Mutex
	working int
}

func (c *chatAdapter) Working(context.Context, Connection, ReplyTarget) error {
	c.mu2.Lock()
	defer c.mu2.Unlock()
	c.working++
	return nil
}

func chatFixture(t *testing.T, settings string) (*fixture, *chatAdapter) {
	t.Helper()
	f := &fixture{t: t, st: newFakeStore(), l: &fakeLauncher{}, r: &fakeRuns{}}
	ca := &chatAdapter{fakeAdapter: fakeAdapter{caps: Capabilities{
		Threads: true, MaxReplyChars: 100, MaxReplyParts: 3, Dialect: DialectMarkdown, AckFirst: true, WorkingIndicator: true,
		Modes: []Mode{ModeDirect, ModeMention, ModeCommand, ModeChannel},
	}}}
	f.ad = &ca.fakeAdapter
	reg := NewRegistry()
	reg.Register(&chatKind{ca})
	f.p = &Pipeline{Store: f.st, Launcher: f.l, Runs: f.r, Registry: reg}
	s, err := ParseSettings([]byte(settings))
	if err != nil {
		t.Fatal(err)
	}
	f.c = Connection{ID: "inb_" + strings.Repeat("b", 32), TenantID: "t-1", Kind: "fake", Namespace: "ns", Agent: "support", Settings: s}
	return f, ca
}

// chatKind makes chatAdapter satisfy Indicator while reporting kind "fake".
type chatKind struct{ *chatAdapter }

func (c *chatKind) Kind() string { return "fake" }

func chatMsg(id, user, team, channel string, mode Mode) Message {
	return Message{
		Sender: Sender{ID: user, Group: team, Display: "Someone"}, Text: "hello", MessageID: id, Channel: channel, Mode: mode,
		ThreadID: team + "/" + channel + "/t1", Size: 10, Reply: ReplyTarget(`{"c":"1"}`),
	}
}

func TestChat_AllowListsAreDefaultDenyAndExact(t *testing.T) {
	// A chat channel may be saved before anyone is paired to it. It admits nobody.
	empty, err := ParseSettings([]byte(`{"modes":["direct"]}`))
	if err != nil {
		t.Fatal(err)
	}
	chatCaps := Capabilities{Modes: []Mode{ModeDirect}}
	if why := empty.Refusal(chatCaps, chatMsg("0", "U1", "T1", "C1", ModeDirect)); why != "sender_not_allowed" {
		t.Errorf("an empty allow-list gave %q, want sender_not_allowed", why)
	}
	for _, bad := range []string{`{"allowChannels":["*"]}`, `{"allowChannels":["C1,C2"]}`, `{"allowSenders":["U1"],"modes":["shout"]}`} {
		if _, err := ParseSettings([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	f, _ := chatFixture(t, `{"allowSenders":["U1"],"allowGroups":["T1"],"allowChannels":["C1"]}`)
	for name, tc := range map[string]struct {
		m    Message
		want Outcome
	}{
		"allowed user in allowed channel": {chatMsg("1", "U1", "T9", "C1", ModeMention), OutcomeStarted},
		"allowed workspace, other user":   {chatMsg("2", "U7", "T1", "C1", ModeMention), OutcomeStarted},
		"allowed user, other channel":     {chatMsg("3", "U1", "T1", "C2", ModeMention), OutcomeRejected},
		"stranger in an allowed channel":  {chatMsg("4", "U7", "T9", "C1", ModeMention), OutcomeRejected},
		"look-alike workspace":            {chatMsg("5", "U7", "T11", "C1", ModeMention), OutcomeRejected},
		"allowed user, no channel known":  {chatMsg("6", "U1", "T1", "", ModeMention), OutcomeRejected},
		"case of ids does not matter":     {chatMsg("7", "u1", "T1", "c1", ModeDirect), OutcomeStarted},
	} {
		if got := f.handle(tc.m, nil); got != tc.want {
			t.Errorf("%s = %v, want %v", name, got, tc.want)
		}
	}
	// A channel-only list admits whoever writes in that channel.
	f, _ = chatFixture(t, `{"allowChannels":["C1"]}`)
	if o := f.handle(chatMsg("1", "anyone", "T1", "C1", ModeMention), nil); o != OutcomeStarted {
		t.Errorf("channel-only list: %v", o)
	}
	if o := f.handle(chatMsg("2", "anyone", "T1", "C2", ModeMention), nil); o != OutcomeRejected {
		t.Errorf("channel-only list, other channel: %v", o)
	}
}

func TestChat_ModesAndBots(t *testing.T) {
	f, _ := chatFixture(t, `{"allowSenders":["U1"]}`)
	// By default direct, mention and command are on; plain channel messages are not.
	for id, tc := range map[string]struct {
		mode Mode
		want Outcome
	}{"d": {ModeDirect, OutcomeStarted}, "m": {ModeMention, OutcomeStarted}, "c": {ModeCommand, OutcomeStarted}, "ch": {ModeChannel, OutcomeRejected}} {
		if got := f.handle(chatMsg(id, "U1", "T1", "C1", tc.mode), nil); got != tc.want {
			t.Errorf("mode %s = %v, want %v", tc.mode, got, tc.want)
		}
	}
	if reason := f.st.find(f.c.ID, keyOf(f.c, "ch")).Reason; reason != "mode_not_enabled" {
		t.Errorf("reason = %q", reason)
	}
	// A connection that names its modes gets exactly those.
	f, _ = chatFixture(t, `{"allowSenders":["U1"],"modes":["direct"]}`)
	if o := f.handle(chatMsg("m", "U1", "T1", "C1", ModeMention), nil); o != OutcomeRejected {
		t.Errorf("mention on a direct-only connection = %v", o)
	}
	// Bots, apps and our own bot: ignored, never a run, even when allow-listed.
	f, _ = chatFixture(t, `{"allowSenders":["B1","U1"]}`)
	bot := chatMsg("b", "B1", "T1", "C1", ModeMention)
	bot.FromBot = true
	if o := f.handle(bot, nil); o != OutcomeIgnored || len(f.l.reqs) != 0 {
		t.Errorf("bot message = %v, runs %d", o, len(f.l.reqs))
	}
	if reason := f.st.find(f.c.ID, keyOf(f.c, "b")).Reason; reason != "bot_message" {
		t.Errorf("reason = %q", reason)
	}
}

func keyOf(c Connection, id string) string { return postgresql.HashKey(c.ID, id) }

func TestChat_CheckSettingsNeedsAdapterContext(t *testing.T) {
	chat := &chatKind{&chatAdapter{fakeAdapter: fakeAdapter{caps: Capabilities{Modes: []Mode{ModeDirect, ModeMention}}}}}
	mail := &fakeAdapter{}
	parse := func(j string) Settings {
		s, err := ParseSettings([]byte(j))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err := CheckSettings(mail, parse(`{"allowChannels":["C1"]}`)); err == nil {
		t.Error("channels accepted for a channel without spaces")
	}
	if err := CheckSettings(mail, parse(`{"allowSenders":["a@b.c"]}`)); err != nil {
		t.Errorf("mail: %v", err)
	}
	if err := CheckSettings(chat, parse(`{"allowSenders":["U1"],"modes":["command"]}`)); err == nil {
		t.Error("a mode the adapter lacks was accepted")
	}
	if err := CheckSettings(chat, parse(`{"allowSenders":["U1"],"modes":["channel"]}`)); err == nil {
		t.Error("channel mode without channels was accepted")
	}
	if err := CheckSettings(chat, parse(`{"allowChannels":["C1"],"modes":["mention"]}`)); err != nil {
		t.Errorf("chat: %v", err)
	}
}

func TestChat_WorkingIndicatorAfterStart(t *testing.T) {
	f, ca := chatFixture(t, `{"allowSenders":["U1"]}`)
	f.handle(chatMsg("1", "U1", "T1", "C1", ModeMention), nil)
	f.handle(chatMsg("2", "U2", "T1", "C1", ModeMention), nil) // rejected: no indicator
	if ca.working != 1 {
		t.Errorf("indicator shown %d times, want 1", ca.working)
	}
}

func TestChat_ThreadsAreScopedByTheAdapterId(t *testing.T) {
	f, _ := chatFixture(t, `{"allowSenders":["U1"]}`)
	f.handle(chatMsg("1", "U1", "T1", "C1", ModeMention), nil)
	other := chatMsg("2", "U1", "T1", "C2", ModeMention) // same platform thread id, another space
	f.handle(other, nil)
	if f.l.reqs[1].PriorRun != "" {
		t.Errorf("a message in another space continued %q", f.l.reqs[1].PriorRun)
	}
	f.handle(chatMsg("3", "U1", "T1", "C1", ModeMention), nil)
	if f.l.reqs[2].PriorRun == "" {
		t.Error("a reply in the same thread did not continue")
	}
}

func TestChat_RepliesAreCutAtPartsTimesLimit(t *testing.T) {
	f, _ := chatFixture(t, `{"allowSenders":["U1"]}`)
	c := f.c
	res := f.p.Prepare(c, Result{Text: strings.Repeat("x", 1000)})
	if n := len([]rune(res.Text)); n != 300 {
		t.Errorf("prepared %d characters, want 3 parts of 100", n)
	}
}

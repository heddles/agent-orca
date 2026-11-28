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
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
)

type fakeRoutes []postgresql.InboundRoute

func (f fakeRoutes) ListInboundRoutes(context.Context, string) ([]postgresql.InboundRoute, error) {
	return f, nil
}

type fakePicker struct {
	asked []PickRequest
	agent string
	err   error
}

func (f *fakePicker) Pick(_ context.Context, r PickRequest) (Picked, error) {
	f.asked = append(f.asked, r)
	return Picked{Agent: AgentRef{Namespace: "ns", Name: f.agent}, Routing: `{"method":"decided"}`}, f.err
}

// autoFixture is a shared app where billing (the default), deploys and net
// answer in C1, which picks automatically, and in C2, which does not. ann may
// ask all three, bob only deploys and net, cat only net.
func autoFixture(t *testing.T) (*fixture, *fakePicker) {
	t.Helper()
	f := newFixture(t)
	route := func(agent string, def bool, senders ...string) postgresql.InboundRoute {
		return postgresql.InboundRoute{Namespace: "ns", Agent: agent, AllowSenders: senders, Places: []postgresql.InboundPlace{
			{ID: "C1", Default: def}, {ID: "C2", Default: def},
		}}
	}
	f.p.Routes = fakeRoutes{
		route("billing", true, "ann"),
		route("deploys", false, "ann", "bob"),
		route("net", false, "ann", "bob", "cat"),
	}
	picker := &fakePicker{agent: "deploys"}
	f.p.Picker = picker
	settings, err := ParseSettings([]byte(`{"allowSenders":["admin"],"autoPlaces":["c1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	f.c.Namespace, f.c.Agent, f.c.Settings = "", "", settings
	return f, picker
}

func placeMsg(id, from, channel, text string) Message {
	m := msg(id, from, "")
	m.Channel, m.Mode, m.Text, m.ThreadID = channel, ModeMention, text, "thread-"+id
	return m
}

func names(refs []AgentRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func TestAutomaticPlacePicksAmongWhatTheSenderMayAsk(t *testing.T) {
	f, picker := autoFixture(t)
	if got := f.handle(placeMsg("m1", "bob", "C1", "roll it back"), nil); got != OutcomeStarted {
		t.Fatalf("outcome = %s", got)
	}
	if len(picker.asked) != 1 || !slices.Equal(names(picker.asked[0].Among), []string{"deploys", "net"}) {
		t.Fatalf("asked %+v", picker.asked)
	}
	// billing is the place's default, but bob may not ask it.
	if picker.asked[0].Default != nil || picker.asked[0].Text != "roll it back" {
		t.Errorf("asked %+v", picker.asked[0])
	}
	if run := f.l.reqs[0]; run.Agent != "deploys" || run.Routing != `{"method":"decided"}` {
		t.Errorf("launched %+v", run)
	}
}

func TestNothingIsPickedWhenThereIsNothingToPick(t *testing.T) {
	for name, tc := range map[string]struct {
		from, channel, text string
		agent               string // "" means rejected
	}{
		"the message names an agent":      {"ann", "C1", "net why is it slow", "net"},
		"the place is not automatic":      {"ann", "C2", "roll it back", "billing"},
		"the sender may ask only one":     {"cat", "C1", "roll it back", "net"},
		"the sender may ask none":         {"eve", "C1", "roll it back", ""},
		"no agent answers in the channel": {"ann", "C9", "roll it back", ""},
	} {
		t.Run(name, func(t *testing.T) {
			f, picker := autoFixture(t)
			outcome := f.handle(placeMsg("m1", tc.from, tc.channel, tc.text), nil)
			if len(picker.asked) != 0 {
				t.Fatalf("picker asked: %+v", picker.asked)
			}
			switch {
			case tc.agent == "":
				if outcome != OutcomeRejected || len(f.l.reqs) != 0 || len(f.ad.replies) != 0 {
					t.Fatalf("outcome %s, runs %+v, replies %+v", outcome, f.l.reqs, f.ad.replies)
				}
			case outcome != OutcomeStarted || f.l.reqs[0].Agent != tc.agent || f.l.reqs[0].Routing != "":
				t.Fatalf("outcome %s, runs %+v", outcome, f.l.reqs)
			}
		})
	}
}

func TestAThreadKeepsItsAgentInAnAutomaticPlace(t *testing.T) {
	f, picker := autoFixture(t)
	first := placeMsg("m1", "ann", "C1", "roll it back")
	f.handle(first, nil)
	f.r.state = RunState{Found: true, Agent: "deploys"}
	next := placeMsg("m2", "ann", "C1", "and the invoices?")
	next.ThreadID = first.ThreadID
	f.handle(next, nil)
	if len(picker.asked) != 1 || len(f.l.reqs) != 2 || f.l.reqs[1].Agent != "deploys" || f.l.reqs[1].Routing != "" {
		t.Fatalf("asked %d times, runs %+v", len(picker.asked), f.l.reqs)
	}
}

// A decision can cost money, so a sender over a limit gets none.
func TestRateLimitsComeBeforeThePick(t *testing.T) {
	f, picker := autoFixture(t)
	f.p.Limiter = &limiter{n: 0}
	if got := f.handle(placeMsg("m1", "ann", "C1", "roll it back"), nil); got != OutcomeThrottled || len(picker.asked) != 0 {
		t.Fatalf("outcome %s after %d picks", got, len(picker.asked))
	}
}

func TestWhenNoAgentIsPicked(t *testing.T) {
	for name, tc := range map[string]struct {
		from   string
		err    error
		pick   string
		agent  string // "" means rejected
		notice string
	}{
		"routing is off: the default answers": {from: "ann", err: ErrNoPicker, agent: "billing"},
		"the picker fails: the default":       {from: "ann", err: errors.New("down"), agent: "billing"},
		"a pick that was not offered":         {from: "ann", pick: "payroll", agent: "billing"},
		"no default the sender may ask":       {from: "bob", err: ErrNoPicker, notice: "name of one: deploys, net."},
		"undecided":                           {from: "ann", err: ErrUndecided, notice: "name of one: billing, deploys, net."},
		"nothing to pick":                     {from: "ann", err: ErrNoneToPick},
	} {
		t.Run(name, func(t *testing.T) {
			f, picker := autoFixture(t)
			picker.err = tc.err
			if tc.pick != "" {
				picker.agent = tc.pick
			}
			outcome := f.handle(placeMsg("m1", tc.from, "C1", "roll it back"), nil)
			if tc.agent != "" {
				if outcome != OutcomeStarted || f.l.reqs[0].Agent != tc.agent {
					t.Fatalf("outcome %s, runs %+v", outcome, f.l.reqs)
				}
				return
			}
			if outcome != OutcomeRejected || len(f.l.reqs) != 0 {
				t.Fatalf("outcome %s, runs %+v", outcome, f.l.reqs)
			}
			sent := ""
			for _, r := range f.ad.replies {
				sent += r.Text
			}
			if tc.notice == "" && sent != "" || !strings.Contains(sent, tc.notice) {
				t.Errorf("replies = %q, want %q", sent, tc.notice)
			}
		})
	}
}

func TestWithoutAPickerTheDefaultAnswers(t *testing.T) {
	f, _ := autoFixture(t)
	f.p.Picker = nil
	if f.handle(placeMsg("m1", "ann", "C1", "roll it back"), nil); len(f.l.reqs) != 1 || f.l.reqs[0].Agent != "billing" {
		t.Fatalf("runs %+v", f.l.reqs)
	}
}

func TestTheAnswerSaysWhoWasPicked(t *testing.T) {
	f, _ := autoFixture(t)
	f.handle(placeMsg("m1", "ann", "C1", "roll it back"), nil)
	f.st.conns[f.c.ID] = postgresql.InboundConnection{ID: f.c.ID, TenantID: "t-1", Kind: "fake", Enabled: true,
		Config: []byte(`{"allowSenders":["admin"]}`)}
	f.r.state = RunState{Found: true, Done: true, Succeeded: true, Agent: "deploys", Picked: true, Output: "Rolled back."}
	f.p.DeliverPending(context.Background())
	want := "Picked deploys for this. To ask another agent, start your message with its name.\n\nRolled back."
	if len(f.ad.replies) != 1 || f.ad.replies[0].Text != want {
		t.Fatalf("replies = %+v", f.ad.replies)
	}
}

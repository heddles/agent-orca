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

package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/verify"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	chatxKind = "chatx"
	chanKind  = "chan"
)

// chatxAdapter is a made-up chat platform: a shared token in a header, modes,
// ack-first, a challenge, and an immediate line for commands.
type chatxAdapter struct{ enriched chan struct{} }

func (a *chatxAdapter) Kind() string { return chatxKind }
func (a *chatxAdapter) Capabilities() inbound.Capabilities {
	return inbound.Capabilities{
		Threads: true, AckFirst: true, Dialect: inbound.DialectMarkdown, MaxReplyChars: 100, MaxReplyParts: 2,
		Modes: []inbound.Mode{inbound.ModeDirect, inbound.ModeMention, inbound.ModeCommand},
	}
}
func (a *chatxAdapter) ValidateConfig(json.RawMessage) error { return nil }
func (a *chatxAdapter) Reply(context.Context, inbound.Connection, inbound.ReplyTarget, inbound.Result) error {
	return nil
}

func (a *chatxAdapter) Describe() inbound.Descriptor {
	return inbound.Descriptor{
		Title:      "Chat X",
		SecretKeys: []inbound.SecretKey{{Key: "token", Required: true}, {Key: "botToken", Required: true}},
		Setup:      []string{"Post to {{url}}"},
	}
}

func (a *chatxAdapter) Receive(_ context.Context, c inbound.Connection, r *http.Request) (inbound.Message, error) {
	if !verify.TokenEqual(r.Header.Get("X-Token"), c.Secret.Get("token")) {
		return inbound.Message{}, &inbound.AuthError{Msg: "invalid token"}
	}
	var p struct{ User, Team, Channel, ID, Text, Mode, Challenge string }
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return inbound.Message{}, err
	}
	if p.Challenge != "" {
		return inbound.Message{}, &inbound.Response{Status: 200, ContentType: "text/plain", Body: []byte(p.Challenge)}
	}
	return inbound.Message{
		Sender: inbound.Sender{ID: p.User, Group: p.Team}, Channel: p.Channel, Mode: inbound.Mode(p.Mode), MessageID: p.ID, Text: p.Text,
		ThreadID: p.Team + "/" + p.Channel, Size: len(p.Text), Reply: inbound.ReplyTarget(`{}`),
	}, nil
}

func (a *chatxAdapter) Enrich(_ context.Context, _ inbound.Connection, m *inbound.Message) {
	if a.enriched != nil {
		a.enriched <- struct{}{}
	}
	m.Title = "enriched"
}

func (a *chatxAdapter) AckResponse(m inbound.Message, o inbound.Outcome) *inbound.Response {
	if m.Mode == inbound.ModeCommand && o != inbound.OutcomeRejected {
		return &inbound.Response{Status: 200, ContentType: "application/json", Body: []byte(`{"text":"working"}`)}
	}
	return nil
}

// gatedStore holds back the first claim until released, to make the pipeline slow.
type gatedStore struct {
	*fakeInboundStore
	gate chan struct{}
}

func (g *gatedStore) ClaimInboundMessage(ctx context.Context, m postgresql.InboundMessage) (bool, error) {
	<-g.gate
	return g.fakeInboundStore.ClaimInboundMessage(ctx, m)
}

type chatFixtureT struct {
	*inboundFixture
	ad *chatxAdapter
}

func newChatFixture(t *testing.T) *chatFixtureT {
	t.Helper()
	f := newInboundFixture(t)
	ad := &chatxAdapter{}
	f.s.inboundPipe.Registry.Register(ad)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "chatx-creds", Namespace: "shared", Labels: map[string]string{inbound.SecretLabel: inbound.SecretLabelValue}},
		Data:       map[string][]byte{"token": []byte("shh-token"), "botToken": []byte("xb")},
	}
	if err := f.c.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	return &chatFixtureT{inboundFixture: f, ad: ad}
}

func (f *chatFixtureT) create(config string) *httptest.ResponseRecorder {
	return f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/inbound-connections",
		`{"namespace":"shared","agent":"support","name":"chat","kind":"chatx","secretRef":"chatx-creds","config":`+config+`}`)
}

func (f *chatFixtureT) post(v inboundView, token string, body map[string]string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, v.Path, strings.NewReader(string(b)))
	req.Header.Set("X-Token", token)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestInboundChat_KindsEndpointAndSettingsChecks(t *testing.T) {
	f := newChatFixture(t)
	rec := f.api(idFor(tenancy.RoleViewer, isoA), http.MethodGet, "/api/inbound-kinds", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("kinds = %d %s", rec.Code, rec.Body)
	}
	kinds := evalDecode[[]inbound.KindInfo](t, rec)
	var got *inbound.KindInfo
	for i := range kinds {
		if kinds[i].Kind == chatxKind {
			got = &kinds[i]
		}
	}
	if got == nil || got.Descriptor.Title != "Chat X" || !got.Capabilities.AckFirst || len(got.Capabilities.Modes) != 3 || !got.Receives || got.Descriptor.Setup[0] != "Post to {{url}}" {
		t.Fatalf("chatx = %+v", got)
	}
	for _, k := range kinds {
		if k.Kind == chanKind && (k.Descriptor.Title != chanKind || k.Capabilities.AckFirst) {
			t.Errorf("a kind without a descriptor: %+v", k)
		}
	}
	// A kind that does not describe itself is still listed; the viewer role is enough.
	for name, cfg := range map[string]string{
		"no allow-list at all":           `{"modes":["direct"]}`,
		"another mode the adapter lacks": `{"allowSenders":["U1"],"modes":["channel"]}`,
		"channel mode not offered":       `{"allowSenders":["U1"],"modes":["channel"]}`,
		"wildcard channel":               `{"allowChannels":["*"]}`,
	} {
		if rec := f.create(cfg); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	rec = f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/inbound-connections",
		`{"namespace":"shared","agent":"support","name":"mailish","kind":"chan","secretRef":"creds","config":{"allowSenders":["a"],"allowChannels":["C1"]}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "chat channels") {
		t.Errorf("channels on a channel without spaces = %d %s", rec.Code, rec.Body)
	}
}

func TestInboundChat_SecretMustHaveTheRequiredKeys(t *testing.T) {
	f := newChatFixture(t)
	// "creds" has only signingSecret: the descriptor of chatx wants token and botToken.
	rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/inbound-connections",
		`{"namespace":"shared","agent":"support","name":"chat","kind":"chatx","secretRef":"creds","config":{"allowSenders":["U1"]}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `\"token\"`) || strings.Contains(rec.Body.String(), inboundSecretValue) {
		t.Errorf("secret without keys = %d %s", rec.Code, rec.Body)
	}
}

func (f *chatFixtureT) createdChat() inboundView {
	f.t.Helper()
	rec := f.create(`{"allowSenders":["U1"],"allowChannels":["C1"]}`)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	return evalDecode[inboundView](f.t, rec)
}

func TestInboundChat_ChallengeAnswersTheRequestAndNeverReachesThePipeline(t *testing.T) {
	f := newChatFixture(t)
	v := f.createdChat()
	rec := f.post(v, "shh-token", map[string]string{"challenge": "abc123"})
	if rec.Code != 200 || rec.Body.String() != "abc123" || rec.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("challenge = %d %q", rec.Code, rec.Body)
	}
	if got := f.store.status(v.ID); len(got) != 0 {
		t.Errorf("a challenge was recorded: %v", got)
	}
	if rec := f.post(v, "wrong", map[string]string{"challenge": "abc123"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", rec.Code)
	}
}

func TestInboundChat_AckFirstRunsTheMessageWithNoInvoker(t *testing.T) {
	f := newChatFixture(t)
	v := f.createdChat()
	body := map[string]string{"user": "U1", "team": "T1", "channel": "C1", "id": "m1", "text": "do the thing", "mode": "mention"}
	rec := f.post(v, "shh-token", body)
	// A plain 200 (not 202): what a platform that retries on silence looks for.
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"started"`) {
		t.Fatalf("receive = %d %s", rec.Code, rec.Body)
	}
	runs := f.runs()
	if len(runs) != 1 || !strings.Contains(runs[0].Spec.Input, "do the thing") || !strings.Contains(runs[0].Spec.Input, "enriched") {
		t.Fatalf("runs = %+v", runs)
	}
	// Chat users are not agent-orca users: the run has no invoker, so invoker-mode servers fail closed.
	r := runs[0]
	if r.Annotations[AnnotationCreatedBy] != TriggerActorPrefix+v.ID || r.Labels[LabelInboundKind] != chatxKind {
		t.Errorf("run = %v %v", r.Labels, r.Annotations)
	}
	if user, why := invokerOf(&r); user != "" || why == "" {
		t.Errorf("invokerOf = %q, %q: a chat run must have no invoker", user, why)
	}
	// Retried by the platform: a duplicate, no second run.
	if rec := f.post(v, "shh-token", body); !strings.Contains(rec.Body.String(), "duplicate") || len(f.runs()) != 1 {
		t.Errorf("retry = %d %s, runs %d", rec.Code, rec.Body, len(f.runs()))
	}
	// A channel that is not on the list, and a user who is not: recorded and refused, still a 200.
	for id, b := range map[string]map[string]string{
		"m2": {"user": "U1", "team": "T1", "channel": "C9", "id": "m2", "text": "x", "mode": "mention"},
		"m3": {"user": "U9", "team": "T1", "channel": "C1", "id": "m3", "text": "x", "mode": "mention"},
		"m4": {"user": "U1", "team": "T1", "channel": "C1", "id": "m4", "text": "x", "mode": "channel"},
	} {
		if rec := f.post(v, "shh-token", b); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "rejected") {
			t.Errorf("%s = %d %s", id, rec.Code, rec.Body)
		}
	}
	if n := len(f.runs()); n != 1 {
		t.Errorf("runs = %d", n)
	}
}

func TestInboundChat_AckFirstDoesNotHoldTheRequestOnASlowPipeline(t *testing.T) {
	old := ackBudget
	ackBudget = 50 * time.Millisecond
	defer func() { ackBudget = old }()
	f := newChatFixture(t)
	v := f.createdChat()
	gs := &gatedStore{fakeInboundStore: f.store, gate: make(chan struct{})}
	f.s.inboundPipe.Store = gs
	start := time.Now()
	rec := f.post(v, "shh-token", map[string]string{"user": "U1", "team": "T1", "channel": "C1", "id": "slow", "text": "hi", "mode": "direct"})
	if time.Since(start) > 2*time.Second {
		t.Errorf("the request was held for %v", time.Since(start))
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "accepted") {
		t.Fatalf("slow pipeline = %d %s", rec.Code, rec.Body)
	}
	if len(f.runs()) != 0 {
		t.Fatal("the run started before the pipeline was released")
	}
	close(gs.gate) // the background work carries on after the answer
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runs()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(f.runs()) != 1 {
		t.Errorf("the message was lost after the acknowledgement: runs = %d", len(f.runs()))
	}
}

func TestInboundChat_CommandGetsItsImmediateLine(t *testing.T) {
	f := newChatFixture(t)
	v := f.createdChat()
	rec := f.post(v, "shh-token", map[string]string{"user": "U1", "team": "T1", "channel": "C1", "id": "c1", "text": "go", "mode": "command"})
	if rec.Code != 200 || rec.Body.String() != `{"text":"working"}` {
		t.Errorf("command = %d %s", rec.Code, rec.Body)
	}
	// A refused command gets the default, which says nothing about why.
	rec = f.post(v, "shh-token", map[string]string{"user": "U9", "team": "T1", "channel": "C1", "id": "c2", "text": "go", "mode": "command"})
	if strings.Contains(rec.Body.String(), "working") || !strings.Contains(rec.Body.String(), "rejected") {
		t.Errorf("refused command = %s", rec.Body)
	}
}

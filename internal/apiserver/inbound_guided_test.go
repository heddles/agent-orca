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
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/connections"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	enteredToken    = "entered-token-value"
	enteredBotToken = "entered-bot-token-value"
	enteredValues   = `{"values":{"token":"` + enteredToken + `","botToken":"` + enteredBotToken + `"}}`
)

type guidedFixture struct{ *chatFixtureT }

func newGuidedFixture(t *testing.T) *guidedFixture {
	t.Helper()
	f := newChatFixture(t)
	keys, err := connections.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetInboundKeys(keys)
	f.s.inboundPipe.Credentials = f.s.InboundBridge().OpenCredentials
	return &guidedFixture{f}
}

// draft is a chat channel saved with no credentials and nobody allowed.
func (f *guidedFixture) draft(name string) inboundView {
	f.t.Helper()
	rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/inbound-connections",
		`{"namespace":"shared","agent":"support","name":"`+name+`","kind":"chatx","config":{"modes":["direct","mention"]}}`)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("draft = %d %s", rec.Code, rec.Body)
	}
	return evalDecode[inboundView](f.t, rec)
}

func (f *guidedFixture) get(id string) inboundView {
	f.t.Helper()
	rec := f.api(idFor(tenancy.RoleViewer, isoA), http.MethodGet, "/api/inbound-connections?namespace=shared&agent=support", "")
	for _, v := range evalDecode[[]inboundView](f.t, rec) {
		if v.ID == id {
			return v
		}
	}
	f.t.Fatalf("connection %s not listed: %s", id, rec.Body)
	return inboundView{}
}

func TestInboundCredentials_OnlyAnAdminEntersThemAndNothingReturnsThem(t *testing.T) {
	f := newGuidedFixture(t)
	v := f.draft("c")
	if v.Credentials != "" || !v.CanStore {
		t.Fatalf("draft = %+v", v)
	}
	path := "/api/inbound-connections/" + v.ID + "/credentials"
	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner, tenancy.RoleBuilder} {
		if rec := f.api(idFor(role, isoA), http.MethodPut, path, enteredValues); rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d %s, want 403", role, rec.Code, rec.Body)
		}
	}
	if rec := f.api(idFor(tenancy.RoleAdmin, isoB), http.MethodPut, path, enteredValues); rec.Code != http.StatusNotFound {
		t.Errorf("another tenant's admin = %d", rec.Code)
	}
	admin := idFor(tenancy.RoleAdmin, isoA)
	for name, body := range map[string]string{
		"a required one missing": `{"values":{"token":"x"}}`,
		"an unknown one":         `{"values":{"token":"x","botToken":"y","baseUrl":"https://evil.example"}}`,
	} {
		if rec := f.api(admin, http.MethodPut, path, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	rec := f.api(admin, http.MethodPut, path, enteredValues)
	if rec.Code != http.StatusOK {
		t.Fatalf("store = %d %s", rec.Code, rec.Body)
	}
	if got := evalDecode[inboundView](t, rec); got.Credentials != credentialsStored {
		t.Errorf("credentials = %q", got.Credentials)
	}
	list := f.api(admin, http.MethodGet, "/api/inbound-connections", "")
	for _, secret := range []string{enteredToken, enteredBotToken} {
		if strings.Contains(rec.Body.String()+list.Body.String(), secret) {
			t.Errorf("a response holds the value %q", secret)
		}
		for _, e := range f.audit.events {
			if strings.Contains(e.Summary, secret) {
				t.Error("the audit log holds a credential")
			}
		}
	}
	stored := *f.store.conns[v.ID]
	if len(stored.CredentialsEnc) == 0 || bytes.Contains(stored.CredentialsEnc, []byte(enteredToken)) {
		t.Fatal("the credentials are not stored encrypted")
	}

	// The channel now works with them...
	if rec := f.post(v, enteredToken, map[string]string{"Challenge": "abc"}); rec.Code != http.StatusOK || rec.Body.String() != "abc" {
		t.Fatalf("challenge with the stored token = %d %s", rec.Code, rec.Body)
	}
	// ...and the sealed value opens for no other connection or tenant.
	br := f.s.InboundBridge()
	if got, err := br.OpenCredentials(stored); err != nil || string(got["token"]) != enteredToken {
		t.Fatalf("open = %v", err)
	}
	moved := stored
	moved.ID = f.draft("other").ID
	if _, err := br.OpenCredentials(moved); err == nil {
		t.Error("credentials copied to another connection opened")
	}
	moved = stored
	moved.TenantID = isoB
	if _, err := br.OpenCredentials(moved); err == nil {
		t.Error("credentials copied to another tenant opened")
	}

	// Naming a Secret instead drops what was stored.
	rec = f.api(admin, http.MethodPatch, "/api/inbound-connections/"+v.ID, `{"secretRef":"chatx-creds"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch to a Secret = %d %s", rec.Code, rec.Body)
	}
}

func TestInboundCredentials_NeedAKey(t *testing.T) {
	f := newGuidedFixture(t)
	v := f.draft("c")
	f.s.SetInboundKeys(nil)
	rec := f.api(idFor(tenancy.RoleAdmin, isoA), http.MethodPut, "/api/inbound-connections/"+v.ID+"/credentials", enteredValues)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Kubernetes Secret") {
		t.Fatalf("without a key = %d %s", rec.Code, rec.Body)
	}
	if got := f.get(v.ID); got.CanStore || got.Credentials != "" {
		t.Errorf("view = %+v", got)
	}
}

func TestInbound_SaysWhetherTheChannelIsReachingUs(t *testing.T) {
	f := newGuidedFixture(t)
	v := f.draft("c")
	f.api(idFor(tenancy.RoleAdmin, isoA), http.MethodPut, "/api/inbound-connections/"+v.ID+"/credentials", enteredValues)
	if got := f.get(v.ID); got.LastRequestAt != nil || got.LastAuthError != "" {
		t.Fatalf("before any request: %+v", got)
	}
	f.post(v, "wrong", map[string]string{"Challenge": "abc"})
	if got := f.get(v.ID); got.LastAuthError != "invalid token" || got.LastRequestAt != nil {
		t.Fatalf("after a bad request: %q %v", got.LastAuthError, got.LastRequestAt)
	}
	f.post(v, enteredToken, map[string]string{"Challenge": "abc"})
	if got := f.get(v.ID); got.LastAuthError != "" || got.LastRequestAt == nil {
		t.Fatalf("after a good one: %q %v", got.LastAuthError, got.LastRequestAt)
	}
}

func TestInboundPairing_AddsThePersonWhoSentTheCodeOnce(t *testing.T) {
	f := newGuidedFixture(t)
	v := f.draft("c")
	builder := idFor(tenancy.RoleBuilder, isoA)
	base := "/api/inbound-connections/" + v.ID + "/pairings"
	if rec := f.api(builder, http.MethodPost, base, ""); rec.Code != http.StatusConflict {
		t.Fatalf("pairing before credentials = %d %s", rec.Code, rec.Body)
	}
	f.api(idFor(tenancy.RoleAdmin, isoA), http.MethodPut, "/api/inbound-connections/"+v.ID+"/credentials", enteredValues)
	if rec := f.api(idFor(tenancy.RoleViewer, isoA), http.MethodPost, base, ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer = %d", rec.Code)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoB), http.MethodPost, base, ""); rec.Code != http.StatusNotFound {
		t.Errorf("another tenant = %d", rec.Code)
	}
	rec := f.api(builder, http.MethodPost, base, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	p := evalDecode[pairingView](t, rec)
	if len(p.Code) != 6 || p.Status != pairingWaiting || time.Until(p.ExpiresAt) > 11*time.Minute {
		t.Fatalf("pairing = %+v", p)
	}
	status := func() pairingView {
		return evalDecode[pairingView](t, f.api(builder, http.MethodGet, base+"/"+p.ID, ""))
	}
	if got := status(); got.Code != "" || got.Status != pairingWaiting {
		t.Fatalf("the code must not be readable later: %+v", got)
	}
	if rec := f.api(builder, http.MethodPost, base+"/"+p.ID+"/approve", ""); rec.Code != http.StatusConflict {
		t.Errorf("approving before anyone sent it = %d", rec.Code)
	}
	send := func(id, user, channel, mode, text string) {
		rec := f.post(v, enteredToken, map[string]string{"User": user, "Team": "T1", "Channel": channel, "ID": id, "Mode": mode, "Text": text})
		if rec.Code != http.StatusOK {
			t.Fatalf("post = %d %s", rec.Code, rec.Body)
		}
	}
	send("w", "U-wrong", "D1", "direct", "pair AAAAAA")
	if got := status(); got.Status != pairingWaiting {
		t.Fatalf("a wrong code was taken: %+v", got)
	}
	send("1", "U-me", "D1", "direct", " Pair "+strings.ToLower(p.Code)+" ")
	send("2", "U-late", "D2", "direct", "pair "+p.Code)
	if got := status(); got.Status != "received" || got.SenderID != "U-me" || !got.Direct {
		t.Fatalf("status = %+v, want the first sender only", got)
	}
	if len(f.runs()) != 0 {
		t.Fatalf("a pairing message started a run")
	}
	rec = f.api(builder, http.MethodPost, base+"/"+p.ID+"/approve", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"replySent":true`) {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	cfg := string(f.get(v.ID).Config)
	if !strings.Contains(cfg, `"allowSenders":["U-me"]`) || strings.Contains(cfg, "U-late") || strings.Contains(cfg, "D1") {
		t.Errorf("config = %s", cfg)
	}
	if rec := f.api(builder, http.MethodPost, base+"/"+p.ID+"/approve", ""); rec.Code != http.StatusNotFound {
		t.Errorf("approving twice = %d", rec.Code)
	}
	// The paired person's next message starts a run.
	send("3", "U-me", "D1", "direct", "hello")
	if n := len(f.runs()); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
}

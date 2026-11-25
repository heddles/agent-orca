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
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func (f *fakeInboundStore) InboundRefusals(_ context.Context, ids []string, _ time.Time) (map[string]map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]int{}
	for _, m := range f.msgs {
		for _, id := range ids {
			if m.ConnectionID == id && m.Status == postgresql.InboundRejected {
				if out[id] == nil {
					out[id] = map[string]int{}
				}
				out[id][m.Reason]++
			}
		}
	}
	return out, nil
}

// lookupAdapter is a channel that looks up its "account" setting.
type lookupAdapter struct {
	chanAdapter
	err error
}

func (a *lookupAdapter) Kind() string { return "lookup" }

func (a *lookupAdapter) CompleteConfig(_ context.Context, c inbound.Connection) (json.RawMessage, error) {
	if a.err != nil {
		return nil, a.err
	}
	if strings.Contains(string(c.Config), `"account"`) {
		return c.Config, nil
	}
	return json.RawMessage(`{"account":"found"}`), nil
}

func TestInbound_ListSaysWhyMessagesWereRefused(t *testing.T) {
	f := newInboundFixture(t)
	v := f.created()
	f.post(v, "mallory@example.com", "m1", "hi")
	f.post(v, "eve@example.com", "m2", "hi")
	rec := f.api(idFor(tenancy.RoleViewer, isoA), http.MethodGet, "/api/inbound-connections?namespace=shared&agent=support", "")
	got := evalDecode[[]inboundView](t, rec)
	if len(got) != 1 || got[0].Refused["sender_not_allowed"] != 2 {
		t.Fatalf("list = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "mallory") {
		t.Error("the list names a refused sender")
	}
}

func TestInbound_URLUsesTheConfiguredPublicAddress(t *testing.T) {
	f := newInboundFixture(t)
	if v := f.created(); v.URL != "" {
		t.Errorf("url = %q with none configured", v.URL)
	}
	if err := f.s.SetInboundPublicURL(" https://hooks.example.com/ "); err != nil {
		t.Fatal(err)
	}
	rec := f.api(idFor(tenancy.RoleViewer, isoA), http.MethodGet, "/api/inbound-connections?namespace=shared&agent=support", "")
	got := evalDecode[[]inboundView](t, rec)
	if len(got) != 1 || got[0].URL != "https://hooks.example.com"+got[0].Path {
		t.Fatalf("list = %s", rec.Body)
	}
	for _, bad := range []string{"http://hooks.example.com", "hooks.example.com", "https://u:p@hooks.example.com", "https://hooks.example.com?x=1", "https://"} {
		if _, err := parseInboundPublicURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestInbound_SettingsAreLookedUpOnceThereAreCredentials(t *testing.T) {
	f := newInboundFixture(t)
	look := &lookupAdapter{}
	f.s.inboundPipe.Registry.Register(look)
	builder := idFor(tenancy.RoleBuilder, isoA)
	create := func(name, secret string) (int, inboundView, string) {
		rec := f.api(builder, http.MethodPost, "/api/inbound-connections",
			`{"namespace":"shared","agent":"support","name":"`+name+`","kind":"lookup","secretRef":"`+secret+`","config":`+chanConfig+`}`)
		if rec.Code != http.StatusCreated {
			return rec.Code, inboundView{}, rec.Body.String()
		}
		return rec.Code, evalDecode[inboundView](t, rec), rec.Body.String()
	}

	// A draft has no credentials, so nothing is looked up.
	code, draft, body := create("draft", "")
	if code != http.StatusCreated || strings.Contains(string(draft.Config), "found") {
		t.Fatalf("draft = %d %s", code, body)
	}
	// Adding the credentials later fills the setting in and keeps the rest.
	rec := f.api(builder, http.MethodPatch, "/api/inbound-connections/"+draft.ID, `{"secretRef":"creds"}`)
	got := evalDecode[inboundView](t, rec)
	if rec.Code != http.StatusOK || !strings.Contains(string(got.Config), `"account":"found"`) ||
		!strings.Contains(string(got.Config), "alice@example.com") {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body)
	}
	// So does creating with them.
	if code, v, body := create("ready", "creds"); code != http.StatusCreated || !strings.Contains(string(v.Config), `"account":"found"`) {
		t.Fatalf("create = %d %s", code, body)
	}
	// Credentials the platform refuses save nothing.
	look.err = errors.New("auth.test: invalid_auth")
	if code, _, body := create("bad", "creds"); code != http.StatusBadRequest || !strings.Contains(body, "invalid_auth") {
		t.Fatalf("refused credentials = %d %s", code, body)
	}
	list := evalDecode[[]inboundView](t, f.api(builder, http.MethodGet, "/api/inbound-connections?namespace=shared&agent=support", ""))
	if len(list) != 2 {
		t.Errorf("%d connections after a refused create, want 2", len(list))
	}
}

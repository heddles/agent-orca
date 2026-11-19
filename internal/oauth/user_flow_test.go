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

package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestLoginURLCarriesStateAndResource(t *testing.T) {
	m := New(nil, nil)
	meta := &Metadata{
		AuthorizationEndpoint: "https://as.example/authorize",
		GrantTypes:            []string{"authorization_code"},
		CodeChallengeMethods:  []string{"S256"},
	}
	u, verifier, err := m.LoginURL(meta, &Config{ClientID: "c", RedirectURI: "https://ui/cb", State: "st-1", Resource: "https://mcp.example/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, u)
	if q.Get("state") != "st-1" || q.Get("resource") != "https://mcp.example/mcp" {
		t.Errorf("query = %v", q)
	}
	if !verifyPKCE(verifier, q.Get("code_challenge")) {
		t.Error("challenge does not match the verifier")
	}
	// The CLI seed sends neither.
	u, _, _ = m.LoginURL(meta, &Config{ClientID: "c", RedirectURI: "https://ui/cb"})
	if q := mustQuery(t, u); q.Has("state") || q.Has("resource") {
		t.Errorf("unexpected params: %v", q)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestExchangeSendsResourceAndReportsScope(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("code") == "bad" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":60,"scope":"read write"}`))
	}))
	defer srv.Close()
	m := New(srv.Client(), nil)
	meta := &Metadata{TokenEndpoint: srv.URL, GrantTypes: []string{"authorization_code", "refresh_token"}}
	cfg := &Config{ClientID: "c", RedirectURI: "https://ui/cb", Resource: "https://mcp.example/mcp"}
	tr, err := m.ExchangeCode(context.Background(), meta, cfg, "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("resource") != "https://mcp.example/mcp" || tr.Scope != "read write" {
		t.Errorf("form = %v, scope = %q", got, tr.Scope)
	}
	cfg.RefreshToken = "rt"
	if _, err := m.Refresh(context.Background(), meta, cfg); err != nil || got.Get("resource") == "" {
		t.Errorf("refresh: %v form=%v", err, got)
	}
	_, err = m.ExchangeCode(context.Background(), meta, cfg, "bad", "verifier")
	if !IsInvalidGrant(err) {
		t.Errorf("want invalid_grant, got %v", err)
	}
	if IsInvalidGrant(errors.New("x")) {
		t.Error("plain error is not invalid_grant")
	}
}

func TestRevoke(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		if r.PostForm.Get("token") == "fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	m := New(srv.Client(), nil)
	cfg := &Config{ClientID: "c", ClientSecret: "s"}
	if err := m.Revoke(context.Background(), &Metadata{RevocationEndpoint: srv.URL}, cfg, "tok", "refresh_token"); err != nil {
		t.Fatal(err)
	}
	if form.Get("token") != "tok" || form.Get("token_type_hint") != "refresh_token" || form.Get("client_id") != "c" {
		t.Errorf("form = %v", form)
	}
	if err := m.Revoke(context.Background(), &Metadata{RevocationEndpoint: srv.URL}, cfg, "fail", ""); err == nil {
		t.Error("a 500 must be an error")
	}
	if err := m.Revoke(context.Background(), &Metadata{}, cfg, "tok", ""); !errors.Is(err, ErrNoRevocationEndpoint) {
		t.Errorf("no endpoint = %v", err)
	}
}

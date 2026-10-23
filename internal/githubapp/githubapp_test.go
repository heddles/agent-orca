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

package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testKey(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return pemBytes, key
}

func TestInstallationToken(t *testing.T) {
	pemBytes, key := testKey(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	var gotReq TokenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		// The App JWT must verify with the App's public key and name the App.
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims := &jwt.RegisteredClaims{}
		_, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
			jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { return now }))
		if err != nil || claims.Issuer != "1234" || claims.ExpiresAt.After(now.Add(10*time.Minute)) {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_abc","expires_at":"2026-10-23T13:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New("1234", 42, pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	c.APIURL, c.Now = srv.URL, func() time.Time { return now }
	req := TokenRequest{Repositories: []string{"agent-orca"}, Permissions: map[string]string{"contents": "write"}}
	tok, err := c.InstallationToken(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "ghs_abc" || !tok.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("token = %+v", tok)
	}
	if !reflect.DeepEqual(gotReq, req) {
		t.Errorf("narrowing sent = %+v, want %+v", gotReq, req)
	}
}

func TestInstallationTokenErrorHidesResponse(t *testing.T) {
	pemBytes, _ := testKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"repository not accessible","secret":"do-not-echo"}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := New("1", 1, pemBytes)
	c.APIURL = srv.URL
	_, err := c.InstallationToken(context.Background(), TokenRequest{})
	if err == nil || !strings.Contains(err.Error(), "repository not accessible") || strings.Contains(err.Error(), "do-not-echo") {
		t.Errorf("err = %v", err)
	}
}

func TestRevokeToken(t *testing.T) {
	for status, wantErr := range map[int]bool{http.StatusNoContent: false, http.StatusUnauthorized: false, http.StatusInternalServerError: true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/installation/token" || r.Header.Get("Authorization") != "Bearer ghs_x" {
				http.Error(w, "unexpected", http.StatusBadRequest)
				return
			}
			w.WriteHeader(status)
		}))
		c := &Client{APIURL: srv.URL}
		if err := c.RevokeToken(context.Background(), "ghs_x"); (err != nil) != wantErr {
			t.Errorf("status %d: err = %v", status, err)
		}
		srv.Close()
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	pemBytes, _ := testKey(t)
	if _, err := New("", 1, pemBytes); err == nil {
		t.Error("missing app ID accepted")
	}
	if _, err := New("1", 1, []byte("not a key")); err == nil {
		t.Error("invalid key accepted")
	}
}

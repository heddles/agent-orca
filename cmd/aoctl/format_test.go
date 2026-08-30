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

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testResp(status int, ct, body string) *http.Response {
	u := &url.URL{Scheme: "http", Host: "127.0.0.1:8084", Path: "/v1/tasks"}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {ct}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    &http.Request{Method: http.MethodGet, URL: u},
	}
}

// TestDecodeJSON_Success verifies a well-formed JSON 200 decodes cleanly.
func TestDecodeJSON_Success(t *testing.T) {
	resp := testResp(http.StatusOK, "application/json", `{"tasks":[{"id":"t1"}],"count":1}`)
	var out struct {
		Tasks []TaskResponse `json:"tasks"`
	}
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		t.Fatalf("decodeJSON: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].ID != "t1" {
		t.Fatalf("bad decode: %+v", out)
	}
}

// TestDecodeJSON_HTML is the regression test for notes 4 & 5: a 200 HTML body
// (e.g. a UI-proxy SPA fallback) must yield an actionable diagnostic, NOT the
// cryptic "invalid character '<'".
func TestDecodeJSON_HTML(t *testing.T) {
	html := `<!doctype html><html><body>agent-orca UI</body></html>`
	resp := testResp(http.StatusOK, "text/html; charset=utf-8", html)
	var out struct{}
	err := decodeJSON(resp, &out, http.StatusOK)
	if err == nil {
		t.Fatal("expected an error for HTML response")
	}
	msg := err.Error()
	if strings.Contains(msg, "invalid character") {
		t.Fatalf("should not leak the raw decode error: %q", msg)
	}
	if !strings.Contains(msg, "non-JSON response") {
		t.Fatalf("expected 'non-JSON response' diagnostic, got: %q", msg)
	}
	if !strings.Contains(msg, "UI proxy") || !strings.Contains(msg, ":8084") {
		t.Fatalf("expected endpoint hint, got: %q", msg)
	}
	if !strings.Contains(msg, "--acp-endpoint") {
		t.Fatalf("expected --acp-endpoint hint in diagnostic, got: %q", msg)
	}
	if !strings.Contains(msg, "127.0.0.1:8084/v1/tasks") {
		t.Fatalf("expected URL in error, got: %q", msg)
	}
}

// TestDecodeJSON_Non200 verifies a non-2xx body surfaces the status + a snippet.
func TestDecodeJSON_Non200(t *testing.T) {
	resp := testResp(http.StatusUnauthorized, "application/json", `{"error":"unauthorized"}`)
	var out struct{}
	err := decodeJSON(resp, &out, http.StatusOK)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected HTTP status in error, got: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("expected body snippet, got: %q", err.Error())
	}
}

// TestDecodeJSON_MalformedJSON verifies a malformed JSON document still reports
// the underlying decode error (with context), not the non-JSON branch.
func TestDecodeJSON_MalformedJSON(t *testing.T) {
	resp := testResp(http.StatusOK, "application/json", `{not json}`)
	var out struct{}
	err := decodeJSON(resp, &out, http.StatusOK)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("expected 'decoding' in error, got: %q", err.Error())
	}
	if strings.Contains(err.Error(), "non-JSON") {
		t.Fatalf("should be a decode error not a content-type error: %q", err.Error())
	}
}

func TestTableWriter_Basic(t *testing.T) {
	var buf bytes.Buffer
	tw := newTableWriter().header("ID", "STATUS")
	tw.row("t-1", "Pending")
	tw.row("t-longer", "Succeeded")
	if err := tw.render(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"ID", "STATUS", "---", "t-1", "t-longer", "Succeeded"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in table output:\n%s", want, out)
		}
	}
}

func TestTableWriter_Empty(t *testing.T) {
	var buf bytes.Buffer
	tw := newTableWriter()
	if err := tw.render(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if buf.String() != "" {
		t.Fatalf("expected empty output, got %q", buf.String())
	}
}

func TestRender_JSON(t *testing.T) {
	s := &settings{jsonOut: true, out: &bytes.Buffer{}}
	in := []string{"a", "b"}
	if err := s.render(in, func(io.Writer) error { t.Fatal("human must not run in json mode"); return nil }); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(s.out.(*bytes.Buffer).String(), "\"a\"") {
		t.Fatalf("expected JSON array, got: %q", s.out.(*bytes.Buffer).String())
	}
}

func TestRender_Human(t *testing.T) {
	s := &settings{jsonOut: false, out: &bytes.Buffer{}}
	called := false
	if err := s.render(nil, func(w io.Writer) error {
		called = true
		_, _ = fmt.Fprintln(w, "human-line")
		return nil
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !called {
		t.Fatal("human formatter was not invoked")
	}
	if !strings.Contains(s.out.(*bytes.Buffer).String(), "human-line") {
		t.Fatalf("expected human output, got: %q", s.out.(*bytes.Buffer).String())
	}
}

// --- command-level --json parity (via runCLI + httptest) ---

func TestCmdTasksList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[{"id":"t1","agent":"a","status":"Pending"}],"count":1}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, nil, "tasks", "ls", "--endpoint", srv.URL, "--token", "tok", "--json")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(stdout, `"id": "t1"`) {
		t.Fatalf("expected JSON array with task id, got: %q", stdout)
	}
}

func TestCmdAgentsList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[{"name":"my-agent","namespace":"ns-a","description":"d"}]}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, nil, "agents", "list", "--acp-endpoint", srv.URL, "--token", "tok", "--json")
	if err != nil {
		t.Fatalf("agents list: %v", err)
	}
	if !strings.Contains(stdout, `"name": "my-agent"`) {
		t.Fatalf("expected JSON manifest, got: %q", stdout)
	}
}

func TestCmdAdminTenantsList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenants":[{"name":"acme","clientID":"c1","targetNamespace":"tenant-acme"}],"count":1}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, nil, "admin", "tenants", "list", "--endpoint", srv.URL, "--token", "sa-token", "--json")
	if err != nil {
		t.Fatalf("admin tenants list: %v", err)
	}
	if !strings.Contains(stdout, `"name": "acme"`) {
		t.Fatalf("expected JSON tenant list, got: %q", stdout)
	}
}

// TestCmdTasksList_Human verifies the default (non-JSON) output is a table that
// still contains the task id (non-regression for the e2e substring assertion).
func TestCmdTasksList_Human(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[{"id":"t1","agent":"a","status":"Pending"}],"count":1}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, nil, "tasks", "ls", "--endpoint", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(stdout, "t1") || !strings.Contains(stdout, "ID") || !strings.Contains(stdout, "STATUS") {
		t.Fatalf("expected a table with id/headers, got: %q", stdout)
	}
	if strings.Contains(stdout, `"id"`) {
		t.Fatalf("human mode should not emit JSON keys, got: %q", stdout)
	}
}

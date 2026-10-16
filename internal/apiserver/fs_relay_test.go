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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/acpfs"
)

// fsTestRun creates a run owned by the test tenant in tenant-acme.
func fsTestRun(name string) *agentorcav1alpha1.AgentRun {
	return &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "tenant-acme",
			Labels: map[string]string{
				"agentorca.io/tenant": "acme",
			},
		},
	}
}

// fsTestServer builds an ACPServer with an in-memory fs relay and a run.
func fsTestServer(t *testing.T, relay acpfs.Relay, run *agentorcav1alpha1.AgentRun) *ACPServer {
	t.Helper()
	s := newACPServer(t, run)
	s.SetFilesystemRelay(relay)
	return s
}

func TestACPFSPending_NoRelayReturns501(t *testing.T) {
	s := newACPServer(t, fsTestRun("acp-run-1"))
	h := s.withTenant(testTenant())

	req := httptest.NewRequest(http.MethodGet, "/runs/acp-run-1/fs/pending", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 without relay, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestACPFSPending_Flow(t *testing.T) {
	relay := acpfs.NewMemRelay()
	s := fsTestServer(t, relay, fsTestRun("acp-run-1"))
	h := s.withTenant(testTenant())

	// Empty pending → requests: [] (long-poll returns immediately because
	// there is nothing to wait for).
	req := httptest.NewRequest(http.MethodGet, "/runs/acp-run-1/fs/pending?wait=0", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty pending, got %d: %s", rec.Code, rec.Body.String())
	}
	var empty struct {
		Requests []acpfs.Request `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decoding empty pending: %v", err)
	}
	if len(empty.Requests) != 0 {
		t.Fatalf("expected no pending requests, got %+v", empty.Requests)
	}

	// A pod enqueues a read; the bridge sees it and answers it.
	created, err := relay.Enqueue(t.Context(), "tenant-acme:acp-run-1", acpfs.EnqueueRequest{
		Method: acpfs.MethodReadTextFile,
		Params: json.RawMessage(`{"path":"main.go"}`),
	})
	if err != nil {
		t.Fatal("enqueue:", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/runs/acp-run-1/fs/pending?wait=0", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with pending, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Requests []acpfs.Request `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding pending: %v", err)
	}
	if len(got.Requests) != 1 || got.Requests[0].ID != created.ID {
		t.Fatalf("expected the enqueued request, got %+v", got.Requests)
	}

	// Respond → 204, and the request disappears from pending.
	respBody := `{"result":{"content":"package main"}}`
	req = httptest.NewRequest(http.MethodPost, "/runs/acp-run-1/fs/pending/"+created.ID+"/response", bytes.NewBufferString(respBody))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on respond, got %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/runs/acp-run-1/fs/pending?wait=0", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding pending after respond: %v", err)
	}
	if len(got.Requests) != 0 {
		t.Fatalf("expected no pending after respond, got %+v", got.Requests)
	}

	// The pod takes the response.
	taken, err := relay.Take(t.Context(), "tenant-acme:acp-run-1", created.ID)
	if err != nil {
		t.Fatal("take:", err)
	}
	if taken == nil || string(taken.Result) != `{"content":"package main"}` {
		t.Fatalf("unexpected taken response: %+v", taken)
	}
}

func TestACPFSPending_TenantIsolation(t *testing.T) {
	relay := acpfs.NewMemRelay()
	run := fsTestRun("acp-run-1")
	s := fsTestServer(t, relay, run)
	// A different tenant cannot see or answer this run's fs endpoints.
	other := &TenantIdentity{TenantName: "mallory", Namespace: "tenant-mallory"}
	h := s.withTenant(other)

	req := httptest.NewRequest(http.MethodGet, "/runs/acp-run-1/fs/pending?wait=0", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for foreign tenant, got %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/runs/acp-run-1/fs/pending/some-id/response", bytes.NewBufferString(`{}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for foreign tenant respond, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestACPFSPending_LongPollReturnsWhenRequestArrives(t *testing.T) {
	relay := acpfs.NewMemRelay()
	s := fsTestServer(t, relay, fsTestRun("acp-run-1"))
	h := s.withTenant(testTenant())

	// Enqueue shortly after the poll starts (from "the pod").
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = relay.Enqueue(t.Context(), "tenant-acme:acp-run-1", acpfs.EnqueueRequest{
			Method: acpfs.MethodReadTextFile,
		})
	}()

	req := httptest.NewRequest(http.MethodGet, "/runs/acp-run-1/fs/pending?wait=3", nil)
	rec := httptest.NewRecorder()
	before := time.Now()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if elapsed := time.Since(before); elapsed >= 3*time.Second {
		t.Fatalf("long poll should return as soon as a request arrives, took %s", elapsed)
	}
	var got struct {
		Requests []acpfs.Request `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Requests) != 1 {
		t.Fatalf("expected exactly one request from long poll, got %s (%v)", rec.Body.String(), err)
	}
}

func TestInternalFS_NoRelayReturns501(t *testing.T) {
	s := newACPServer(t, fsTestRun("pod-run-1"))
	srv := &Server{crdClient: s.crdClient}

	req := httptest.NewRequest(http.MethodPost, "/agentrun/tenant-acme/pod-run-1/fs",
		bytes.NewBufferString(`{"method":"fs/read_text_file","params":{"path":"a.go"}}`))
	rec := httptest.NewRecorder()
	srv.enqueueFSRequest(rec, req, "tenant-acme", "pod-run-1")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 without relay, got %d", rec.Code)
	}
}

func TestInternalFS_EnqueueTakeFlow(t *testing.T) {
	relay := acpfs.NewMemRelay()
	s := newACPServer(t, fsTestRun("pod-run-1"))
	srv := &Server{crdClient: s.crdClient, fsRelay: relay}

	// Unknown run → 404 (no relay entry created).
	req := httptest.NewRequest(http.MethodPost, "/agentrun/tenant-acme/nope/fs",
		bytes.NewBufferString(`{"method":"fs/read_text_file","params":{"path":"a.go"}}`))
	rec := httptest.NewRecorder()
	srv.enqueueFSRequest(rec, req, "tenant-acme", "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown run, got %d", rec.Code)
	}

	// Valid run + method → 201 with an ID; a disallowed method → 400.
	req = httptest.NewRequest(http.MethodPost, "/agentrun/tenant-acme/pod-run-1/fs",
		bytes.NewBufferString(`{"method":"fs/read_text_file","params":{"path":"a.go"}}`))
	rec = httptest.NewRecorder()
	srv.enqueueFSRequest(rec, req, "tenant-acme", "pod-run-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created acpfs.Request
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("expected request id, got %s (%v)", rec.Body.String(), err)
	}

	req = httptest.NewRequest(http.MethodPost, "/agentrun/tenant-acme/pod-run-1/fs",
		bytes.NewBufferString(`{"method":"fs/evil","params":{}}`))
	rec = httptest.NewRecorder()
	srv.enqueueFSRequest(rec, req, "tenant-acme", "pod-run-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-fs method, got %d", rec.Code)
	}

	// The bridge answers via the relay; the pod's take returns it, and a
	// second take is gone (410).
	if err := relay.Respond(t.Context(), "tenant-acme:pod-run-1", created.ID, &acpfs.Response{
		Result: json.RawMessage(`{"content":"x"}`),
	}); err != nil {
		t.Fatal("respond:", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/agentrun/tenant-acme/pod-run-1/fs/"+created.ID, nil)
	rec = httptest.NewRecorder()
	srv.takeFSResponse(rec, req, "tenant-acme", "pod-run-1", created.ID)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"id":"`+created.ID+`","result":{"content":"x"}}`+"\n" {
		t.Fatalf("expected the response body, got %d: %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/agentrun/tenant-acme/pod-run-1/fs/"+created.ID, nil)
	rec = httptest.NewRecorder()
	srv.takeFSResponse(rec, req, "tenant-acme", "pod-run-1", created.ID)
	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410 after take, got %d", rec.Code)
	}
}

func TestInternalFS_TakeNoWaitPendingReturns204(t *testing.T) {
	relay := acpfs.NewMemRelay()
	s := newACPServer(t, fsTestRun("pod-run-1"))
	srv := &Server{crdClient: s.crdClient, fsRelay: relay}

	created, err := relay.Enqueue(t.Context(), "tenant-acme:pod-run-1", acpfs.EnqueueRequest{
		Method: acpfs.MethodWriteTextFile,
	})
	if err != nil {
		t.Fatal("enqueue:", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/agentrun/tenant-acme/pod-run-1/fs/"+created.ID, nil)
	rec := httptest.NewRecorder()
	srv.takeFSResponse(rec, req, "tenant-acme", "pod-run-1", created.ID)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 while pending, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFilesystemManifestPolicy_Projection(t *testing.T) {
	if got := filesystemManifestPolicy(nil); got != nil {
		t.Fatalf("nil spec should project to nil manifest policy, got %+v", got)
	}
	p := filesystemManifestPolicy(&agentorcav1alpha1.FilesystemAccessSpec{
		Mode:          agentorcav1alpha1.FilesystemReadWrite,
		WriteApproval: true,
	})
	if p == nil || p.Mode != "readWrite" || !p.WriteApproval {
		t.Fatalf("readWrite projection wrong: %+v", p)
	}
	// Empty mode (pre-enum CR) is conservatively readOnly; writeApproval
	// passes through unchanged.
	p = filesystemManifestPolicy(&agentorcav1alpha1.FilesystemAccessSpec{})
	if p.Mode != "readOnly" || p.WriteApproval {
		t.Fatalf("empty-mode projection should be readOnly without approval: %+v", p)
	}
}

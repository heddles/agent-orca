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
	"net/http/httptest"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
)

type fakeRoutingReader struct {
	events         []postgresql.RoutingEvent
	err            error
	gotNS, gotRun  string
	gotLimit, hits int
}

func (f *fakeRoutingReader) RoutingEventsForRun(_ context.Context, ns, run string, limit int) ([]postgresql.RoutingEvent, error) {
	f.hits++
	f.gotNS, f.gotRun, f.gotLimit = ns, run, limit
	return f.events, f.err
}

func getRunRoutingEvents(s *UIServer, ctx context.Context, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleRunOrStream(rec, req)
	return rec
}

func TestRunRoutingEvents_ReturnsTheRunsHistory(t *testing.T) {
	reader := &fakeRoutingReader{events: []postgresql.RoutingEvent{
		{DecisionID: "d-1", Provider: "p", Path: "rule:weighted"},
		{DecisionID: "d-2", Provider: "q", Path: "fallback", ParentDecisionID: "d-1"},
	}}
	s := &UIServer{routingReader: reader}

	rec := getRunRoutingEvents(s, context.Background(), "/api/runs/team-a/run-1/routing-events?limit=500")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Events []postgresql.RoutingEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 2 || body.Events[1].ParentDecisionID != "d-1" {
		t.Errorf("events = %+v", body.Events)
	}
	if reader.gotNS != "team-a" || reader.gotRun != routingTestRun || reader.gotLimit != 500 { //nolint:goconst
		t.Errorf("queried %q/%q limit %d", reader.gotNS, reader.gotRun, reader.gotLimit)
	}
}

func TestRunRoutingEvents_TenantsSeeOnlyTheirNamespaces(t *testing.T) {
	reader := &fakeRoutingReader{}
	s := &UIServer{routingReader: reader}
	ctx := context.WithValue(context.Background(), tenantIdentityKey, &TenantIdentity{Roles: testAdminRoles, TenantName: "acme", Namespaces: []string{"team-a"}})

	if rec := getRunRoutingEvents(s, ctx, "/api/runs/team-b/run-1/routing-events"); rec.Code != http.StatusForbidden {
		t.Errorf("other namespace: status = %d, want 403", rec.Code)
	}
	if reader.hits != 0 {
		t.Errorf("a forbidden request reached the store")
	}
	if rec := getRunRoutingEvents(s, ctx, "/api/runs/team-a/run-1/routing-events"); rec.Code != http.StatusOK {
		t.Errorf("own namespace: status = %d, want 200", rec.Code)
	}
}

func TestRunRoutingEvents_WithoutPostgresIs503(t *testing.T) {
	s := &UIServer{}
	if rec := getRunRoutingEvents(s, context.Background(), "/api/runs/ns/run/routing-events"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so the UI falls back to the last 100", rec.Code)
	}
}

func TestRunRoutingEvents_StoreErrorAndMethod(t *testing.T) {
	s := &UIServer{routingReader: &fakeRoutingReader{err: errors.New("boom")}}
	if rec := getRunRoutingEvents(s, context.Background(), "/api/runs/ns/run/routing-events"); rec.Code != http.StatusInternalServerError {
		t.Errorf("store error: status = %d, want 500", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs/ns/run/routing-events", nil)
	rec := httptest.NewRecorder()
	s.handleRunOrStream(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d, want 405", rec.Code)
	}
}

type fakeRoutingLister struct {
	fakeRoutingReader
	events []postgresql.RoutingEvent
	got    postgresql.RoutingEventQuery
	err    error
}

func (f *fakeRoutingLister) ListRoutingEvents(_ context.Context, q postgresql.RoutingEventQuery) ([]postgresql.RoutingEvent, error) {
	f.hits++
	f.got = q
	return f.events, f.err
}

func listRoutingEvents(s *UIServer, ctx context.Context, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/routing-events"+query, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleRoutingEvents(rec, req)
	return rec
}

func TestRoutingEventsList_PassesFiltersAndNamesTheRun(t *testing.T) {
	l := &fakeRoutingLister{events: []postgresql.RoutingEvent{{DecisionID: "d-1", Provider: "p", Namespace: "team-a", RunName: routingTestRun}}}
	s := &UIServer{routingReader: l}
	rec := listRoutingEvents(s, context.Background(),
		"?namespace=team-a&agent=bot&selector=sel&outcome=error&shadow=true&since=2026-01-02T03:04:05Z&until=2026-01-03T00:00:00Z&limit=20")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	g := l.got
	if len(g.Namespaces) != 1 || g.Namespaces[0] != "team-a" || g.Agent != "bot" || g.Selector != "sel" || g.Outcome != "error" ||
		!g.IncludeShadow || g.Limit != 20 || g.Since.IsZero() || g.Until.IsZero() {
		t.Errorf("query = %+v", g)
	}
	var body struct {
		Events []struct{ DecisionID, Namespace, Run string } `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Events) != 1 || body.Events[0].Namespace != "team-a" || body.Events[0].Run != routingTestRun {
		t.Errorf("body = %s, %v", rec.Body, err)
	}
}

func TestRoutingEventsList_TenantsAreLimitedToTheirNamespaces(t *testing.T) {
	l := &fakeRoutingLister{}
	s := &UIServer{routingReader: l}
	ctx := context.WithValue(context.Background(), tenantIdentityKey, &TenantIdentity{Roles: testAdminRoles, TenantName: "acme", Namespaces: []string{"team-a", "team-c"}})

	if rec := listRoutingEvents(s, ctx, "?namespace=team-b"); rec.Code != http.StatusForbidden || l.hits != 0 {
		t.Errorf("other namespace: status = %d, hits = %d; want 403 and no store call", rec.Code, l.hits)
	}
	if rec := listRoutingEvents(s, ctx, ""); rec.Code != http.StatusOK || len(l.got.Namespaces) != 2 {
		t.Errorf("no namespace: status = %d, query = %+v; want the tenant's namespaces", rec.Code, l.got)
	}
	none := context.WithValue(context.Background(), tenantIdentityKey, &TenantIdentity{Roles: testAdminRoles, TenantName: "acme"})
	hits := l.hits
	if rec := listRoutingEvents(s, none, ""); rec.Code != http.StatusOK || l.hits != hits {
		t.Errorf("tenant without namespaces: status = %d, hits %d -> %d; want 200 and an empty answer without a query", rec.Code, hits, l.hits)
	}
}

func TestRoutingEventsList_RejectsBadInput(t *testing.T) {
	l := &fakeRoutingLister{}
	s := &UIServer{routingReader: l}
	for _, q := range []string{"?since=yesterday", "?until=x", "?limit=0", "?limit=501", "?limit=x"} {
		if rec := listRoutingEvents(s, context.Background(), q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
	l.err = postgresql.ErrInvalidRoutingQuery
	if rec := listRoutingEvents(s, context.Background(), "?outcome=bogus"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad outcome: status = %d, want 400", rec.Code)
	}
	l.err = errors.New("boom")
	if rec := listRoutingEvents(s, context.Background(), ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("store error: status = %d, want 500", rec.Code)
	}
	if rec := listRoutingEvents(&UIServer{}, context.Background(), ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: status = %d, want 503", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/routing-events", nil)
	rec := httptest.NewRecorder()
	s.handleRoutingEvents(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d, want 405", rec.Code)
	}
}

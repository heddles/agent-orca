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

package postgresql

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// defaults renders who is the default of each place, e.g. "C1=a DM=b".
func defaults(t *testing.T, s *Store, ctx context.Context, conn string) string {
	t.Helper()
	routes, err := s.ListInboundRoutes(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range routes {
		for _, p := range r.Places {
			if p.Default {
				name := p.ID
				if name == DirectPlace {
					name = "DM"
				}
				out = append(out, name+"="+r.Agent)
			}
		}
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

func TestInboundRoutesAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	b := WithTenant(context.Background(), tenantB)
	app := newInbound(t, evalNSA, "shared-bot")
	app.Namespace, app.Agent, app.SecretRef = "", "", ""
	if _, err := s.CreateInboundConnection(a, app); err != nil {
		t.Fatal(err)
	}
	own := newInbound(t, evalNSA, "own")
	if _, err := s.CreateInboundConnection(a, own); err != nil {
		t.Fatal(err)
	}
	route := func(agent string, places ...string) InboundRoute {
		r := InboundRoute{Namespace: evalNSA, Agent: agent, AllowSenders: []string{"U1"}}
		for _, p := range places {
			r.Places = append(r.Places, InboundPlace{ID: p, Name: "#" + strings.ToLower(p)})
		}
		return r
	}
	save := func(r InboundRoute) {
		t.Helper()
		if err := s.SaveInboundRoute(a, app.ID, r, "u"); err != nil {
			t.Fatal(err)
		}
	}
	want := func(step, w string) {
		t.Helper()
		if got := defaults(t, s, a, app.ID); got != w {
			t.Errorf("%s: defaults = %q, want %q", step, got, w)
		}
	}

	save(route("one", "C1", DirectPlace))
	want("the first agent is the default", "C1=one DM=one")
	save(route("two", "C1", "C2", DirectPlace))
	want("joining takes no default", "C1=one C2=two DM=one")
	save(route("one", "C1", DirectPlace))
	want("saving again keeps its defaults", "C1=one C2=two DM=one")

	if err := s.SetInboundDefault(a, app.ID, "C1", evalNSA, "two"); err != nil {
		t.Fatal(err)
	}
	want("moved", "C1=two C2=two DM=one")
	if err := s.SetInboundDefault(a, app.ID, "C2", evalNSA, "one"); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("default for an agent not in the place = %v", err)
	}
	want("a refused move changes nothing", "C1=two C2=two DM=one")

	save(route("two", "C2"))
	want("leaving a place passes its default on", "C1=one C2=two DM=one")

	// Names are kept when a later save has none.
	r := route("two", "C2")
	r.Places[0].Name = ""
	save(r)
	routes, _ := s.ListInboundRoutes(a, app.ID)
	if len(routes) != 2 || len(routes[1].Places) != 1 || routes[1].Places[0].Name != "#c2" || !slices.Equal(routes[1].AllowSenders, []string{"U1"}) {
		t.Errorf("routes = %+v", routes)
	}
	save(route("two"))
	want("no places at all", "C1=one DM=one")

	// Other tenants and one agent's own channel have no routes.
	if got, err := s.ListInboundRoutes(b, app.ID); err != nil || len(got) != 0 {
		t.Errorf("other tenant list = %v, %v", got, err)
	}
	if err := s.SaveInboundRoute(b, app.ID, route("x", "C1"), "u"); !errors.Is(err, ErrInboundNotFound) {
		t.Errorf("other tenant save = %v", err)
	}
	if err := s.DeleteInboundRoute(b, app.ID, evalNSA, "one"); !errors.Is(err, ErrInboundNotFound) {
		t.Errorf("other tenant delete = %v", err)
	}
	if err := s.SaveInboundRoute(a, own.ID, route("x", "C1"), "u"); !errors.Is(err, ErrInvalidInbound) {
		t.Errorf("a route on one agent's channel = %v", err)
	}

	save(route("two", "C1", DirectPlace))
	if err := s.DeleteInboundRoute(a, app.ID, evalNSA, "one"); err != nil {
		t.Fatal(err)
	}
	want("removing an agent passes its defaults on", "C1=two DM=two")
	if err := s.DeleteInboundRoute(a, app.ID, evalNSA, "one"); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("removing twice = %v", err)
	}
	if _, err := s.DeleteInboundConnection(a, app.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListInboundRoutes(a, app.ID); len(got) != 0 {
		t.Errorf("routes of a deleted app: %v", got)
	}
}

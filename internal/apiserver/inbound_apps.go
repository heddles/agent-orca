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

// Shared chat apps (docs/inbound-channels.md, "Shared chat apps"): one bot,
// several agents.
//
//	GET    /api/chat-apps                           viewer: the apps, who answers where, the known people
//	POST   /api/chat-apps                           admin, audited: connect a new app
//	PUT    /api/chat-apps/{id}/agents/{ns}/{agent}  builder, audited: where an agent answers and for whom
//	DELETE /api/chat-apps/{id}/agents/{ns}/{agent}  builder, audited
//	PUT    /api/chat-apps/{id}/default              admin, audited: which agent answers unnamed messages in a place
//	PUT    /api/chat-apps/{id}/auto                 admin, audited: whether a place picks the agent for them
//	GET    /api/chat-apps/{id}/places               builder: the platform's channels, by name
//
// The app itself (credentials, test, enable, delete, workspace pairing) uses the
// connection endpoints, where a shared app needs the admin role.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	maxRoutePlaces = 50
	maxRoutePeople = 100
	maxKnownPeople = 200
)

// routeStore is the part of postgresql.Store the shared apps use.
type routeStore interface {
	ListInboundRoutes(ctx context.Context, connectionID string) ([]postgresql.InboundRoute, error)
	SaveInboundRoute(ctx context.Context, connectionID string, r postgresql.InboundRoute, by string) error
	DeleteInboundRoute(ctx context.Context, connectionID, namespace, agent string) error
	SetInboundDefault(ctx context.Context, connectionID, place, namespace, agent string) error
}

// person is someone who has paired with a shared app, so builders can pick them
// by name instead of by id.
type person struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type chatAppView struct {
	inboundView
	Routes []postgresql.InboundRoute `json:"routes"`
	People []person                  `json:"people"`
	// AutoPlaces pick the agent for a message that names none.
	AutoPlaces []string `json:"autoPlaces"`
	// CanListPlaces: channels can be picked by name.
	CanListPlaces bool `json:"canListPlaces"`
}

func knownPeople(config json.RawMessage) []person {
	var c struct {
		People []person `json:"people"`
	}
	_ = json.Unmarshal(config, &c)
	if c.People == nil {
		return []person{}
	}
	return c.People
}

// rememberPerson adds or renames a person in a shared app's config.
func rememberPerson(config json.RawMessage, who person) (json.RawMessage, error) {
	var whole map[string]json.RawMessage
	if len(config) > 0 {
		if err := json.Unmarshal(config, &whole); err != nil {
			return nil, err
		}
	}
	if whole == nil {
		whole = map[string]json.RawMessage{}
	}
	people := knownPeople(config)
	i := slices.IndexFunc(people, func(p person) bool { return strings.EqualFold(p.ID, who.ID) })
	switch {
	case i >= 0:
		if who.Name != "" {
			people[i].Name = who.Name
		}
	case len(people) >= maxKnownPeople:
		return nil, errors.New("the app has the most people it can remember")
	default:
		people = append(people, who)
	}
	b, err := json.Marshal(people)
	if err != nil {
		return nil, err
	}
	whole["people"] = b
	return json.Marshal(whole)
}

func (s *UIServer) registerChatAppRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/chat-apps", s.handleChatApps)
	mux.HandleFunc("/api/chat-apps/{id}/agents/{ns}/{agent}", s.handleChatAppAgent)
	mux.HandleFunc("/api/chat-apps/{id}/default", s.handleChatAppDefault)
	mux.HandleFunc("/api/chat-apps/{id}/auto", s.handleChatAppAuto)
	mux.HandleFunc("/api/chat-apps/{id}/places", s.handleChatAppPlaces)
}

func (s *UIServer) routeSource() routeStore {
	if r, ok := s.inboundSource().(routeStore); ok {
		return r
	}
	return nil
}

func (s *UIServer) chatAppViewOf(ctx context.Context, c postgresql.InboundConnection) (chatAppView, error) {
	v := chatAppView{inboundView: s.inboundViewOf(c), Routes: []postgresql.InboundRoute{}, People: knownPeople(c.Config),
		AutoPlaces: autoPlaces(c.Config)}
	if a, ok := s.inboundPipe.Registry.Get(c.Kind); ok {
		_, v.CanListPlaces = a.(inbound.Directory)
	}
	routes, err := s.routeSource().ListInboundRoutes(pgScope(ctx), c.ID)
	if err != nil {
		return v, err
	}
	for _, r := range routes {
		if namespaceAllowed(ctx, r.Namespace) {
			v.Routes = append(v.Routes, r)
		}
	}
	return v, nil
}

// chatApp loads a shared app of the caller's tenant.
func (s *UIServer) chatApp(w http.ResponseWriter, r *http.Request, role tenancy.Role) (postgresql.InboundConnection, bool) {
	store, ok := s.inboundPreflight(w, r, role)
	if !ok {
		return postgresql.InboundConnection{}, false
	}
	c, ok := s.ownedInbound(w, r, store)
	if !ok {
		return c, false
	}
	if !c.Shared() {
		jsonError(w, "not found", http.StatusNotFound)
		return c, false
	}
	return c, true
}

func (s *UIServer) handleChatApps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listChatApps(w, r)
	case http.MethodPost:
		s.createChatApp(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *UIServer) listChatApps(w http.ResponseWriter, r *http.Request) {
	store, ok := s.inboundPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	// Every namespace of the tenant: shared apps have none, and the store's
	// scope keeps to the tenant.
	all, err := store.ListInboundConnections(pgScope(r.Context()), nil, "")
	if err != nil {
		inboundFail(w, "listing chat apps", err)
		return
	}
	t, scoped := TenantFromContext(r.Context())
	var ids []string
	var apps []postgresql.InboundConnection
	for _, c := range all {
		if c.Shared() && (!scoped || t == nil || t.TenantID == "" || c.TenantID == t.TenantID) {
			apps = append(apps, c)
			ids = append(ids, c.ID)
		}
	}
	refused, err := store.InboundRefusals(pgScope(r.Context()), ids, time.Now().Add(-refusalWindow))
	if err != nil {
		slog.Warn("counting refused messages", "err", err)
	}
	out := make([]chatAppView, 0, len(apps))
	for _, c := range apps {
		v, err := s.chatAppViewOf(r.Context(), c)
		if err != nil {
			inboundFail(w, "listing chat apps", err)
			return
		}
		v.Refused = refused[c.ID]
		out = append(out, v)
	}
	jsonResponse(w, out)
}

func (s *UIServer) createChatApp(w http.ResponseWriter, r *http.Request) {
	store, ok := s.inboundPreflight(w, r, tenancy.RoleAdmin)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if !readJSONBody(w, r, maxInboundBody, &req) {
		return
	}
	a, known := s.inboundPipe.Registry.Get(req.Kind)
	if !known || len(a.Capabilities().Modes) == 0 {
		jsonError(w, "choose a chat platform", http.StatusBadRequest)
		return
	}
	// Plain channel messages stay off: with several agents in a channel, every
	// message would start a run.
	modes := slices.DeleteFunc(slices.Clone(a.Capabilities().Modes), func(m inbound.Mode) bool { return m == inbound.ModeChannel })
	config, err := json.Marshal(map[string]any{"modes": modes})
	if err != nil {
		jsonError(w, "creating the app failed", http.StatusInternalServerError)
		return
	}
	if !s.checkInboundConfig(w, req.Kind, config) {
		return
	}
	id, err := postgresql.NewInboundID()
	if err != nil {
		jsonError(w, "creating the id failed", http.StatusInternalServerError)
		return
	}
	draft := postgresql.InboundConnection{ID: id, Kind: req.Kind, Name: strings.TrimSpace(req.Name), Config: config, CreatedBy: actorOf(r.Context())}
	if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		draft.TenantID = t.TenantID
	}
	c, err := store.CreateInboundConnection(pgScope(r.Context()), draft)
	if err != nil {
		inboundFail(w, "creating the app", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditCreate, kindInbound, "", "", c.ID, fmt.Sprintf("connected %s chat app %q", c.Kind, c.Name)))
	v := chatAppView{inboundView: s.inboundViewOf(c), Routes: []postgresql.InboundRoute{}, People: []person{},
		AutoPlaces: []string{}}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(v)
}

// routeAgent checks that the caller may put an agent into a chat app: it is one
// of theirs, published and not a template.
func (s *UIServer) routeAgent(w http.ResponseWriter, r *http.Request, ns, name string) bool {
	if !namespaceAllowed(r.Context(), ns) {
		jsonError(w, "not found", http.StatusNotFound)
		return false
	}
	agent, ok := s.getAgentFor(w, r, ns, name)
	if !ok {
		return false
	}
	if agent.Labels[LabelTemplate] == templateMarker {
		jsonError(w, "this agent is a template: make your own copy first", http.StatusConflict)
		return false
	}
	published, err := s.agentPublished(r.Context(), agent.Namespace, agent.Name)
	if err != nil {
		jsonError(w, "checking the agent failed", http.StatusInternalServerError)
		return false
	}
	if !published {
		jsonError(w, "publish the agent before adding it to a chat app", http.StatusConflict)
		return false
	}
	return true
}

// cleanIDs checks a list of exact ids: no wildcards, no blanks, no repeats.
func cleanIDs(what string, in []string, limit int) ([]string, error) {
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || strings.ContainsAny(v, "*?, \t\r\n") || len(v) > 200 {
			return nil, fmt.Errorf("%s holds exact ids only: %q is not one", what, v)
		}
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, v) }) {
			out = append(out, v)
		}
	}
	if len(out) > limit {
		return nil, fmt.Errorf("%s has at most %d entries", what, limit)
	}
	return out, nil
}

func checkRoute(in postgresql.InboundRoute) (postgresql.InboundRoute, error) {
	var err error
	if in.AllowSenders, err = cleanIDs("allowSenders", in.AllowSenders, maxRoutePeople); err != nil {
		return in, err
	}
	if in.Approvers, err = cleanIDs("approvers", in.Approvers, maxRoutePeople); err != nil {
		return in, err
	}
	for _, a := range in.Approvers {
		if !slices.ContainsFunc(in.AllowSenders, func(o string) bool { return strings.EqualFold(o, a) }) {
			return in, fmt.Errorf("an approver must also be allowed to ask: %q is not", a)
		}
	}
	if len(in.Places) > maxRoutePlaces {
		return in, fmt.Errorf("an agent answers in at most %d places", maxRoutePlaces)
	}
	seen := map[string]bool{}
	places := []postgresql.InboundPlace{}
	for _, p := range in.Places {
		p.ID, p.Name = strings.TrimSpace(p.ID), strings.TrimSpace(p.Name)
		if strings.ContainsAny(p.ID, "*?, \t\r\n#") || len(p.ID) > 200 || len(p.Name) > 100 {
			return in, fmt.Errorf("places are exact channel ids: %q is not one", p.ID)
		}
		if !seen[strings.ToLower(p.ID)] {
			seen[strings.ToLower(p.ID)] = true
			places = append(places, p)
		}
	}
	in.Places = places
	return in, nil
}

func (s *UIServer) handleChatAppAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, ok := s.chatApp(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	ns, agent := r.PathValue("ns"), r.PathValue("agent")
	var summary string
	if r.Method == http.MethodDelete {
		// No agent check: an agent that is gone or unpublished must be removable.
		if !namespaceAllowed(r.Context(), ns) {
			jsonError(w, "not found", http.StatusNotFound)
			return
		}
		if err := s.routeSource().DeleteInboundRoute(pgScope(r.Context()), c.ID, ns, agent); err != nil {
			routeFail(w, "removing the agent", err)
			return
		}
		summary = fmt.Sprintf("removed agent %s from chat app %q", agent, c.Name)
	} else {
		var req struct {
			Places       []postgresql.InboundPlace `json:"places"`
			AllowSenders []string                  `json:"allowSenders"`
			Approvers    []string                  `json:"approvers"`
		}
		if !readJSONBody(w, r, maxInboundBody, &req) || !s.routeAgent(w, r, ns, agent) {
			return
		}
		route, err := checkRoute(postgresql.InboundRoute{Namespace: ns, Agent: agent, AllowSenders: req.AllowSenders, Approvers: req.Approvers, Places: req.Places})
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.routeSource().SaveInboundRoute(pgScope(r.Context()), c.ID, route, actorOf(r.Context())); err != nil {
			routeFail(w, "saving", err)
			return
		}
		summary = fmt.Sprintf("changed where agent %s answers in chat app %q: %d places, %d people, %d approvers",
			agent, c.Name, len(route.Places), len(route.AllowSenders), len(route.Approvers))
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindInbound, ns, agent, c.ID, summary))
	s.writeChatApp(w, r, c)
}

func (s *UIServer) writeChatApp(w http.ResponseWriter, r *http.Request, c postgresql.InboundConnection) {
	v, err := s.chatAppViewOf(r.Context(), c)
	if err != nil {
		inboundFail(w, "reading the chat app", err)
		return
	}
	jsonResponse(w, v)
}

func routeFail(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, postgresql.ErrRouteNotFound) {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	inboundFail(w, what, err)
}

// handleChatAppDefault moves unnamed messages in a place, direct messages
// included, to another agent. Only an administrator may: it changes who answers
// everyone who writes there.
func (s *UIServer) handleChatAppDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, ok := s.chatApp(w, r, tenancy.RoleAdmin)
	if !ok {
		return
	}
	var req struct {
		Place     string `json:"place"`
		Namespace string `json:"namespace"`
		Agent     string `json:"agent"`
	}
	if !readJSONBody(w, r, maxInboundBody, &req) {
		return
	}
	if !namespaceAllowed(r.Context(), req.Namespace) {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.routeSource().SetInboundDefault(pgScope(r.Context()), c.ID, req.Place, req.Namespace, req.Agent); err != nil {
		routeFail(w, "setting the default", err)
		return
	}
	where := "direct messages"
	if req.Place != postgresql.DirectPlace {
		where = "channel " + req.Place
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindInbound, req.Namespace, req.Agent, c.ID,
		fmt.Sprintf("made agent %s the default for %s in chat app %q", req.Agent, where, c.Name)))
	s.writeChatApp(w, r, c)
}

func autoPlaces(config json.RawMessage) []string {
	var c struct {
		AutoPlaces []string `json:"autoPlaces"`
	}
	_ = json.Unmarshal(config, &c)
	if c.AutoPlaces == nil {
		return []string{}
	}
	return c.AutoPlaces
}

// setAutoPlace turns picking on or off for a place in a shared app's config.
func setAutoPlace(config json.RawMessage, place string, auto bool) (json.RawMessage, error) {
	var whole map[string]json.RawMessage
	if len(config) > 0 {
		if err := json.Unmarshal(config, &whole); err != nil {
			return nil, err
		}
	}
	if whole == nil {
		whole = map[string]json.RawMessage{}
	}
	places := slices.DeleteFunc(autoPlaces(config), func(p string) bool { return strings.EqualFold(p, place) })
	if auto {
		places = append(places, place)
	}
	b, err := json.Marshal(places)
	if err != nil {
		return nil, err
	}
	whole["autoPlaces"] = b
	return json.Marshal(whole)
}

// handleChatAppAuto sets whether a place, direct messages included, picks the
// agent for a message that names none. Only an administrator may: it changes
// who answers everyone who writes there.
func (s *UIServer) handleChatAppAuto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, ok := s.chatApp(w, r, tenancy.RoleAdmin)
	if !ok {
		return
	}
	var req struct {
		Place string `json:"place"`
		Auto  bool   `json:"auto"`
	}
	if !readJSONBody(w, r, maxInboundBody, &req) {
		return
	}
	// Turning it off needs no agent there: one that is gone must not leave it on.
	if req.Auto {
		routes, err := s.routeSource().ListInboundRoutes(pgScope(r.Context()), c.ID)
		if err != nil {
			inboundFail(w, "reading the routes", err)
			return
		}
		if !slices.ContainsFunc(routes, func(x postgresql.InboundRoute) bool {
			return namespaceAllowed(r.Context(), x.Namespace) &&
				slices.ContainsFunc(x.Places, func(p postgresql.InboundPlace) bool { return strings.EqualFold(p.ID, req.Place) })
		}) {
			jsonError(w, "no agent answers there", http.StatusNotFound)
			return
		}
	}
	config, err := setAutoPlace(c.Config, req.Place, req.Auto)
	if err != nil {
		jsonError(w, "saving failed", http.StatusInternalServerError)
		return
	}
	store, ok := s.inboundPreflight(w, r, tenancy.RoleAdmin)
	if !ok {
		return
	}
	next, err := store.UpdateInboundConnection(pgScope(r.Context()), c.ID, postgresql.InboundPatch{Config: &config})
	if err != nil {
		inboundFail(w, "saving", err)
		return
	}
	where := "direct messages"
	if req.Place != postgresql.DirectPlace {
		where = "channel " + req.Place
	}
	verb := "stopped picking agents automatically"
	if req.Auto {
		verb = "started picking agents automatically"
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindInbound, "", "", c.ID,
		fmt.Sprintf("%s in %s in chat app %q", verb, where, c.Name)))
	s.writeChatApp(w, r, next)
}

func (s *UIServer) handleChatAppPlaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc, ok := s.chatApp(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	a, _ := s.inboundPipe.Registry.Get(sc.Kind)
	dir, ok := a.(inbound.Directory)
	if !ok {
		jsonError(w, "this platform cannot list its channels: add them by sending a code", http.StatusConflict)
		return
	}
	c, err := s.inboundPipe.Resolve(r.Context(), sc)
	if err != nil {
		jsonError(w, "the app's credentials cannot be used: "+err.Error(), http.StatusConflict)
		return
	}
	places, err := dir.ListPlaces(r.Context(), c)
	if err != nil {
		jsonError(w, "listing channels failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if places == nil {
		places = []inbound.Place{}
	}
	jsonResponse(w, places)
}

// pairingNames asks the platform what the sender and channel are called, when it
// can. Best effort: the ids are always shown.
func (s *UIServer) pairingNames(ctx context.Context, sc postgresql.InboundConnection, p postgresql.InboundPairing) (sender, channel string) {
	sender = p.SenderName
	a, ok := s.inboundPipe.Registry.Get(sc.Kind)
	dir, isDir := a.(inbound.Directory)
	if !ok || !isDir || p.ReceivedAt == nil {
		return sender, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := s.inboundPipe.Resolve(ctx, sc)
	if err != nil {
		return sender, ""
	}
	if sender == "" {
		if n, err := dir.PersonName(ctx, c, p.SenderID); err == nil {
			sender = n
		}
	}
	if p.Mode != string(inbound.ModeDirect) {
		if places, err := dir.ListPlaces(ctx, c); err == nil {
			if i := slices.IndexFunc(places, func(x inbound.Place) bool { return strings.EqualFold(x.ID, p.ChannelID) }); i >= 0 {
				channel = places[i].Name
			}
		}
	}
	return sender, channel
}

// approveShared adds a paired sender to a shared app: always to the people it
// knows, and, for a code made from an agent's page, to that agent's route along
// with the place the code came from.
func (s *UIServer) approveShared(
	w http.ResponseWriter, r *http.Request, store inboundStore, cur postgresql.InboundConnection, p postgresql.InboundPairing,
) (postgresql.InboundConnection, bool) {
	name, channel := s.pairingNames(r.Context(), cur, p)
	config, err := rememberPerson(cur.Config, person{ID: p.SenderID, Name: name})
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return cur, false
	}
	if p.Agent != "" {
		routes, err := s.routeSource().ListInboundRoutes(pgScope(r.Context()), cur.ID)
		if err != nil {
			inboundFail(w, "reading the routes", err)
			return cur, false
		}
		route := postgresql.InboundRoute{Namespace: p.Namespace, Agent: p.Agent}
		for _, x := range routes {
			if x.Namespace == p.Namespace && x.Agent == p.Agent {
				route = x
			}
		}
		route.AllowSenders = append(route.AllowSenders, p.SenderID)
		place := postgresql.InboundPlace{ID: postgresql.DirectPlace}
		if p.Mode != string(inbound.ModeDirect) {
			place = postgresql.InboundPlace{ID: p.ChannelID, Name: channel}
		}
		if !slices.ContainsFunc(route.Places, func(x postgresql.InboundPlace) bool { return strings.EqualFold(x.ID, place.ID) }) {
			route.Places = append(route.Places, place)
		}
		if route, err = checkRoute(route); err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return cur, false
		}
		if err := s.routeSource().SaveInboundRoute(pgScope(r.Context()), cur.ID, route, actorOf(r.Context())); err != nil {
			routeFail(w, "adding the person", err)
			return cur, false
		}
	}
	c, err := store.UpdateInboundConnection(pgScope(r.Context()), cur.ID, postgresql.InboundPatch{Config: &config})
	if err != nil {
		inboundFail(w, "adding the person", err)
		return cur, false
	}
	return c, true
}

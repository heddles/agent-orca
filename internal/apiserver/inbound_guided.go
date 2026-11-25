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

// Guided channel setup (docs/inbound-channels.md, "Guided setup").
//
//	PUT    /api/inbound-connections/{id}/credentials              admin, audited: store credentials, never returned
//	POST   /api/inbound-connections/{id}/pairings                 builder: a one-time code
//	GET    /api/inbound-connections/{id}/pairings/{pairing}       builder: whether someone sent it
//	POST   /api/inbound-connections/{id}/pairings/{pairing}/approve  builder, audited: allow that sender
//	DELETE /api/inbound-connections/{id}/pairings/{pairing}       builder: discard

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

	"github.com/heddles/agent-orca/internal/connections"
	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	credentialsStored = "stored"
	credentialsSecret = "secret"
	pairingWaiting    = "waiting"
	// maxCredentialBytes bounds one value: the largest is a service account key file.
	maxCredentialBytes = 16 << 10
	// inboundNoteEvery spaces out the writes about refused requests, which anyone
	// who knows a connection's address can cause.
	inboundNoteEvery = 30 * time.Second
)

// SetInboundKeys lets administrators enter channel credentials in the UI; they
// are sealed under the connection keyring. nil leaves only Kubernetes Secrets.
func (s *UIServer) SetInboundKeys(k *connections.Keyring) { s.inboundKeys = k }

func credentialsBinding(c postgresql.InboundConnection) []byte {
	return connections.InboundCredentialsAAD(c.TenantID, c.ID)
}

// OpenCredentials opens the credentials entered for a connection. The sealed
// value is bound to the tenant and the connection, so it opens for no other row.
func (b *InboundBridge) OpenCredentials(c postgresql.InboundConnection) (map[string][]byte, error) {
	if b.s.inboundKeys == nil {
		return nil, errors.New("the connection encryption key is not set up, so the stored credentials cannot be read")
	}
	raw, err := b.s.inboundKeys.Open(c.CredentialsKeyID, c.CredentialsEnc, credentialsBinding(c))
	if err != nil {
		return nil, errors.New("the stored credentials could not be decrypted")
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, errors.New("the stored credentials are damaged")
	}
	out := make(map[string][]byte, len(values))
	for k, v := range values {
		out[k] = []byte(v)
	}
	return out, nil
}

// checkCredentialValues keeps what the adapter declares, and requires what it
// requires. It never echoes a value.
func checkCredentialValues(a inbound.Adapter, in map[string]string) (map[string]string, error) {
	d, ok := a.(inbound.Describer)
	if !ok || len(d.Describe().SecretKeys) == 0 {
		return nil, errors.New("credentials for this kind of channel cannot be entered here")
	}
	out := map[string]string{}
	known := []string{}
	for _, k := range d.Describe().SecretKeys {
		known = append(known, k.Key)
		v := strings.TrimSpace(in[k.Key])
		switch {
		case v == "" && k.Required:
			return nil, fmt.Errorf("%s is required", k.Key)
		case len(v) > maxCredentialBytes:
			return nil, fmt.Errorf("%s is too long", k.Key)
		case v != "":
			out[k.Key] = v
		}
	}
	for k := range in {
		if !slices.Contains(known, k) {
			return nil, fmt.Errorf("this kind of channel takes only %s", strings.Join(known, ", "))
		}
	}
	return out, nil
}

// handleInboundCredentials stores credentials an administrator entered. Only an
// administrator may: they include where the operator connects to and, for some
// channels, switches that relax that connection's protections.
func (s *UIServer) handleInboundCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.inboundPreflight(w, r, tenancy.RoleAdmin)
	if !ok {
		return
	}
	var req struct {
		Values map[string]string `json:"values"`
	}
	if !readJSONBody(w, r, maxInboundBody, &req) {
		return
	}
	cur, ok := s.ownedInbound(w, r, store)
	if !ok {
		return
	}
	if s.inboundKeys == nil {
		jsonError(w, "this installation cannot store credentials: set up the connection encryption key (database.connectionKeySecret), or use a Kubernetes Secret", http.StatusServiceUnavailable)
		return
	}
	a, known := s.inboundPipe.Registry.Get(cur.Kind)
	if !known {
		jsonError(w, "unknown kind of channel", http.StatusConflict)
		return
	}
	values, err := checkCredentialValues(a, req.Values)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	plain, err := json.Marshal(values)
	if err != nil {
		jsonError(w, "saving the credentials failed", http.StatusInternalServerError)
		return
	}
	next := cur
	next.SecretRef = ""
	next.CredentialsKeyID, next.CredentialsEnc, err = s.inboundKeys.Seal(plain, credentialsBinding(cur))
	if err != nil {
		jsonError(w, "encrypting the credentials failed", http.StatusInternalServerError)
		return
	}
	// Looking up what they allow also shows whether the platform accepts them;
	// credentials it refuses are not saved.
	config, ok := s.completeInboundConfig(w, r.Context(), next)
	if !ok {
		return
	}
	c, err := store.SetInboundCredentials(pgScope(r.Context()), cur.ID, next.CredentialsKeyID, next.CredentialsEnc, config)
	if err != nil {
		inboundFail(w, "storing the credentials", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindInbound, c.Namespace, c.Agent, c.ID,
		fmt.Sprintf("replaced the credentials of %s channel %q", c.Kind, c.Name)))
	jsonResponse(w, s.inboundViewOf(c))
}

// noteInboundRequest records whether the channel is reaching us. A refusal is
// written at most every inboundNoteEvery per connection.
func (s *UIServer) noteInboundRequest(ctx context.Context, c postgresql.InboundConnection, authError string) {
	store := s.inboundSource()
	if store == nil {
		return
	}
	if authError != "" {
		if last, ok := s.inboundSeen.Load(c.ID); ok && time.Since(last.(time.Time)) < inboundNoteEvery {
			return
		}
		s.inboundSeen.Store(c.ID, time.Now())
	} else {
		s.inboundSeen.Delete(c.ID)
		if c.LastAuthError == "" && c.LastRequestAt != nil && time.Since(*c.LastRequestAt) < inboundNoteEvery {
			return
		}
	}
	// Not the request's context: an answer already on its way must not lose this.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := store.RecordInboundRequest(postgresql.WithTenant(wctx, c.TenantID), c.ID, authError); err != nil {
		slog.Warn("recording an inbound request", "connection", c.ID, "err", err)
	}
}

// pairingView is a pairing as a browser sees it. Code is returned only when it
// is made: only its digest is kept.
type pairingView struct {
	ID        string    `json:"id"`
	Code      string    `json:"code,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Status is "waiting" until a message with the code arrives, then "received".
	Status     string `json:"status"`
	SenderID   string `json:"senderId,omitempty"`
	SenderName string `json:"senderName,omitempty"`
	ChannelID  string `json:"channelId,omitempty"`
	// ChannelName is filled when the platform can name it.
	ChannelName string `json:"channelName,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Direct      bool   `json:"direct,omitempty"`
}

func pairingViewOf(p postgresql.InboundPairing) pairingView {
	v := pairingView{ID: p.ID, ExpiresAt: p.ExpiresAt, Status: pairingWaiting}
	if p.ReceivedAt != nil {
		v.Status, v.SenderID, v.SenderName, v.ChannelID = "received", p.SenderID, p.SenderName, p.ChannelID
		v.Direct = p.Mode == string(inbound.ModeDirect)
	}
	return v
}

func pairingFail(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, postgresql.ErrPairingNotFound) {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	inboundFail(w, what, err)
}

func (s *UIServer) handleInboundPairings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.inboundPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req struct {
		// Namespace and Agent: on a shared app, the agent the person is for.
		Namespace string `json:"namespace"`
		Agent     string `json:"agent"`
	}
	if r.ContentLength != 0 && !readJSONBody(w, r, maxInboundBody, &req) {
		return
	}
	cur, ok := s.ownedInbound(w, r, store)
	if !ok {
		return
	}
	switch {
	case !cur.Shared() && req.Agent != "":
		jsonError(w, "this channel belongs to one agent", http.StatusBadRequest)
		return
	case cur.Shared() && req.Agent == "":
		// A code for the app itself makes someone known to every builder.
		if !requireRole(w, r, tenancy.RoleAdmin) {
			return
		}
	case cur.Shared():
		if !s.routeAgent(w, r, req.Namespace, req.Agent) {
			return
		}
	}
	if a, known := s.inboundPipe.Registry.Get(cur.Kind); !known || len(a.Capabilities().Modes) == 0 {
		jsonError(w, "pairing is for chat channels", http.StatusConflict)
		return
	}
	if len(cur.CredentialsEnc) == 0 && cur.SecretRef == "" {
		jsonError(w, "add the credentials first: without them no message can be checked", http.StatusConflict)
		return
	}
	code, err := inbound.NewPairingCode()
	if err != nil {
		jsonError(w, "making the code failed", http.StatusInternalServerError)
		return
	}
	id, err := postgresql.NewInboundID()
	if err != nil {
		jsonError(w, "making the code failed", http.StatusInternalServerError)
		return
	}
	p := postgresql.InboundPairing{
		ID: id, TenantID: cur.TenantID, ConnectionID: cur.ID, CodeHash: inbound.PairingHash(cur.ID, code),
		CreatedBy: actorOf(r.Context()), ExpiresAt: time.Now().Add(inbound.PairingTTL),
		Namespace: req.Namespace, Agent: req.Agent,
	}
	if err := store.CreateInboundPairing(pgScope(r.Context()), p); err != nil {
		inboundFail(w, "creating the code", err)
		return
	}
	v := pairingViewOf(p)
	v.Code = code
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *UIServer) handleInboundPairing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.inboundPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	cur, ok := s.ownedInbound(w, r, store)
	if !ok {
		return
	}
	held, err := store.GetInboundPairing(pgScope(r.Context()), cur.ID, r.PathValue("pairing"))
	if err != nil {
		pairingFail(w, "reading the code", err)
		return
	}
	if cur.Shared() && held.Agent == "" && !requireRole(w, r, tenancy.RoleAdmin) {
		return
	}
	if r.Method == http.MethodDelete {
		if _, err := store.TakeInboundPairing(pgScope(r.Context()), cur.ID, r.PathValue("pairing")); err != nil {
			pairingFail(w, "discarding the code", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	v := pairingViewOf(held)
	if held.ReceivedAt != nil {
		v.SenderName, v.ChannelName = s.pairingNames(r.Context(), cur, held)
	}
	v.Agent = held.Agent
	jsonResponse(w, v)
}

// allowSender adds the sender (and, for a message from a channel when the
// connection lists channels, that channel) to a stored config.
func allowSender(config json.RawMessage, p postgresql.InboundPairing) (json.RawMessage, error) {
	var whole map[string]json.RawMessage
	if len(config) > 0 {
		if err := json.Unmarshal(config, &whole); err != nil {
			return nil, err
		}
	}
	if whole == nil {
		whole = map[string]json.RawMessage{}
	}
	add := func(key, value string) error {
		var list []string
		if raw, ok := whole[key]; ok {
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
		}
		if value != "" && !slices.ContainsFunc(list, func(v string) bool { return strings.EqualFold(v, value) }) {
			list = append(list, value)
		}
		b, err := json.Marshal(list)
		whole[key] = b
		return err
	}
	if err := add("allowSenders", p.SenderID); err != nil {
		return nil, err
	}
	if p.Mode != string(inbound.ModeDirect) {
		var channels []string
		if raw, ok := whole["allowChannels"]; ok {
			if err := json.Unmarshal(raw, &channels); err != nil {
				return nil, err
			}
		}
		if len(channels) > 0 {
			if err := add("allowChannels", p.ChannelID); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(whole)
}

// handleInboundPairingApprove allows the person who sent the code, and tells them
// so in the chat, which also proves that answers get back.
func (s *UIServer) handleInboundPairingApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.inboundPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	cur, ok := s.ownedInbound(w, r, store)
	if !ok {
		return
	}
	p, err := store.GetInboundPairing(pgScope(r.Context()), cur.ID, r.PathValue("pairing"))
	if err == nil && p.ReceivedAt == nil {
		jsonError(w, "nobody has sent the code yet", http.StatusConflict)
		return
	}
	if err == nil && cur.Shared() {
		if p.Agent == "" && !requireRole(w, r, tenancy.RoleAdmin) {
			return
		}
		if p.Agent != "" && !s.routeAgent(w, r, p.Namespace, p.Agent) {
			return
		}
	}
	if err == nil {
		// Taken before the change, so two approvals cannot both act.
		p, err = store.TakeInboundPairing(pgScope(r.Context()), cur.ID, p.ID)
	}
	if err != nil {
		pairingFail(w, "approving", err)
		return
	}
	var c postgresql.InboundConnection
	if cur.Shared() {
		if c, ok = s.approveShared(w, r, store, cur, p); !ok {
			return
		}
	} else {
		config, err := allowSender(cur.Config, p)
		if err != nil {
			jsonError(w, "the channel's settings could not be changed", http.StatusInternalServerError)
			return
		}
		if !s.checkInboundConfig(w, cur.Kind, config) {
			return
		}
		if c, err = store.UpdateInboundConnection(pgScope(r.Context()), cur.ID, postgresql.InboundPatch{Config: &config}); err != nil {
			inboundFail(w, "allowing the sender", err)
			return
		}
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindInbound, c.Namespace, c.Agent, c.ID,
		fmt.Sprintf("allowed a paired sender on %s channel %q", c.Kind, c.Name)))
	out := struct {
		inboundView
		// ReplySent: the confirmation reached the chat. ReplyError says why not.
		ReplySent  bool   `json:"replySent"`
		ReplyError string `json:"replyError,omitempty"`
	}{inboundView: s.inboundViewOf(c)}
	if err := s.confirmPairing(r.Context(), c, p); err != nil {
		slog.Warn("confirming a pairing", "connection", c.ID, "err", err)
		out.ReplyError = err.Error()
	} else {
		out.ReplySent = true
	}
	jsonResponse(w, out)
}

func (s *UIServer) confirmPairing(ctx context.Context, sc postgresql.InboundConnection, p postgresql.InboundPairing) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := s.inboundPipe.Resolve(ctx, sc)
	if err != nil {
		return err
	}
	a, ok := s.inboundPipe.Registry.Get(sc.Kind)
	if !ok || len(p.ReplyTarget) == 0 {
		return errors.New("there is nowhere to send the reply")
	}
	return a.Reply(ctx, c, p.ReplyTarget, inbound.Result{Text: "You're connected. Send me a message to try it.", Notice: true})
}

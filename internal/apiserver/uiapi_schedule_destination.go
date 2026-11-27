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

// Changing where the reports of a schedule managed outside the UI go
// (docs/schedules.md). The schedule itself is not written: Git keeps owning it,
// and the choice is kept in the database.
//
//	PUT    /api/schedules/{ns}/{name}/destination  admin, audited: {app, channelID, channelName}
//	DELETE /api/schedules/{ns}/{name}/destination  admin, audited: go back to what Git says

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const schedActionDestination = "destination"

var slackChannelIDRe = regexp.MustCompile(`^[CG][A-Z0-9]{2,30}$`)

type destinationStore interface {
	SaveScheduleDestination(ctx context.Context, d postgresql.ScheduleDestination) error
	ListScheduleDestinations(ctx context.Context) ([]postgresql.ScheduleDestination, error)
	DeleteScheduleDestination(ctx context.Context, tenantID, namespace, name string) (bool, error)
}

func (s *UIServer) destinationSource() destinationStore {
	if s.destinations != nil {
		return s.destinations
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

func destinationKey(tenantID, ns, name string) string { return tenantID + "/" + ns + "/" + name }

// chosenDestinations is every saved choice the caller's tenant can see; empty
// without a database.
func (s *UIServer) chosenDestinations(ctx context.Context) (map[string]postgresql.ScheduleDestination, error) {
	out := map[string]postgresql.ScheduleDestination{}
	store := s.destinationSource()
	if store == nil {
		return out, nil
	}
	all, err := store.ListScheduleDestinations(pgScope(ctx))
	if err != nil {
		return out, err
	}
	for _, d := range all {
		out[destinationKey(d.TenantID, d.Namespace, d.Name)] = d
	}
	return out, nil
}

// scheduleTenantID is the tenant that owns a schedule's namespace, as the
// controller finds it; "" when none does.
func (s *UIServer) scheduleTenantID(ctx context.Context, sc *agentorcav1alpha1.AgentSchedule) (string, error) {
	var tcs agentorcav1alpha1.TenantConfigList
	if err := s.crdClient.List(ctx, &tcs); err != nil {
		return "", err
	}
	tc := tenancy.TenantConfigFor(tcs.Items, sc)
	if tc == nil {
		return "", nil
	}
	return tenancy.TenantIDOf(tc), nil
}

// applyChosenDestination makes the list say where reports go now.
func (s *UIServer) applyChosenDestination(ctx context.Context, sc *agentorcav1alpha1.AgentSchedule, v *scheduleNotifyView,
	chosen map[string]postgresql.ScheduleDestination, readErr error) {
	if v == nil || tenancy.Detect(sc, s.ownDetect).Owner == tenancy.OwnerUI {
		return
	}
	if readErr != nil {
		v.Problem = "the channel chosen in the UI could not be read, so what is shown here may not be where reports go"
		return
	}
	if len(chosen) == 0 {
		return
	}
	tenantID, err := s.scheduleTenantID(ctx, sc)
	if err != nil {
		v.Problem = "the schedule's tenant could not be read, so what is shown here may not be where reports go"
		return
	}
	d, ok := chosen[destinationKey(tenantID, sc.Namespace, sc.Name)]
	if !ok {
		return
	}
	v.Override = &scheduleOverrideView{SetBy: d.SetBy, SetAt: d.SetAt.UTC().Format(time.RFC3339), Git: scheduleNotifyTarget{Channel: v.Channel, Slack: v.Slack}}
	v.Channel, v.Slack = "", &agentorcav1alpha1.ScheduleSlackTarget{App: d.App, ChannelID: d.ChannelID, ChannelName: d.ChannelName}
}

func (s *UIServer) handleScheduleDestination(w http.ResponseWriter, r *http.Request, sc *agentorcav1alpha1.AgentSchedule) {
	if !requireRole(w, r, tenancy.RoleAdmin) {
		return
	}
	if !canWrite(r.Context(), sc, kindAgentSchedule) {
		jsonError(w, "this schedule is shared by the platform and is read-only", http.StatusForbidden)
		return
	}
	store := s.destinationSource()
	if store == nil {
		jsonError(w, "choosing a channel here needs the database", http.StatusServiceUnavailable)
		return
	}
	tenantID, err := s.scheduleTenantID(r.Context(), sc)
	if err != nil {
		writeAPIError(w, "reading the schedule's tenant", err)
		return
	}
	// The row is keyed by the tenant the controller will look it up under.
	if t, ok := TenantFromContext(r.Context()); tenantID == "" || (ok && t != nil && t.TenantID != "" && t.TenantID != tenantID) {
		jsonError(w, "no tenant of yours owns this schedule", http.StatusConflict)
		return
	}
	if r.Method == http.MethodDelete {
		s.resetScheduleDestination(w, r, store, tenantID, sc)
		return
	}
	if tenancy.Detect(sc, s.ownDetect).Owner == tenancy.OwnerUI {
		jsonError(w, "this schedule is edited here: change the destination in its settings", http.StatusConflict)
		return
	}
	if sc.Spec.AgentRef == "" || sc.Spec.Result == nil || sc.Spec.Result.Notify == nil {
		jsonError(w, "this schedule sends no messages: add spec.result.notify in Git first, which says what is sent", http.StatusConflict)
		return
	}
	var in agentorcav1alpha1.ScheduleSlackTarget
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		jsonError(w, "the body must be {app, channelID, channelName}", http.StatusBadRequest)
		return
	}
	if !slackChannelIDRe.MatchString(in.ChannelID) {
		jsonError(w, "channelID must be Slack's id of a channel, like C0123456789", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(in.ChannelName) > 100 || strings.IndexFunc(in.ChannelName, unicode.IsControl) >= 0 {
		jsonError(w, "channelName is at most 100 characters, on one line", http.StatusBadRequest)
		return
	}
	if s.inboundPipe == nil {
		jsonError(w, "chat apps are not available on this installation", http.StatusServiceUnavailable)
		return
	}
	if err := s.inboundPipe.CheckApp(pgScope(r.Context()), tenantID, "slack", in.App); err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := store.SaveScheduleDestination(pgScope(r.Context()), postgresql.ScheduleDestination{
		TenantID: tenantID, Namespace: sc.Namespace, Name: sc.Name,
		App: in.App, ChannelID: in.ChannelID, ChannelName: in.ChannelName, SetBy: actorOf(r.Context()),
	}); err != nil {
		slog.Error("saving a schedule's destination", "schedule", sc.Namespace+"/"+sc.Name, "err", err)
		jsonError(w, "the channel could not be saved", http.StatusInternalServerError)
		return
	}
	to := schedule.WithSlackDestination(sc.Spec.Result.Notify, in)
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindAgentSchedule, sc.Namespace, sc.Name, sc.ResourceVersion,
		fmt.Sprintf("chose where AgentSchedule %s/%s sends its reports, in place of Git's: %s", sc.Namespace, sc.Name, to.Destination())))
	jsonResponse(w, map[string]string{"status": "saved"})
}

func (s *UIServer) resetScheduleDestination(w http.ResponseWriter, r *http.Request, store destinationStore, tenantID string, sc *agentorcav1alpha1.AgentSchedule) {
	gone, err := store.DeleteScheduleDestination(pgScope(r.Context()), tenantID, sc.Namespace, sc.Name)
	if err != nil {
		slog.Error("removing a schedule's destination", "schedule", sc.Namespace+"/"+sc.Name, "err", err)
		jsonError(w, "the saved channel could not be removed", http.StatusInternalServerError)
		return
	}
	if !gone {
		jsonResponse(w, map[string]string{"status": "unchanged"})
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindAgentSchedule, sc.Namespace, sc.Name, sc.ResourceVersion,
		fmt.Sprintf("reset where AgentSchedule %s/%s sends its reports to what Git says", sc.Namespace, sc.Name)))
	jsonResponse(w, map[string]string{"status": "reset"})
}

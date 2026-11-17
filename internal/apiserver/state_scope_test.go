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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/state"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func redisStateStore(t *testing.T) (state.Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	s, err := state.NewStoreFromConfig(state.Config{Backend: "redis", RedisURL: "redis://" + mr.Addr()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, mr
}

func labelledRun(name, tenantID string) *agentorcav1alpha1.AgentRun {
	r := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	if tenantID != "" {
		r.Labels = map[string]string{tenancy.LabelTenantID: tenantID}
	}
	return r
}

func TestTenantPrefixIsOnByDefault(t *testing.T) {
	t.Setenv(state.PrefixEnvVar, "")
	if !state.PrefixEnabled() {
		t.Error("tenant key prefixes must be on by default")
	}
	t.Setenv(state.PrefixEnvVar, "false")
	if state.PrefixEnabled() {
		t.Error("AGENTORCA_STATE_TENANT_PREFIX=false must turn the prefix off")
	}
}

// What the router writes under the run's tenant prefix is what the API reads
// and writes: the answer and cancel flag the API sets are found by a router view
// built from the same tenant ID, and the router's checkpoint is found by the API.
func TestAPIAndRouterAgreeOnARunsKeys(t *testing.T) {
	t.Setenv(state.PrefixEnvVar, "")
	raw, mr := redisStateStore(t)
	ctx := context.Background()
	run := labelledRun("r1", isoA)
	routerView := state.ForTenant(raw, isoA) // what cmd/model-router builds from cfg.TenantID

	// API writes, router reads.
	api := runState(raw, run)
	if err := api.SaveAnswer(ctx, "clarify-answer:ns:r1", "yes", time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := routerView.LoadAnswer(ctx, "clarify-answer:ns:r1"); got != "yes" {
		t.Errorf("router read answer %q, want yes", got)
	}
	if !mr.Exists(state.TenantPrefix(isoA) + "clarify-answer:ns:r1") {
		t.Errorf("answer not under the tenant prefix; keys = %v", mr.Keys())
	}
	if err := api.SignalCancel(ctx, "ns", "r1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := routerView.IsCancelled(ctx, "ns", "r1"); !ok {
		t.Error("router did not see the API's cancel flag")
	}

	// Router writes, API reads.
	if err := routerView.SaveToken(ctx, "tokens:ns:r1", "hello"); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists(state.TenantPrefix(isoA) + "tokens:ns:r1") {
		t.Errorf("token stream not under the tenant prefix; keys = %v", mr.Keys())
	}
	if err := routerView.SaveKV(ctx, "mcpapp", "k", []byte("<html>"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if b, _ := api.LoadKV(ctx, "mcpapp", "k"); string(b) != "<html>" {
		t.Errorf("API read mcp app %q", b)
	}

	// A run of another tenant with the same name is a different set of keys.
	other := runState(raw, labelledRun("r1", isoB))
	if ok, _ := other.IsCancelled(ctx, "ns", "r1"); ok {
		t.Error("another tenant's run saw this run's cancel flag")
	}
	if b, _ := other.LoadKV(ctx, "mcpapp", "k"); b != nil {
		t.Error("another tenant read this run's cached MCP app")
	}
}

// A run that started before the upgrade keeps its unprefixed keys readable.
func TestAPIReadsPreUpgradeUnprefixedKeys(t *testing.T) {
	t.Setenv(state.PrefixEnvVar, "")
	raw, _ := redisStateStore(t)
	ctx := context.Background()
	if err := raw.SaveAnswer(ctx, "answer:ns:old", "legacy", time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := runState(raw, labelledRun("old", isoA)).LoadAnswer(ctx, "answer:ns:old"); got != "legacy" {
		t.Errorf("fallback read %q, want legacy", got)
	}
}

// The escape hatch turns the API's prefix off with the operator's.
func TestRunStateFollowsTheEscapeHatch(t *testing.T) {
	t.Setenv(state.PrefixEnvVar, "false")
	raw, mr := redisStateStore(t)
	if err := runState(raw, labelledRun("r", isoA)).SaveAnswer(context.Background(), "answer:ns:r", "x", time.Hour); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists("answer:ns:r") {
		t.Errorf("prefix off must write the plain key; keys = %v", mr.Keys())
	}
}

// The stop handler signals cancel under the run's tenant prefix, where the
// router looks.
func TestStopRunSignalsCancelUnderTheTenantPrefix(t *testing.T) {
	t.Setenv(state.PrefixEnvVar, "")
	raw, mr := redisStateStore(t)
	run := labelledRun("r1", isoA)
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseRunning
	c := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(run).WithStatusSubresource(run).Build()
	s := &UIServer{crdClient: c, store: raw}

	rec := httptest.NewRecorder()
	s.handleStopRun(rec, httptest.NewRequest(http.MethodPost, "/", nil), "ns", "r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop = %d: %s", rec.Code, rec.Body)
	}
	if ok, _ := state.ForTenant(raw, isoA).IsCancelled(context.Background(), "ns", "r1"); !ok {
		t.Errorf("router view did not see the cancel; keys = %v", mr.Keys())
	}
}

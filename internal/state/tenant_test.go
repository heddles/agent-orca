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

package state

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

const (
	tidA = "tnt_01935c7e-0000-7000-8000-00000000000a"
	tidB = "tnt_01935c7e-0000-7000-8000-00000000000b"
)

func tenantTestStore(t *testing.T) (Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	s, err := newRedisStore(Config{RedisURL: "redis://" + mr.Addr()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, mr
}

func TestForTenantEmptyIDIsTheStoreItself(t *testing.T) {
	s, _ := tenantTestStore(t)
	if ForTenant(s, "") != s {
		t.Error("an empty tenant ID must not wrap the store")
	}
}

func TestTenantsDoNotSeeEachOthersKeys(t *testing.T) {
	raw, mr := tenantTestStore(t)
	ctx := context.Background()
	a, b := ForTenant(raw, tidA), ForTenant(raw, tidB)
	key := "agentorca/runs/same-name/state"

	msgs := []json.RawMessage{json.RawMessage(`{"role":"user","content":"a-secret"}`)}
	if err := a.SaveMessages(ctx, key, msgs, time.Hour); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists(TenantPrefix(tidA) + key) {
		t.Fatalf("checkpoint not under the tenant prefix; keys = %v", mr.Keys())
	}
	if got, err := b.LoadMessages(ctx, key); err != nil || got != nil {
		t.Errorf("B read A's checkpoint: %v, %v", got, err)
	}
	if got, err := a.LoadMessages(ctx, key); err != nil || len(got) != 1 {
		t.Errorf("A lost its checkpoint: %v, %v", got, err)
	}

	if err := a.SaveSpend(ctx, key, 1.5, time.Hour); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.LoadSpend(ctx, key); v != 0 {
		t.Errorf("B read A's spend: %v", v)
	}
	if v, _ := a.LoadSpend(ctx, key); v != 1.5 {
		t.Errorf("A spend = %v", v)
	}

	stream := "tokens:ns:run"
	_ = a.SaveToken(ctx, stream, "hello")
	_ = a.SaveTraceEvent(ctx, stream, `{"type":"done"}`)
	if es, _ := b.ReadTraceEvents(ctx, stream); len(es) != 0 {
		t.Errorf("B read A's token stream: %v", es)
	}
	if es, _ := a.ReadTraceEvents(ctx, stream); len(es) == 0 {
		t.Error("A lost its token stream")
	}

	_ = a.SaveKV(ctx, "scope", "k", []byte("v"), time.Hour)
	if v, _ := b.LoadKV(ctx, "scope", "k"); v != nil {
		t.Errorf("B read A's KV: %s", v)
	}
	if ks, _ := b.ListKV(ctx, "scope"); len(ks) != 0 {
		t.Errorf("B lists A's KV: %v", ks)
	}
	if ks, _ := a.ListMessageKeys(ctx, "agentorca/runs/*/state"); !slices.Equal(ks, []string{key}) {
		t.Errorf("A lists %v, want the key without its prefix", ks)
	}
	if ks, _ := b.ListMessageKeys(ctx, "*"); len(ks) != 0 {
		t.Errorf("a broad pattern from B reached A's keys: %v", ks)
	}

	_ = a.SaveHTTPOutput(ctx, "run", "out-a")
	if v, _ := b.LoadHTTPOutput(ctx, "run"); v != "" {
		t.Errorf("B read A's http output: %q", v)
	}
	if v, _ := a.LoadHTTPOutput(ctx, "run"); v != "out-a" {
		t.Errorf("A http output = %q", v)
	}

	_ = a.SignalCancel(ctx, "ns", "run")
	if ok, _ := b.IsCancelled(ctx, "ns", "run"); ok {
		t.Error("A's cancel flag cancelled B's run")
	}
	if ok, _ := a.IsCancelled(ctx, "ns", "run"); !ok {
		t.Error("A's cancel flag lost")
	}
}

// Keys written before the upgrade are still read; the next write moves them.
func TestTenantViewFallsBackToOldKeysAndWritesNew(t *testing.T) {
	raw, mr := tenantTestStore(t)
	ctx := context.Background()
	a := ForTenant(raw, tidA)
	key := "agentorca/runs/old/state"

	msgs := []json.RawMessage{json.RawMessage(`{"role":"user","content":"x"}`)}
	_ = raw.SaveMessages(ctx, key, msgs, time.Hour)
	_ = raw.SaveSpend(ctx, key, 2.0, time.Hour)
	_ = raw.SaveToken(ctx, "tokens:ns:old", "t")
	_ = raw.SaveTraceEvent(ctx, "tokens:ns:old", `{"type":"toolCall"}`)
	_ = raw.SaveAnswer(ctx, "ans", "yes", time.Hour)
	_ = raw.SaveKV(ctx, "scope", "k", []byte("v"), time.Hour)

	if got, _ := a.LoadMessages(ctx, key); len(got) != 1 {
		t.Errorf("old checkpoint not read: %v", got)
	}
	if v, _ := a.LoadSpend(ctx, key); v != 2.0 {
		t.Errorf("old spend not read: %v", v)
	}
	if es, _ := a.ReadTraceEvents(ctx, "tokens:ns:old"); len(es) == 0 {
		t.Error("old stream not read")
	}
	if v, _ := a.LoadAnswer(ctx, "ans"); v != "yes" {
		t.Errorf("old answer = %q", v)
	}
	if v, _ := a.LoadKV(ctx, "scope", "k"); string(v) != "v" {
		t.Errorf("old kv = %q", v)
	}
	if ks, _ := a.ListMessageKeys(ctx, "agentorca/runs/*/state"); !slices.Contains(ks, key) {
		t.Errorf("old key not listed: %v", ks)
	}

	// Writing goes to the new key; reading then prefers it.
	_ = a.SaveSpend(ctx, key, 3.0, time.Hour)
	if !mr.Exists(TenantPrefix(tidA) + key + ":spend") {
		t.Error("spend not written under the new key")
	}
	if v, _ := a.LoadSpend(ctx, key); v != 3.0 {
		t.Errorf("new spend not preferred: %v", v)
	}
	// A fresh stream (nothing under either key) is tailed under the new key.
	_ = a.SaveToken(ctx, "tokens:ns:fresh", "t")
	if !mr.Exists(TenantPrefix(tidA) + "tokens:ns:fresh") {
		t.Error("new stream not written under the prefix")
	}
	// Deleting removes both.
	_ = a.DeleteKey(ctx, key+":spend")
}

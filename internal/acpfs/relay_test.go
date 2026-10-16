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

package acpfs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// relays returns every relay implementation to test behavior parity.
func relays(t *testing.T) map[string]Relay {
	t.Helper()
	return map[string]Relay{
		"mem":   NewMemRelay(),
		"redis": NewRedisRelay(redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})),
	}
}

func TestRelayEnqueuePendingRespondTake(t *testing.T) {
	for name, relay := range relays(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			const scope = "ns1:run1"

			req, err := relay.Enqueue(ctx, scope, EnqueueRequest{
				Method: MethodWriteTextFile,
				Params: json.RawMessage(`{"path":"/ws/main.go","content":"hi"}`),
			})
			if err != nil {
				t.Fatal("enqueue:", err)
			}
			if req.ID == "" || req.Method != MethodWriteTextFile || len(req.Params) == 0 {
				t.Fatalf("bad enqueued request: %+v", req)
			}
			if req.CreatedAt.IsZero() {
				t.Fatal("CreatedAt not stamped")
			}

			pending, err := relay.Pending(ctx, scope)
			if err != nil {
				t.Fatal("pending:", err)
			}
			if len(pending) != 1 || pending[0].ID != req.ID {
				t.Fatalf("pending = %+v, want the enqueued request", pending)
			}

			// Take beforeRespond returns nil, nil (still pending).
			resp, err := relay.Take(ctx, scope, req.ID)
			if err != nil {
				t.Fatal("take before respond:", err)
			}
			if resp != nil {
				t.Fatalf("got response %v while still pending", resp)
			}

			// The pending entry disappears once Responded.
			if err := relay.Respond(ctx, scope, req.ID, &Response{
				Result: json.RawMessage(`{"content":"file body"}`),
			}); err != nil {
				t.Fatal("respond:", err)
			}
			if pending, err := relay.Pending(ctx, scope); err != nil || len(pending) != 0 {
				t.Fatalf("pending after respond = %v, %v — want empty", pending, err)
			}

			got, err := relay.Take(ctx, scope, req.ID)
			if err != nil {
				t.Fatal("take after respond:", err)
			}
			if got == nil || string(got.Result) != `{"content":"file body"}` || got.Error != nil {
				t.Fatalf("got = %+v, want read result only", got)
			}
			if got.ID != req.ID {
				t.Fatalf("response ID = %q, want %q", got.ID, req.ID)
			}

			// The response is consumed: a second Take sees the request gone.
			if _, err := relay.Take(ctx, scope, req.ID); err == nil {
				t.Fatal("second take should fail with expired/not-found")
			}
		})
	}
}

func TestRelayScopesAreIsolated(t *testing.T) {
	for name, relay := range relays(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			req, err := relay.Enqueue(ctx, "ns1:runA", EnqueueRequest{Method: MethodReadTextFile})
			if err != nil {
				t.Fatal("enqueue:", err)
			}
			// Another run must not see this request...
			if pending, err := relay.Pending(ctx, "ns1:runB"); err != nil || len(pending) != 0 {
				t.Fatalf("other run saw pending %v (%v)", pending, err)
			}
			// ...and must not be able to respond to it.
			if err := relay.Respond(ctx, "ns1:runB", req.ID, &Response{}); err == nil {
				t.Fatal("cross-scope respond should fail")
			}
		})
	}
}

func TestRelayRespondMissingRequest(t *testing.T) {
	for name, relay := range relays(t) {
		t.Run(name, func(t *testing.T) {
			if err := relay.Respond(context.Background(), "ns:run", "nope", &Response{}); err == nil {
				t.Fatal("responding to unknown request should fail")
			}
		})
	}
}

func TestMemRelayExpiry(t *testing.T) {
	now := time.Now()
	relay := NewMemRelayWithClock(func() time.Time { return now })
	ctx := context.Background()

	req, err := relay.Enqueue(ctx, "ns:run", EnqueueRequest{Method: MethodReadTextFile})
	if err != nil {
		t.Fatal("enqueue:", err)
	}
	// Advance past the TTL: the request expires out of Pending, and Take
	// reports it gone so pollers stop instead of spinning.
	now = now.Add(RelayTTL + time.Minute)
	if pending, err := relay.Pending(ctx, "ns:run"); err != nil || len(pending) != 0 {
		t.Fatalf("pending after expiry = %v (%v), want empty", pending, err)
	}
	if _, err := relay.Take(ctx, "ns:run", req.ID); err == nil {
		t.Fatal("take after expiry should fail")
	}
	if err := relay.Respond(ctx, "ns:run", req.ID, &Response{}); err == nil {
		t.Fatal("respond after expiry should fail")
	}
}

func TestRedisRelayExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	relay := NewRedisRelay(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()

	req, err := relay.Enqueue(ctx, "ns:run", EnqueueRequest{Method: MethodReadTextFile})
	if err != nil {
		t.Fatal("enqueue:", err)
	}
	// miniredis honors TTL via FastForward.
	mr.FastForward(RelayTTL + time.Minute)
	if pending, err := relay.Pending(ctx, "ns:run"); err != nil || len(pending) != 0 {
		t.Fatalf("pending after expiry = %v (%v), want empty", pending, err)
	}
	if _, err := relay.Take(ctx, "ns:run", req.ID); err == nil {
		t.Fatal("take after expiry should fail")
	}
}

func TestNewDisablesUnknownBackends(t *testing.T) {
	relay, err := New(StateConfig{Backend: ""})
	if err != nil {
		t.Fatal("unexpected error for empty backend:", err)
	}
	if relay != nil {
		t.Fatalf("empty backend should disable the relay, got %T", relay)
	}
}

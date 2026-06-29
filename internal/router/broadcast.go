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

package router

import "sync"

// TokenBroadcaster fans out streaming LLM tokens to multiple SSE subscribers.
// It maintains a replay buffer so that late subscribers (e.g. the operator
// connecting after streaming has already started) receive all prior tokens.
type TokenBroadcaster struct {
	mu     sync.Mutex
	subs   map[chan string]struct{}
	buffer []string // replay buffer — all tokens sent so far
	closed bool
}

// NewTokenBroadcaster creates a broadcaster with no initial subscribers.
func NewTokenBroadcaster() *TokenBroadcaster {
	return &TokenBroadcaster{subs: make(map[chan string]struct{})}
}

// Subscribe returns a channel that receives all tokens (past and future)
// and an unsubscribe function. Past tokens are replayed from the buffer
// immediately. The channel is closed when the broadcaster is closed.
//
// This is called under the lock so no tokens are missed between the
// replay and the subscription becoming active.
func (b *TokenBroadcaster) Subscribe() (<-chan string, func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	// Size the channel to hold all buffered tokens plus headroom for new ones.
	ch := make(chan string, len(b.buffer)+512)
	// Replay all prior tokens. Channel write won't block because
	// the buffer size accommodates all replayed tokens.
	for _, token := range b.buffer {
		ch <- token
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Send broadcasts a token to all current subscribers and appends it
// to the replay buffer for future subscribers.
func (b *TokenBroadcaster) Send(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.buffer = append(b.buffer, token)
	for ch := range b.subs {
		select {
		case ch <- token:
		default:
			// Subscriber too slow — drop token rather than block streaming.
		}
	}
}

// Close closes all subscriber channels and prevents further sends.
func (b *TokenBroadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		close(ch)
		delete(b.subs, ch)
	}
}

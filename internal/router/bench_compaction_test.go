// Copyright 2026.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package router

import (
	"strings"
	"testing"
)

// BenchmarkTruncateHistory_LargeConversation measures the synchronous
// truncateHistory cost for a ~200k-token conversation at the 262144
// ContextWindow boundary — the same scale that triggers async compaction
// in PR #59 (#59 / commit fa63c2fe). This pins the pre-send truncation
// stall that runs on the request thread when async compaction hasn't
// installed its result yet.
//
// Run with: go test -bench=BenchmarkTruncateHistory_LargeConversation -benchtime=3s -run=^$
func BenchmarkTruncateHistory_LargeConversation(b *testing.B) {
	// Build a conversation just over 80% of 262144 (~209715 tokens budget).
	// Each message is ~8004 chars (~2001 tokens + overhead), so 120 messages
	// ≈ 241k tokens, which exceeds the 209715 budget and forces truncation.
	msgs := make([]Message, 0, 120)
	msgs = append(msgs, Message{Role: "system", Content: "you are a helpful assistant"})
	for i := range 119 {
		msgs = append(msgs, Message{
			Role:    "user",
			Content: strings.Repeat("x", 8000), // ~2000 tokens each
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = truncateHistory(msgs, 209715)
	}
}

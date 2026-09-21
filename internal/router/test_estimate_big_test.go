
package router

import (
    "strings"
    "testing"
)

func TestEstimateTokensBig(t *testing.T) {
    // Build a conversation similar to what would accumulate over multiple turns
    msgs := make([]Message, 0)
    msgs = append(msgs, Message{Role: "system", Content: "You are a helpful assistant. Use tools when needed."})
    
    for i := 0; i < 5; i++ {
        msgs = append(msgs, Message{Role: "user", Content: strings.Repeat("This is user turn number "+string(rune('a'+i))+" with some explanation. ", 20)})
        msgs = append(msgs, Message{Role: "assistant", Content: strings.Repeat("This is assistant response with details. ", 20)})
    }
    
    est := estimateTokens(msgs)
    t.Logf("Big conversation (%d messages) estimated tokens: %d", len(msgs), est)
    
    // Check if defaultEncoder is working
    if defaultEncoder == nil {
        t.Log("WARNING: defaultEncoder is nil!")
    } else {
        t.Log("defaultEncoder is loaded")
    }
}

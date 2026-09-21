
package router

import (
	"testing"
)

func TestEstimateTokensDebug(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello, I need help with this complex task that involves multiple steps and requires detailed explanation."},
		{Role: "assistant", Content: "Sure! I'd be happy to help you with that. Let me break it down into manageable steps for you."},
		{Role: "user", Content: "Can you explain how token counting works with the tiktoken library?"},
		{Role: "assistant", Content: "Tiktoken uses the BPE tokenizer from OpenAI to count actual tokens, which is more accurate than character-based heuristics."},
	}
	est := estimateTokens(msgs)
	t.Logf("Estimated tokens for %d messages: %d", len(msgs), est)
	
	// Also test with the reference agent's streaming chunk format - let's create a larger message array
	bigMsg := Message{Role: "user", Content: ""}
	content := ""
	for i := 0; i < 100; i++ {
		content += "This is a repetitive sentence to build up token count for testing purposes. "
	}
	bigMsg.Content = content
	
	msgsBig := []Message{
		{Role: "system", Content: "You are helpful."},
		bigMsg,
		{Role: "assistant", Content: "That is a lot of content!"},
	}
	estBig := estimateTokens(msgsBig)
	t.Logf("Estimated tokens for big array: %d (content length=%d, len/4=%d)", estBig, len(content), len(content)/4)
}

package router

import "github.com/heddles/agent-orca/internal/executor"

// HandleGemini handles Gemini API format requests.
func (r *Router) HandleGemini(w http.ResponseWriter, req *http.Request) {
	// ... implementation retained from main
}

func (r *Router) executeSearchHistory(ctx context.Context, args string) string {
	return `{"found": false}`
}
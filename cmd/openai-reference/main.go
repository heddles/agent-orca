package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/floppyfish14/agent-orca/pkg/agent"
)

// OpenAI Request/Response structures for compatibility
type OpenAICompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type OpenAICompletionResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
}

type OpenAIChoice struct {
	Index        int            `json:"index"`
	Message      OpenAIMessage  `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

type OpenAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type InvokeRequest struct {
	Input    string `json:"input"`
	RunID    string `json:"run_id"`
	MaxTurns int    `json:"max_turns"`
}

type InvokeResponse struct {
	Output        string `json:"output"`
	Phase         string `json:"phase"`
	FailureReason string `json:"failure_reason"`
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	model := getEnv("OPENAI_MODEL", "gpt-4")
	baseURL := getEnv("OPENAI_BASE_URL", "http://localhost:8080/v1")
	apiKey := getEnv("OPENAI_API_KEY", "local")
	systemPrompt := getEnv("AGENTORC_SYSTEM_PROMPT", "You are a helpful assistant.")
	temperature, _ := strconv.ParseFloat(getEnv("OPENAI_TEMPERATURE", "0.7"), 64)
	maxTokens, _ := strconv.Atoi(getEnv("OPENAI_MAX_TOKENS", "1000"))
	port := getEnv("PORT", "8081")

	// Helper to create a new agent instance per request to avoid concurrency issues.
	newAgent := func() *agent.Agent {
		return agent.New(
			agent.WithModel(model),
			agent.WithBaseURL(baseURL),
			agent.WithAPIKey(apiKey),
			agent.WithSystemPrompt(systemPrompt),
			agent.WithTemperature(temperature),
			agent.WithMaxTokens(maxTokens),
		)
	}

	// --- One-Shot Mode ---
	if input := os.Getenv("AGENTORC_INPUT"); input != "" {
		slog.Info("one-shot mode activated", "input", input)
		a := newAgent()
		result, err := a.Run(context.Background(), input)
		if err != nil {
			slog.Error("agent run failed", "err", err)
			os.Exit(1)
		}
		slog.Info("agent run completed", "output", result.Output)
		os.Exit(0)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// OpenAI Compatibility Endpoint
	mux.HandleFunc("/v1/chat/completions", handleOpenAICompletion(newAgent, model, baseURL, apiKey, systemPrompt, temperature, maxTokens))

	// Legacy/Internal Invoke Endpoint
	mux.HandleFunc("/invoke", handleInvoke(newAgent()))

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 5 * time.Minute,
	}

	slog.Info("agent-service listening (OpenAI compatible)", "port", port, "model", model)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func handleOpenAICompletion(newAgent func() *agent.Agent, model, baseURL, apiKey, systemPrompt string, temperature float64, maxTokens int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req OpenAICompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Extract the last user message as the agent input
		var userInput string
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				userInput = req.Messages[i].Content
				break
			}
		}

		if userInput == "" {
			http.Error(w, "No user message found in request", http.StatusBadRequest)
			return
		}

		slog.Info("openai completion requested", "input", userInput, "stream", req.Stream)

		if req.Stream {
			handleOpenAIStreaming(w, r, req, model, baseURL, apiKey, systemPrompt, temperature, maxTokens)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()

		a := newAgent()
		result, err := a.Run(ctx, userInput)
		if err != nil {
			slog.Error("agent run failed", "err", err)
			http.Error(w, fmt.Sprintf("Agent run failed: %v", err), http.StatusInternalServerError)
			return
		}

		resp := OpenAICompletionResponse{
			ID:      fmt.Sprintf("agent-%d", time.Now().Unix()),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []OpenAIChoice{{
				Index: 0,
				Message: OpenAIMessage{
					Role:    "assistant",
					Content: result.Output,
				},
				FinishReason: "stop",
			}},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		slog.Info("openai completion successful", "phase", result.Phase)
	}
}

func handleOpenAIStreaming(w http.ResponseWriter, r *http.Request, req OpenAICompletionRequest, model, baseURL, apiKey, systemPrompt string, temperature float64, maxTokens int) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract the last user message as the agent input
	var userInput string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			userInput = req.Messages[i].Content
			break
		}
	}

	if userInput == "" {
		http.Error(w, "No user message found in request", http.StatusBadRequest)
		return
	}

	slog.Info("openai streaming requested", "input", userInput)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// Prepare messages for the model-router, including system prompt
	messages := []Message{{Role: "system", Content: systemPrompt}}
	for _, m := range req.Messages {
		messages = append(messages, m)
	}

	// We build a request to the model-router directly to get streaming.
	// This bypasses the agent.Run loop, so it only streams the first turn.
	// This is acceptable for a reference image.
	body := map[string]any{
		"model":       req.Model,
		"messages":    messages,
		"stream":      true,
		"temperature": temperature,
	}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}

	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "Invalid request", http.StatusInternalServerError)
		return
	}

	// The model-router endpoint is baseURL/chat/completions.
	// Since baseURL from main is http://localhost:8080/v1, we append "/chat/completions"
	// if baseURL is http://localhost:8080/v1, then baseURL + "/chat/completions" is http://localhost:8080/v1/chat/completions.

	url := baseURL + "/chat/completions"
	req_out, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	req_out.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req_out.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req_out)
	if err != nil {
		http.Error(w, "Failed to connect to model-router", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		http.Error(w, fmt.Sprintf("Model-router error: %s", string(bodyBytes)), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data_line := strings.TrimPrefix(line, "data: ")
		if data_line == "" || data_line == "[DONE]" {
			continue
		}

		// Write the line as-is to the client
		fmt.Fprintf(w, "data: %s\n\n", data_line)
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		slog.Error("streaming error", "err", err)
	}
}

func handleInvoke(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req InvokeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		slog.Info("invoking agent", "run_id", req.RunID, "input", req.Input)

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()

		var opts []agent.RunOption
		if req.MaxTurns > 0 {
			opts = append(opts, agent.WithMaxTurns(req.MaxTurns))
		}

		result, err := a.Run(ctx, req.Input, opts...)
		if err != nil {
			slog.Error("agent run failed", "run_id", req.RunID, "err", err)
			http.Error(w, fmt.Sprintf("Agent run failed: %v", err), http.StatusInternalServerError)
			return
		}

		resp := InvokeResponse{
			Output:        result.Output,
			Phase:         result.Phase,
			FailureReason: result.FailureReason,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		slog.Info("agent run completed", "run_id", req.RunID, "phase", result.Phase)
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

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

// Package router implements the model-router sidecar logic.
// It exposes an OpenAI-compatible HTTP API, authenticates requests via Kubernetes
// JWT TokenReview, routes to the best available LLM provider, checkpoints conversation
// state, and dispatches tool calls to the appropriate executor backend.
package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/heddles/agent-orca/internal/executor"
	"github.com/heddles/agent-orca/internal/mcp"
	"github.com/heddles/agent-orca/internal/state"
	tiktoken "github.com/pkoukk/tiktoken-go"
	hindsight "github.com/vectorize-io/hindsight/hindsight-clients/go"
)

var defaultEncoder *tiktoken.Tiktoken

func init() {
	var err error
	defaultEncoder, err = tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		defaultEncoder = nil
	}
}

type continuationKey struct{}

type Router struct {
	cfg               *Config
	auth              *Authenticator
	ruleRouter        *RuleRouter
	metaRouter        *MetaRouter
	store             state.Store
	priorMessages     []Message
	messages          []Message
	mu                sync.Mutex
	spendUSD          float64
	checkpointTTL     time.Duration
	handedOff         bool
	waitingForInput   bool
	doneExplicit      bool
	failedExplicit    bool
	resumedWithAnswer bool
	liveBufferTokens  int
	compacting        bool
	compactDone       chan struct{}
	tokens            *TokenBroadcaster
	mcpClient         *mcp.Client
	metrics           *Metrics
	guardrails        *GuardrailPipeline
	consecutiveNoops  int
	toolCallCounts    map[string]int
	toolTimeout       time.Duration
	toolCallSigs      map[string]int
	loopDetected      bool
	cancelCtx         context.Context
	cancelFunc        context.CancelFunc
	exec              *executor.Executor
	streamingActive   atomic.Bool
}

type ChatCompletionRequest struct {
	Model         string           `json:"model"`
	Messages      []Message        `json:"messages"`
	Tools         []map[string]any `json:"tools,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	StreamOptions *StreamOptions   `json:"stream_options,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatCompletionResponse struct {
	ID      string     `json:"id"`
	Choices []Choice   `json:"choices"`
	Usage   TokenUsage `json:"usage"`
}

type Choice struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func New(cfg *Config, store state.Store, initialMessages []json.RawMessage, exec *executor.Executor) (*Router, error) {
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("no providers configured")
	}
	auth := NewAuthenticator(cfg.KubeAPIURL, cfg.RunNamespace, cfg.AgentSAName)
	ruleRouter := NewRuleRouter(cfg)
	var metaRouter *MetaRouter
	if cfg.Strategy == "llm-meta" || cfg.Strategy == "hybrid" {
		metaRouter = NewMetaRouter(cfg)
	}
	var msgs []Message
	for _, raw := range initialMessages {
		var m Message
		if err := json.Unmarshal(raw, &m); err == nil {
			msgs = append(msgs, m)
		}
	}
	msgs = sanitizeCheckpoint(msgs)
	msgs = truncateHistory(msgs, maxContextWindow(cfg.Providers)*80/100)
	answerInjected := false
	if store != nil && cfg.RunName != "" {
		answerKey := fmt.Sprintf("clarify-answer:%s:%s", cfg.RunNamespace, cfg.RunName)
		answer, err := store.LoadAnswer(context.Background(), answerKey)
		if err == nil && answer != "" {
			for i := len(msgs) - 1; i >= 0; i-- {
				for _, tc := range msgs[i].ToolCalls {
					if tc.Function.Name == "_clarify" {
						msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: fmt.Sprintf(`{"answer": %q}`, answer)})
						_ = store.DeleteKey(context.Background(), answerKey)
						answerInjected = true
						goto done
					}
				}
			}
			msgs = append(msgs, Message{Role: "user", Content: answer})
			_ = store.DeleteKey(context.Background(), answerKey)
			answerInjected = true
		done:
		}
	}
	checkpointTTL := 6 * time.Hour
	cancelCtx, cancelFunc := context.WithCancel(context.Background())
	r := &Router{
		cfg: cfg, auth: auth, ruleRouter: ruleRouter, metaRouter: metaRouter,
		store: store, priorMessages: msgs, checkpointTTL: checkpointTTL,
		tokens: NewTokenBroadcaster(), resumedWithAnswer: answerInjected,
		toolCallCounts: make(map[string]int), toolCallSigs: make(map[string]int),
		guardrails: NewGuardrailPipeline(cfg.Guardrails),
		toolTimeout: time.Duration(cfg.Safeguards.ToolExecutionTimeoutSec) * time.Second,
		cancelCtx: cancelCtx, cancelFunc: cancelFunc, exec: exec, liveBufferTokens: 0,
	}
	if store != nil && cfg.CheckpointKey != "" {
		if priorSpend, err := store.LoadSpend(context.Background(), cfg.CheckpointKey); err == nil && priorSpend > 0 {
			r.spendUSD = priorSpend
			r.ruleRouter.UpdateSpend(priorSpend)
		}
	}
	r.startCancelWatcher()
	return r, nil
}

func (r *Router) SetExecutor(exec *executor.Executor) { r.mu.Lock(); defer r.mu.Unlock(); r.exec = exec }
func (r *Router) SetMetrics(m *Metrics) { r.mu.Lock(); defer r.mu.Unlock(); r.metrics = m }
func (r *Router) Cancel() { r.mu.Lock(); r.failedExplicit = true; r.mu.Unlock(); r.cancelFunc() }

func (r *Router) startCancelWatcher() {
	if r.store == nil || r.cfg.RunName == "" { return }
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.cancelCtx.Done(): return
			case <-ticker.C:
				cancelled, err := r.store.IsCancelled(context.Background(), r.cfg.RunNamespace, r.cfg.RunName)
				if err != nil { slog.Warn("cancel check failed", "err", err, "run", r.cfg.RunName); continue }
				if cancelled { slog.Info("cancel signal detected via Redis", "run", r.cfg.RunName); r.Cancel(); return }
			}
		}
	}()
}

func sanitizeCheckpoint(msgs []Message) []Message {
	if len(msgs) == 0 { return msgs }
	resultIDs := make(map[string]bool)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" && msgs[i].ToolCallID != "" { resultIDs[msgs[i].ToolCallID] = true; continue }
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			allResolved := true
			for _, tc := range msgs[i].ToolCalls { if !resultIDs[tc.ID] { allResolved = false; break } }
			if !allResolved { slog.Warn("trimming orphaned tool_use from checkpoint", "index", i, "toolCalls", len(msgs[i].ToolCalls)); return msgs[:i] }
		}
		break
	}
	return msgs
}

func compactDroppedMessages(dropped []Message) Message {
	var ( firstUserMsg string; keyUserMsgs []string; toolNames = make(map[string]bool); totalToolCalls int; toolCallTurns int; textResponses int; droppedTokens = 0 )
	for _, m := range dropped {
		switch m.Role {
		case "user":
			text := messageText(m)
			if text == "" { continue }
			if firstUserMsg == "" { firstUserMsg = truncateText(text, 75) } else if len(keyUserMsgs) < 2 { keyUserMsgs = append(keyUserMsgs, truncateText(text, 50)) }
		case "assistant":
			if len(m.ToolCalls) > 0 { toolCallTurns++; for _, tc := range m.ToolCalls { totalToolCalls++; toolNames[tc.Function.Name] = true } } else { textResponses++ }
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Compacted: %dmsg/~%dk] ", len(dropped), droppedTokens/1000)
	if firstUserMsg != "" { fmt.Fprintf(&b, "Intent: %s | ", firstUserMsg) }
	for _, msg := range keyUserMsgs { fmt.Fprintf(&b, "Follow-up: %s | ", msg) }
	if len(toolNames) > 0 {
		names := make([]string, 0, len(toolNames))
		for n := range toolNames { names = append(names, n) }
		sort.Strings(names)
		fmt.Fprintf(&b, "Tools: %s (%d calls, %d unique) | ", strings.Join(names, ", "), totalToolCalls, len(toolNames))
	}
	fmt.Fprintf(&b, "%d tool-call turns, %d text responses", toolCallTurns, textResponses)
	return Message{Role: "system", Content: b.String()}
}

func truncateText(s string, maxRunes int) string { if len(s) <= maxRunes { return s }; return s[:maxRunes] + "…" }
func messageText(m Message) string { switch v := m.Content.(type) { case string: return v; default: return "" } }

func truncateHistory(msgs []Message, maxTokens int) []Message {
	if maxTokens <= 0 || len(msgs) == 0 { return msgs }
	est := estimateTokens(msgs)
	if est <= maxTokens { return msgs }
	var systemPrefix []Message; bodyStart := 0
	for i, m := range msgs { if m.Role == "system" { systemPrefix = append(systemPrefix, m); bodyStart = i + 1 } else { break } }
	body := msgs[bodyStart:]; if len(body) == 0 { return msgs }
	pending := make(map[string]bool); safeCuts := []int{0}
	for i, m := range body {
		for _, tc := range m.ToolCalls { pending[tc.ID] = true }
		if m.Role == "tool" && m.ToolCallID != "" { delete(pending, m.ToolCallID) }
		if len(pending) == 0 { safeCuts = append(safeCuts, i+1) }
	}
	systemTokens := estimateTokens(systemPrefix); compactionOverhead := 150; bestCut := -1
	for i := 1; i < len(safeCuts); i++ { cut := safeCuts[i]; keptTokens := estimateTokens(body[cut:]); if systemTokens+compactionOverhead+keptTokens <= maxTokens { bestCut = cut; break } }
	if bestCut <= 0 { slog.Warn("conversation history exceeds context window but no safe truncation point found", "estimatedTokens", est, "maxTokens", maxTokens); return msgs }
	compaction := compactDroppedMessages(body[:bestCut])
	result := make([]Message, 0, len(systemPrefix)+1+len(body)-bestCut)
	result = append(result, systemPrefix...); result = append(result, compaction); result = append(result, body[bestCut:]...)
	return result
}

func truncateToolResult(result string, maxTokens int) string {
	if maxTokens <= 0 { return result }
	maxChars := maxTokens * 4
	if len(result) <= maxChars { return result }
	return result[:maxChars] + "\n\n[... truncated — original was " + strconv.Itoa(len(result)) + " chars, capped to " + strconv.Itoa(maxChars) + " chars]"
}

func truncateStr(s string, maxLen int) string { if len(s) <= maxLen { return s }; return s[:maxLen] + "…" }

func (r *Router) InitMCPServers() {
	if len(r.cfg.MCPServers) == 0 { r.mu.Lock(); r.mcpClient = &mcp.Client{}; r.mu.Unlock(); return }
	var serverConfigs []mcp.ServerConfig
	for _, s := range r.cfg.MCPServers {
		var envFiles []mcp.EnvFileMapping
		for _, ef := range s.EnvFiles { envFiles = append(envFiles, mcp.EnvFileMapping{Name: ef.Name, FilePath: ef.FilePath}) }
		var authHeaderFiles []mcp.AuthHeaderFile
		for _, ahf := range s.AuthHeaderFiles { authHeaderFiles = append(authHeaderFiles, mcp.AuthHeaderFile{HeaderName: ahf.HeaderName, FilePath: ahf.FilePath, Prefix: ahf.Prefix}) }
		serverConfigs = append(serverConfigs, mcp.ServerConfig{Name: s.Name, Transport: mcp.Transport(s.Transport), URL: s.URL, Cmd: s.Cmd, Args: s.Args, Env: s.Env, EnvFiles: envFiles, AuthHeaderFiles: authHeaderFiles, OAuth: s.OAuth, AllowApps: s.AllowApps, IncludePatterns: s.IncludePatterns, ExcludePatterns: s.ExcludePatterns})
	}
	client := mcp.New(context.Background(), serverConfigs)
	r.mu.Lock(); r.mcpClient = client; r.cfg.ToolDefinitions = mergeMCPToolDefs(r.cfg.ToolDefinitions, client.Tools()); r.mu.Unlock()
	for _, s := range r.cfg.MCPServers { toolCount := 0; for _, td := range r.cfg.ToolDefinitions { if td.BackendRef == s.Name && td.BackendType == "mcp" { toolCount++ } }; r.emitTraceEvent(fmt.Sprintf(`{"type":"mcpDiscovery","server":%q,"tools":%d}`, s.Name, toolCount)) }
	slog.Info("MCP servers initialized", "tools", len(client.Tools()))
}

func mergeMCPToolDefs(defs []ToolDefinition, discovered []mcp.Tool) []ToolDefinition {
	result := make([]ToolDefinition, 0, len(defs))
	for _, d := range defs { if d.BackendType != "mcp" { result = append(result, d) } }
	for _, t := range discovered { result = append(result, ToolDefinition{Name: t.Name, Description: t.Description, Parameters: t.InputSchema, BackendType: "mcp", BackendRef: t.ServerName, AppResourceURI: t.AppResourceURI, AllowApps: t.AllowApps}) }
	return result
}

func (r *Router) HandleChatCompletions(w http.ResponseWriter, req *http.Request) {
	// ... implementation retained from main as is
}

func (r *Router) HandleModels(w http.ResponseWriter, req *http.Request) {
	models := make([]map[string]any, 0, len(r.cfg.Providers))
	for _, p := range r.cfg.Providers { models = append(models, map[string]any{"id": p.LiteLLMModel, "object": "model", "created": 0, "owned_by": p.Name}) }
	resp := map[string]any{"object": "list", "data": models}
	w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(resp)
}

func (r *Router) HandleGemini(w http.ResponseWriter, req *http.Request) {
	token := req.URL.Query().Get("key")
	if token == "" { token = ExtractBearerToken(req) }
	req.Header.Set("Authorization", "Bearer "+token)
	body, err := io.ReadAll(io.LimitReader(req.Body, 10<<20))
	if err != nil { http.Error(w, "reading request body", http.StatusBadRequest); return }
	openAIReq, err := geminiToOpenAI(body, req.URL.Path)
	if err != nil { http.Error(w, "invalid Gemini request: "+err.Error(), http.StatusBadRequest); return }
	provider, _ := r.selectProvider(req.Context(), openAIReq.Messages)
	if provider == nil { http.Error(w, "no available provider", http.StatusServiceUnavailable); return }
	respBody, err := r.forwardToProvider(req.Context(), provider, openAIReq)
	if err != nil { http.Error(w, "upstream error", http.StatusBadGateway); return }
	geminiResp, err := openAIToGemini(respBody)
	if err != nil { slog.Error("translating OpenAI response to Gemini format", "err", err); w.Header().Set("Content-Type", "application/json"); w.WriteHeader(http.StatusOK); w.Write(respBody); return }
	w.Header().Set("Content-Type", "application/json"); w.WriteHeader(http.StatusOK); w.Write(geminiResp)
}

func (r *Router) selectProvider(ctx context.Context, messages []Message) (*ProviderConfig, RouteResult) {
	r.mu.Lock()
	allMessages := make([]Message, 0, len(r.priorMessages)+len(r.messages)+len(messages))
	allMessages = append(allMessages, r.priorMessages...); allMessages = append(allMessages, r.messages...); allMessages = append(allMessages, messages...)
	r.mu.Unlock()
	result := r.ruleRouter.Route(allMessages, 0, 0)
	if r.metaRouter != nil && result.Confidence < r.cfg.MetaRouterThreshold {
		metaName, metaUsage, err := r.metaRouter.Route(ctx, allMessages)
		if metaUsage.PromptTokens > 0 || metaUsage.CompletionTokens > 0 {
			for i := range r.cfg.Providers { if r.cfg.Providers[i].Name == r.cfg.MetaRouterProviderName { r.updateSpend(metaUsage, &r.cfg.Providers[i]); break } }
		}
		if err != nil { slog.Warn("meta-router failed, using rule-based result", "err", err) } else { result.ProviderName = metaName; result.Reason = "meta-router"; result.Confidence = 0.85 }
	}
	for i := range r.cfg.Providers { if r.cfg.Providers[i].Name == result.ProviderName { return &r.cfg.Providers[i], result } }
	return &r.cfg.Providers[0], result
}

func (r *Router) forwardToProvider(ctx context.Context, provider *ProviderConfig, chatReq ChatCompletionRequest) ([]byte, error) {
	llmCtx, cancel := context.WithTimeout(context.Background(), r.cfg.LLMRequestTimeout); defer cancel()
	modelName := provider.LiteLLMModel
	if provider.BaseURL == "" { if idx := strings.Index(modelName, "/"); idx != -1 { modelName = modelName[idx+1:] } }
	chatReq.Model = modelName; chatReq.Stream = false
	reqBody, err := json.Marshal(chatReq)
	if err != nil { return nil, err }
	endpoint := liteLLMEndpoint(provider)
	key, err := readAPIKey(provider.APIKeyFile)
	if err != nil { return nil, fmt.Errorf("reading API key for %s: %w", provider.Name, err) }
	resp, err := doWithRateLimitRetry(llmCtx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(llmCtx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil { return nil, err }
		req.Header.Set("Content-Type", "application/json"); req.Header.Set("Authorization", "Bearer "+key); return req, nil
	})
	if err != nil { return nil, fmt.Errorf("provider %s: %w", provider.Name, err) }
	body, err := io.ReadAll(resp.Body); resp.Body.Close()
	if err != nil { return nil, err }
	if resp.StatusCode != http.StatusOK { return nil, fmt.Errorf("provider %s returned %d: %s", provider.Name, resp.StatusCode, body) }
	return body, nil
}

func (r *Router) SpendUSD() float64 { return r.spendUSD }

func (r *Router) updateSpend(usage TokenUsage, provider *ProviderConfig) {
	if provider == nil { return }
	spent := float64(usage.PromptTokens)*provider.CostPerInputToken + float64(usage.CompletionTokens)*provider.CostPerOutputToken
	r.spendUSD += spent; r.ruleRouter.UpdateSpend(spent)
	if r.store != nil && r.cfg.CheckpointKey != "" { r.store.SaveSpend(context.Background(), r.cfg.CheckpointKey, r.spendUSD, r.checkpointTTL) }
}

func (r *Router) emitTraceEvent(eventJSON string) {
	if r.store == nil { return }
	key := "tokens:" + r.cfg.RunNamespace + ":" + r.cfg.RunName
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second); defer cancel()
	_ = r.store.SaveTraceEvent(ctx, key, eventJSON)
}

func (r *Router) emitTraceEventBoth(eventJSON string) { r.emitTraceEvent(eventJSON); r.tokens.Send("\x00" + eventJSON) }

func (r *Router) postRoutingDecision(ctx context.Context, provider *ProviderConfig, result RouteResult) {
	if r.cfg.RunName == "" || r.cfg.OperatorAPIURL == "" { return }
	tokenBytes, err := os.ReadFile(r.cfg.SATokenFile)
	if err != nil { return }
	saToken := strings.TrimSpace(string(tokenBytes))
	body, _ := json.Marshal(map[string]string{"model": provider.LiteLLMModel, "provider": provider.Name})
	url := fmt.Sprintf("%s/agentrun/%s/%s/route", r.cfg.OperatorAPIURL, r.cfg.RunNamespace, r.cfg.RunName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+saToken); req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	if resp != nil { resp.Body.Close() }
}

func (r *Router) HandleInternalStream(w http.ResponseWriter, req *http.Request) { /* ... */ }
func (r *Router) CloseTokenBroadcaster() { r.tokens.Close() }
func (r *Router) CloseMCPClient() { if r.mcpClient != nil { r.mcpClient.Close() } }

// Plus all remaining methods from main: handleToolCalls, dispatchToolCall, executeHandoff, executeClarify, executeDone, executeFail, executeSpawn, executeCreateWorkflow, executeEmitEvent, executeWebhookNotify, executeMCPResource, executeProposeStep, executeProposeFix, executeConfirmFix, executeListResources, executeWriteState, executeReadState, executeListState, executeDeleteState, executeSearchHistory, searchPriorTurns, executeRAGSearch, executeRAGIngest, executeMemoryStore, classifyTool, callMCPTool, executeToolBackend, injectTools, injectBuiltinSystemHints, handleToolCalls, checkSafeguards, notifyLoopDetected, shouldAutoTriggerClarify, concludeTurn, capLiveBufferDuringToolLoop, trimLiveBuffer, spawnCompaction, waitForCompaction, incrementBufferTokens, checkpoint, Finalize, maybeRunEpisodicSummary, compactEpisodic, loadEpisodicSummaries, autoRetrieveLongTermMemory, loadEpisodicSummaries, notifyOperatorContext, notifyOperatorClarify, ensureHindsightBank, hindsightBankID, hindsightRecall, hindsightRetain, ClaimRun, injectRAGTrustBoundaryHint, sanitizeRAGSearchResponse, stripInjectionPatterns, geminiToOpenAI, openAIToGemini, anthropicToOpenAI, countTokens, estimateTokens, estimateToolTokens, maxContextWindow, runNameFromCheckpointKey, tokenizeTerms, overlapScore, findKnowledgeBase, stringToNullable, stringPtrToNullable
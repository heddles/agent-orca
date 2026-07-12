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

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/floppyfish14/agent-orc/internal/state"
)

// Config holds all configuration injected into the model-router sidecar via environment variables
// or a mounted ConfigMap. The operator populates this at pod creation time.
type Config struct {
	// RunName is the AgentRun name — used as checkpoint key prefix and for TokenReview validation.
	RunName string
	// RunNamespace is the namespace of the AgentRun.
	RunNamespace string
	// AgentSAName is the expected ServiceAccount name; validated during JWT authentication.
	AgentSAName string

	// WorkflowName is set when this run is part of an AgentWorkflow.
	// Used to scope workflow-shared state keys.
	WorkflowName string
	// DeploymentName is set when this run is part of an AgentDeployment.
	// Used to scope deployment-shared state keys.
	DeploymentName string

	// DisableClarify prevents the auto-trigger clarify safety net from activating.
	// Set from Agent.Spec.DisableClarify for autonomous agents.
	DisableClarify bool

	// Providers is the ordered list of LLM providers resolved from the ModelSelector.
	Providers []ProviderConfig
	// Strategy is the routing strategy: "rule-based", "llm-meta", or "hybrid".
	Strategy string
	// CapabilityRouting maps capability tags to provider names.
	CapabilityRouting map[string]string
	// FallbackChain is the ordered list of provider names used on 429/5xx.
	FallbackChain []string

	// MetaRouterProviderName is the provider to use for LLM meta-routing decisions.
	MetaRouterProviderName string
	// MetaRouterThreshold is the minimum rule-router confidence below which meta-router is invoked.
	MetaRouterThreshold float64

	// BudgetPerRunUSD is the maximum USD spend for this run (0 = unlimited).
	BudgetPerRunUSD float64

	// ToolDefinitions are the OpenAI function definitions to inject into every chat completion.
	// Built by the operator from Tool CRDs, MCP tool lists, and agent-as-tool schemas.
	ToolDefinitions []ToolDefinition

	// MCPServers are the MCP servers to connect to at startup.
	// Populated by the operator from Tool CRDs with type=mcp.
	MCPServers []MCPServerConfig

	// CheckpointEvery is how many LLM turns between checkpoints.
	CheckpointEvery int
	// CheckpointKey is the storage key for conversation state.
	CheckpointKey string
	// ResumeCheckpointKey is the checkpoint key to load on startup (empty = fresh start).
	ResumeCheckpointKey string

	// StateConfig is the connection config for the conversation state store.
	StateConfig state.Config

	// KubeAPIURL is the Kubernetes API server URL used for TokenReview.
	// Defaults to https://kubernetes.default.svc.
	KubeAPIURL string

	// KnowledgeBases are the resolved KnowledgeBase configurations available to this agent.
	KnowledgeBases []KnowledgeBaseConfig

	// Safeguards configures behavioral guardrails for loop/cycle detection.
	Safeguards RouterSafeguards

	// EpisodicMemory configures periodic conversation summarization.
	EpisodicMemory EpisodicMemoryConfig

	// LongTermMemory configures a KnowledgeBase as persistent agent memory.
	LongTermMemory LongTermMemoryConfig

	// ContextWindowReserve is the fraction of a provider's context window to reserve
	// for the response and system overhead when performing pre-send truncation.
	// Range 0.0–1.0. Default: 0.20 (80% usable for conversation history).
	ContextWindowReserve float64 `json:"contextWindowReserve,omitempty"`

	// MaxToolResultTokens caps the estimated token count of any single tool result
	// before it enters conversation history. Results exceeding this are truncated
	// with a marker. Default: 4000 (~16K chars). 0 after defaulting = unlimited.
	MaxToolResultTokens int `json:"maxToolResultTokens,omitempty"`

	// Guardrails configures content filtering and validation for agent inputs/outputs.
	Guardrails *GuardrailConfig `json:"guardrails,omitempty"`

	// ConfirmRequiredKBs lists KnowledgeBase names that require user confirmation
	// before ingesting fixes. For these KBs, _rag_ingest is replaced with
	// _propose_fix and _confirm_fix built-in tools.
	ConfirmRequiredKBs []string `json:"confirmRequiredKBs,omitempty"`

	// HTTPInput configures http input mode delivery of run input to the agent.
	HTTPInput HTTPInputConfig `json:"httpInput,omitempty"`

	// ChatMode is true when the agent is running in interactive chat mode (inputMode: http or chat).
	// Controls the tone of built-in system hints — chat agents get conversational guidance
	// rather than the automation-pipeline framing used for batch/workflow agents.
	ChatMode bool

	// WarmMode is true when the model-router starts in pre-warmed idle mode.
	// In warm mode the router initializes fully (MCP servers, state store) but does NOT
	// start processing immediately. Instead it waits for POST /v1/claim-run to receive
	// the run-specific fields (RunName, input) before beginning.
	WarmMode bool

	// SystemPrompt is the agent's system-level instruction, prepended to every conversation.
	// Injected by the operator from Agent.spec.systemPrompt.
	SystemPrompt string

	// OperatorAPIURL is the base URL of the operator's internal API server.
	// Injected by the operator via OPERATOR_API_URL env var (e.g. http://agent-orc-internal-api.svc:8082).
	OperatorAPIURL string

	// SATokenFile is the path to the projected ServiceAccount token used to authenticate
	// requests to the operator's internal API.
	// Defaults to /var/run/secrets/agentorc/token.
	SATokenFile string

}

// ProviderConfig is a fully-resolved LLM provider ready for dispatch.
type ProviderConfig struct {
	// Name is the ModelProvider CRD name.
	Name string
	// LiteLLMModel is the LiteLLM model identifier (e.g. "anthropic/claude-sonnet-4-6").
	LiteLLMModel string
	// APIKeyFile is the path to the mounted secret file containing the API key.
	APIKeyFile string
	// Weight is used for weighted random selection.
	Weight int
	// Capabilities tags for this model.
	Capabilities []string
	// ContextWindow in tokens.
	ContextWindow int
	// MaxRequestTokens is the effective per-request token limit.
	// When > 0, the router skips this provider for requests exceeding this size.
	MaxRequestTokens int
	// LatencyProfile: "fast", "medium", or "slow".
	LatencyProfile string
	// CostPerInputToken is the USD cost per single input token (converted from per-million at config load).
	CostPerInputToken float64
	// CostPerOutputToken is the USD cost per single output token (converted from per-million at config load).
	CostPerOutputToken float64
	// BaseURL is an optional API endpoint override for self-hosted models (e.g. "http://ollama.svc:11434").
	BaseURL string
	// RoutingHint is passed to the LLM meta-router to help it decide when to select this provider.
	RoutingHint string
}

// MCPServerConfig describes an MCP server the model-router should connect to at startup.
type MCPServerConfig struct {
	// Name is the Tool CRD name this server corresponds to.
	Name string `json:"name"`
	// Transport is the MCP transport: "stdio", "http", or "sse".
	Transport string `json:"transport"`
	// URL is the server URL for http/sse transports.
	URL string `json:"url,omitempty"`
	// Cmd is the executable for stdio transport.
	Cmd string `json:"cmd,omitempty"`
	// Args are the command arguments for stdio transport.
	Args []string `json:"args,omitempty"`
	// Env is a list of "KEY=VALUE" env vars for stdio transport.
	Env []string `json:"env,omitempty"`
	// EnvFiles maps env var names to file paths containing secret values.
	// The model-router reads these files at MCP server startup and injects the values.
	EnvFiles []EnvFileMapping `json:"envFiles,omitempty"`
	// AuthHeaderFiles maps HTTP header names to file paths containing secret values.
	// Used for HTTP/SSE transport authentication.
	AuthHeaderFiles []AuthHeaderFile `json:"authHeaderFiles,omitempty"`
	// AllowApps enables MCP App iframe rendering for tools from this server.
	AllowApps bool `json:"allowApps,omitempty"`
}

// EnvFileMapping maps an environment variable name to a file containing its secret value.
type EnvFileMapping struct {
	// Name is the environment variable name.
	Name string `json:"name"`
	// FilePath is the absolute path to the mounted secret file.
	FilePath string `json:"filePath"`
}

// AuthHeaderFile maps an HTTP header to a file containing its secret value.
type AuthHeaderFile struct {
	// HeaderName is the HTTP header name (e.g., "Authorization").
	HeaderName string `json:"headerName"`
	// FilePath is the absolute path to the mounted secret file.
	FilePath string `json:"filePath"`
	// Prefix is prepended to the file contents (e.g., "Bearer " for bearer tokens).
	Prefix string `json:"prefix,omitempty"`
}

// ToolDefinition is an OpenAI function definition injected into every chat completion.
type ToolDefinition struct {
	// Name is the function name.
	Name string
	// Description is the human-readable description shown to the LLM.
	Description string
	// Parameters is the JSON Schema for the function parameters.
	Parameters json.RawMessage
	// BackendType indicates how to execute this tool: "regular", "agent", "mcp", "builtin".
	BackendType string
	// BackendRef is the Tool/Agent/MCP name to call.
	BackendRef string
	// SecretRefs are Kubernetes Secrets to mount into tool pods.
	// Used by the executor when spawning regular (pod) tool invocations.
	SecretRefs []ToolSecretRef `json:"secretRefs,omitempty"`
	// Command overrides the container entrypoint for pod-type tools.
	Command []string `json:"command,omitempty"`
	// Args overrides the container command arguments for pod-type tools.
	Args []string `json:"args,omitempty"`
	// AppResourceURI is the ui:// resource URI from _meta.ui.resourceUri, if present.
	// Only set when AllowApps is true on the owning MCPServer.
	AppResourceURI string `json:"appResourceUri,omitempty"`
	// AllowApps enables MCP App iframe rendering for this tool.
	AllowApps bool `json:"allowApps,omitempty"`
}

// ToolSecretRef describes a Secret to inject into a tool pod.
type ToolSecretRef struct {
	// SecretName is the Kubernetes Secret name.
	SecretName string `json:"secretName"`
	// MountPath is where to mount the Secret. Empty means inject as env vars.
	MountPath string `json:"mountPath,omitempty"`
}

// RouterSafeguards configures behavioral guardrails applied by the model-router per run.
type RouterSafeguards struct {
	// MaxConsecutiveNoopTurns halts the run if the LLM produces this many consecutive
	// turns with no tool calls and fewer than MinSubstantiveTokens output tokens.
	// 0 disables the check.
	MaxConsecutiveNoopTurns int `json:"maxConsecutiveNoopTurns,omitempty"`
	// MinSubstantiveTokens is the minimum output token count for a turn to count as substantive.
	// Defaults to 20 when MaxConsecutiveNoopTurns is set.
	MinSubstantiveTokens int `json:"minSubstantiveTokens,omitempty"`
	// MaxRepeatedToolCalls halts the run if the same tool+args hash is seen this many times.
	// 0 disables the check.
	MaxRepeatedToolCalls int `json:"maxRepeatedToolCalls,omitempty"`
	// ToolFrequencyCap halts the run if any single tool is called this many times total.
	// 0 disables the check.
	ToolFrequencyCap int `json:"toolFrequencyCap,omitempty"`
	// ToolExecutionTimeoutSec is the maximum seconds a single tool call may take
	// before being cancelled and returning a structured error to the LLM.
	// 0 means use the default (60s).
	ToolExecutionTimeoutSec int `json:"toolExecutionTimeoutSec,omitempty"`
}

// EpisodicMemoryConfig enables periodic summarization of the conversation to keep the
// active context window compact across long-running runs.
type EpisodicMemoryConfig struct {
	// SummaryEvery is the number of LLM turns between summarization calls. 0 = disabled.
	SummaryEvery int `json:"summaryEvery,omitempty"`
	// SummaryProviderName is the ModelProvider name to use for the summarization call.
	SummaryProviderName string `json:"summaryProviderName,omitempty"`
	// SummaryModel is the LiteLLM model identifier for summarization.
	SummaryModel string `json:"summaryModel,omitempty"`
}

// LongTermMemoryConfig wires a KnowledgeBase as the agent's persistent long-term memory.
type LongTermMemoryConfig struct {
	// Enabled is true when long-term memory is active.
	Enabled bool `json:"enabled,omitempty"`
	// KBName is the KnowledgeBase CR name used for long-term memory.
	KBName string `json:"kbName,omitempty"`
	// VectorStoreURL is the Qdrant gRPC endpoint.
	VectorStoreURL string `json:"vectorStoreURL,omitempty"`
	// CollectionName is the Qdrant collection.
	CollectionName string `json:"collectionName,omitempty"`
	// EmbeddingProviderName is the ModelProvider for embedding queries.
	EmbeddingProviderName string `json:"embeddingProviderName,omitempty"`
	// EmbeddingModel is the LiteLLM model identifier for embeddings.
	EmbeddingModel string `json:"embeddingModel,omitempty"`
	// Dimensions is the embedding vector size.
	Dimensions int `json:"dimensions,omitempty"`
	// TopK is how many memories to retrieve per turn. Defaults to 5.
	TopK int `json:"topK,omitempty"`
}

// HTTPInputConfig describes how the model-router delivers run input to an HTTP agent.
type HTTPInputConfig struct {
	Enabled bool   `json:"enabled"`
	Input   string `json:"input"`
	Port    int    `json:"port"`
	Path    string `json:"path"`
	RunName string `json:"runName"`
}

// WarmRunInput is the payload sent to POST /v1/claim-run when a warm pod is assigned to a run.
type WarmRunInput struct {
	// RunName is the AgentRun name to assign to this warm pod.
	RunName string `json:"runName"`
	// Input is the initial message to deliver to the agent.
	Input string `json:"input"`
	// PriorRunRef is the previous AgentRun name whose checkpoint should be loaded (empty = fresh).
	PriorRunRef string `json:"priorRunRef,omitempty"`
}

// KnowledgeBaseConfig is a resolved KnowledgeBase ready for RAG queries.
type KnowledgeBaseConfig struct {
	// Name is the KnowledgeBase CR name.
	Name string
	// VectorStoreURL is the Qdrant gRPC endpoint.
	VectorStoreURL string
	// CollectionName is the Qdrant collection to search.
	CollectionName string
	// EmbeddingProviderName is the ModelProvider to use for embedding queries.
	EmbeddingProviderName string
	// EmbeddingModel is the LiteLLM model identifier for embeddings.
	EmbeddingModel string
	// Dimensions is the embedding vector size.
	Dimensions int
}

// Message represents a single message in an LLM conversation.
type Message struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"` // string or []ContentPart
	Name       string      `json:"name,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
}

// ToolCall represents an LLM-requested function invocation.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`            // always "function"
	Index    int          `json:"index,omitempty"` // used in streaming deltas to identify which call a chunk belongs to
	Function FunctionCall `json:"function"`
}

// FunctionCall holds the name and arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded arguments
}

// ConfigFromEnv builds a Config from environment variables and the mounted config file.
// The operator writes a JSON config file to /etc/agentorc/router-config.json at pod creation.
func ConfigFromEnv() (*Config, error) {
	configPath := os.Getenv("AGENTORC_ROUTER_CONFIG")
	if configPath == "" {
		configPath = "/etc/agentorc/router-config.json"
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading router config %s: %w", configPath, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing router config: %w", err)
	}

	// Defaults.
	if cfg.KubeAPIURL == "" {
		cfg.KubeAPIURL = "https://kubernetes.default.svc"
	}
	if cfg.CheckpointEvery <= 0 {
		cfg.CheckpointEvery = 1
	}
	if cfg.MetaRouterThreshold == 0 {
		cfg.MetaRouterThreshold = 0.6
	}
	if cfg.OperatorAPIURL == "" {
		cfg.OperatorAPIURL = "http://localhost:8082"
	}
	if cfg.SATokenFile == "" {
		cfg.SATokenFile = "/var/run/secrets/agentorc/token"
	}
	if cfg.Safeguards.MaxConsecutiveNoopTurns > 0 && cfg.Safeguards.MinSubstantiveTokens <= 0 {
		cfg.Safeguards.MinSubstantiveTokens = 20
	}
	if cfg.Safeguards.ToolExecutionTimeoutSec <= 0 {
		cfg.Safeguards.ToolExecutionTimeoutSec = 60
	}
	if cfg.LongTermMemory.Enabled && cfg.LongTermMemory.TopK <= 0 {
		cfg.LongTermMemory.TopK = 5
	}
	if cfg.ContextWindowReserve <= 0 || cfg.ContextWindowReserve >= 1.0 {
		cfg.ContextWindowReserve = 0.20
	}
	if cfg.MaxToolResultTokens <= 0 {
		cfg.MaxToolResultTokens = 4000
	}

	return &cfg, nil
}

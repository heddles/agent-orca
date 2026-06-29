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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// KnowledgeBaseSpec defines a vector store–backed knowledge base for RAG.
// The operator auto-deploys a dedicated Qdrant instance per KnowledgeBase and manages
// document ingestion, chunking, and embedding.
type KnowledgeBaseSpec struct {
	// Description is a human-readable summary of this knowledge base.
	// +optional
	Description string `json:"description,omitempty"`

	// AllowedAgents is the explicit list of Agent names (in the same namespace) that are
	// permitted to use this KnowledgeBase. When set to a non-nil list, only the named
	// agents may access this KnowledgeBase. When omitted (nil), access is unrestricted
	// (backward-compatible default for existing KnowledgeBases).
	//
	// To explicitly deny all agents, set this to an empty list: allowedAgents: [].
	//
	// The operator enforces this at AgentRun/AgentDeployment creation time.
	// At runtime, the operator API verifies the calling agent's identity via
	// TokenReview and rejects requests from agents not in this list.
	// +optional
	AllowedAgents []string `json:"allowedAgents,omitempty"`

	// VectorStore configures the backing vector database.
	// +optional
	VectorStore VectorStoreConfig `json:"vectorStore,omitempty"`

	// Embedding configures how documents are chunked and embedded.
	Embedding EmbeddingConfig `json:"embedding"`

	// Ingestion defines document sources. If nil, documents are only added
	// at runtime via the _rag_ingest tool.
	// +optional
	Ingestion *IngestionConfig `json:"ingestion,omitempty"`
}

// VectorStoreConfig configures the vector database backend.
type VectorStoreConfig struct {
	// Provider is the vector store implementation: "qdrant" (default).
	// +kubebuilder:validation:Enum=qdrant
	// +kubebuilder:default=qdrant
	// +optional
	Provider string `json:"provider,omitempty"`

	// CollectionName overrides the Qdrant collection name.
	// Defaults to the KnowledgeBase CR name.
	// +optional
	CollectionName string `json:"collectionName,omitempty"`

	// StorageSize is the PVC size for the Qdrant StatefulSet.
	// +kubebuilder:default="10Gi"
	// +optional
	StorageSize string `json:"storageSize,omitempty"`

	// Resources overrides default CPU/memory for the Qdrant pod.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`

	// AutoUpgrade controls whether the operator automatically upgrades the
	// Qdrant instance to match the operator's target version. When true
	// (default), the operator walks through sequential minor versions with
	// a snapshot before each step. Set to false to hold the current version.
	// +kubebuilder:default=true
	// +optional
	AutoUpgrade *bool `json:"autoUpgrade,omitempty"`
}

// EmbeddingConfig controls document chunking and embedding.
type EmbeddingConfig struct {
	// ModelSelectorRef names the ModelSelector used for embedding calls.
	// The selected model must support the /v1/embeddings endpoint.
	// +kubebuilder:validation:MinLength=1
	ModelSelectorRef string `json:"modelSelectorRef"`

	// ChunkSize is the maximum token count per document chunk.
	// +kubebuilder:default=512
	// +optional
	ChunkSize int `json:"chunkSize,omitempty"`

	// ChunkOverlap is the token overlap between adjacent chunks.
	// +kubebuilder:default=64
	// +optional
	ChunkOverlap int `json:"chunkOverlap,omitempty"`
}

// IngestionConfig defines where documents come from.
type IngestionConfig struct {
	// ConfigMapRefs lists ConfigMaps whose data keys are treated as documents.
	// +optional
	ConfigMapRefs []LocalObjectRef `json:"configMapRefs,omitempty"`

	// URLs lists HTTP(S) endpoints to fetch and ingest as documents.
	// +optional
	URLs []string `json:"urls,omitempty"`

	// S3 configures S3-compatible object storage as a document source.
	// +optional
	S3 *S3IngestionSource `json:"s3,omitempty"`

	// MCP lists MCP servers to fetch documents from via Kubernetes Jobs.
	// Each entry launches a Job that connects to the referenced MCPServer,
	// runs an optional discover step to enumerate items, then fetches each
	// item. Results are written to a ConfigMap and ingested by the controller.
	// +optional
	MCP []MCPIngestionSource `json:"mcp,omitempty"`

	// SyncIntervalSeconds re-ingests sources on a schedule. 0 means one-shot.
	// +kubebuilder:default=0
	// +optional
	SyncIntervalSeconds int `json:"syncIntervalSeconds,omitempty"`
}

// MCPIngestionSource configures document ingestion from an MCP server.
// The controller launches a Kubernetes Job that connects to the referenced
// MCPServer, runs an optional discover tool call to enumerate items, then
// calls the fetch tool for each item. Documents are returned via a ConfigMap
// and ingested through the standard chunking/embedding pipeline.
type MCPIngestionSource struct {
	// MCPServerRef names the MCPServer CR (same namespace) to use.
	// The MCPServer must include "_controller" in its allowedAgents list.
	// +kubebuilder:validation:MinLength=1
	MCPServerRef string `json:"mcpServerRef"`

	// Discover is an optional tool call that returns a list of items to iterate.
	// If omitted, Fetch runs once with its static arguments.
	// +optional
	Discover *MCPToolCall `json:"discover,omitempty"`

	// ItemExtractor configures how to extract individual items from the
	// discover result:
	//   - "lines": split by newline, skip empty lines (default).
	//   - "jsonArray": parse as a JSON array of strings.
	//   - "jsonObjects": parse as a JSON array of objects; requires ItemField.
	// +kubebuilder:validation:Enum=lines;jsonArray;jsonObjects
	// +kubebuilder:default=lines
	// +optional
	ItemExtractor string `json:"itemExtractor,omitempty"`

	// ItemField selects which field to extract from each JSON object when
	// ItemExtractor is "jsonObjects". For example, "path" extracts the
	// "path" key from each object. Required when ItemExtractor is "jsonObjects".
	// +optional
	ItemField string `json:"itemField,omitempty"`

	// ItemFilter optionally filters JSON objects before extraction (only
	// applies to "jsonObjects"). Objects are included only when the named
	// field equals the specified value. For example, setting key="type" and
	// value="file" keeps only objects where type is "file".
	// +optional
	ItemFilter *ItemFilterConfig `json:"itemFilter,omitempty"`

	// DiscoverRecursive enables recursive directory traversal. When true,
	// items matching DirectoryFilter are re-discovered by calling the
	// Discover tool with "{{item}}" in its arguments replaced by the
	// directory path. Requires Discover.Arguments to contain "{{item}}".
	// +optional
	DiscoverRecursive bool `json:"discoverRecursive,omitempty"`

	// DirectoryFilter identifies directory entries in discover results.
	// Required when DiscoverRecursive is true. Objects matching this
	// filter are queued for recursive discovery rather than fetched.
	// +optional
	DirectoryFilter *ItemFilterConfig `json:"directoryFilter,omitempty"`

	// IncludePatterns is a list of glob patterns. If non-empty, only
	// items matching at least one pattern are kept. Supports "**" for
	// recursive matching (e.g. "**/*.go", "docs/**").
	// +optional
	IncludePatterns []string `json:"includePatterns,omitempty"`

	// ExcludePatterns is a list of glob patterns. Items matching any
	// pattern are dropped (applied after IncludePatterns).
	// Supports "**" for recursive matching (e.g. "vendor/**").
	// +optional
	ExcludePatterns []string `json:"excludePatterns,omitempty"`

	// Fetch is the tool call to execute for each discovered item (or once
	// if no Discover step). String values in Arguments may contain
	// "{{item}}" which is replaced with each discovered item.
	Fetch MCPToolCall `json:"fetch"`

	// MaxItems caps the number of items processed from the discover step.
	// Prevents runaway ingestion. 0 means unlimited.
	// +kubebuilder:default=500
	// +optional
	MaxItems int `json:"maxItems,omitempty"`

	// DocumentIDTemplate is a Go template for generating document IDs.
	// Available variables: {{.Item}}, {{.MCPServer}}, {{.Tool}}.
	// Defaults to "mcp/{{.MCPServer}}/{{.Item}}".
	// +optional
	DocumentIDTemplate string `json:"documentIDTemplate,omitempty"`

	// Resources sets CPU/memory for the ingestion Job pod.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`
}

// ItemFilterConfig filters JSON objects during extraction.
type ItemFilterConfig struct {
	// Key is the JSON field name to check (e.g. "type").
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// Value is the required value for the field (e.g. "file").
	// Objects where the field does not equal this value are excluded.
	// +kubebuilder:validation:MinLength=1
	Value string `json:"value"`
}

// MCPToolCall specifies a single MCP tool invocation.
type MCPToolCall struct {
	// Tool is the MCP tool name to call (e.g. "get_file_contents").
	// +kubebuilder:validation:MinLength=1
	Tool string `json:"tool"`

	// Arguments is a JSON object of tool arguments.
	// String values may contain "{{item}}" placeholders when used in
	// the Fetch step of an MCPIngestionSource.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Arguments *runtime.RawExtension `json:"arguments,omitempty"`
}

// S3IngestionSource configures document ingestion from S3-compatible storage.
type S3IngestionSource struct {
	// Bucket is the S3 bucket name.
	Bucket string `json:"bucket"`

	// Prefix filters objects by key prefix.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region is the AWS region (or S3-compatible endpoint region).
	// +optional
	Region string `json:"region,omitempty"`

	// SecretRef references a Secret containing AWS credentials.
	// Expected keys: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY.
	// +optional
	SecretRef *SecretKeyRef `json:"secretRef,omitempty"`
}

// KnowledgeBaseStatus holds observed state for a KnowledgeBase.
type KnowledgeBaseStatus struct {
	// Ready indicates the vector store is accessible and has been seeded.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// DocumentCount is the number of source documents ingested.
	// +optional
	DocumentCount int `json:"documentCount,omitempty"`

	// ChunkCount is the number of vector chunks stored.
	// +optional
	ChunkCount int `json:"chunkCount,omitempty"`

	// VectorStoreURL is the resolved Qdrant endpoint.
	// +optional
	VectorStoreURL string `json:"vectorStoreURL,omitempty"`

	// CollectionName is the resolved Qdrant collection name.
	// +optional
	CollectionName string `json:"collectionName,omitempty"`

	// EmbeddingModelProvider is the name of the ModelProvider CR used when the
	// Qdrant collection was first created. Once set, all subsequent embedding
	// calls use this provider directly to prevent dimension mismatches caused
	// by a ModelSelector routing to a different model between calls.
	// +optional
	EmbeddingModelProvider string `json:"embeddingModelProvider,omitempty"`

	// EmbeddingModel is the LiteLLM model string (e.g. "openai/text-embedding-3-large")
	// from the pinned ModelProvider.
	// +optional
	EmbeddingModel string `json:"embeddingModel,omitempty"`

	// EmbeddingDimensions is the vector size discovered from the embedding model on first use.
	// The controller probes the embedding API with a test string and records the actual output
	// dimension here. This prevents the user from having to specify dimensions manually and
	// eliminates Qdrant dimension-mismatch errors caused by mismatched configuration.
	// +optional
	EmbeddingDimensions int `json:"embeddingDimensions,omitempty"`

	// StorageUsedPercent is the percentage of PVC storage consumed by Qdrant (0–100).
	// +optional
	StorageUsedPercent int `json:"storageUsedPercent,omitempty"`

	// LastSyncTime is when documents were last ingested.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// QdrantVersion is the currently running Qdrant version as reported by
	// the instance's HTTP API (e.g. "1.17.1"). Empty until the first probe.
	// +optional
	QdrantVersion string `json:"qdrantVersion,omitempty"`

	// QdrantTargetVersion is the operator's desired Qdrant version for this
	// KnowledgeBase. Derived from the operator build or Helm override.
	// +optional
	QdrantTargetVersion string `json:"qdrantTargetVersion,omitempty"`

	// QdrantUpgradeState tracks in-progress sequential upgrades.
	// Empty when no upgrade is in progress.
	// +kubebuilder:validation:Enum="";Snapshot;WaitingForReady;Failed
	// +optional
	QdrantUpgradeState string `json:"qdrantUpgradeState,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Docs",type=integer,JSONPath=`.status.documentCount`
// +kubebuilder:printcolumn:name="Chunks",type=integer,JSONPath=`.status.chunkCount`
// +kubebuilder:printcolumn:name="Storage%",type=integer,JSONPath=`.status.storageUsedPercent`
// +kubebuilder:printcolumn:name="Qdrant",type=string,JSONPath=`.status.qdrantVersion`
// +kubebuilder:printcolumn:name="EmbedProvider",type=string,JSONPath=`.status.embeddingModelProvider`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KnowledgeBase defines a vector store–backed knowledge base for RAG.
// Agents reference KnowledgeBases by name and gain access to _rag_search
// and _rag_ingest built-in tools. The operator auto-deploys a namespace-shared
// Qdrant instance and manages document ingestion, chunking, and embedding.
type KnowledgeBase struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KnowledgeBaseSpec   `json:"spec,omitempty"`
	Status KnowledgeBaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseList contains a list of KnowledgeBase.
type KnowledgeBaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBase `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KnowledgeBase{}, &KnowledgeBaseList{})
}

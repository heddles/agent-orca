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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EgressSinkType is the kind of external message broker to deliver results to.
// +kubebuilder:validation:Enum=kafka;pubsub;redis
type EgressSinkType string

const (
	EgressSinkKafka  EgressSinkType = "kafka"
	EgressSinkPubSub EgressSinkType = "pubsub"
	EgressSinkRedis  EgressSinkType = "redis"
)

// EgressConfig configures durable result delivery to an external message bus.
// When set on an AgentRun, the controller publishes the final TaskResponse
// to the configured sink upon reaching a terminal phase (Succeeded or Failed).
// This is independent of webhook callbacks — both can be configured simultaneously.
type EgressConfig struct {
	// Type is the sink type: kafka, pubsub, or redis.
	// +kubebuilder:validation:Enum=kafka;pubsub;redis
	Type EgressSinkType `json:"type"`

	// Topic is the destination topic/queue/stream name.
	// For Kafka: the topic name. For Pub/Sub: the topic name. For Redis: the stream name.
	// +kubebuilder:validation:MinLength=1
	Topic string `json:"topic"`

	// Brokers is the list of broker addresses (kafka only).
	// Example: ["kafka:9092"]
	// +optional
	Brokers []string `json:"brokers,omitempty"`

	// ProjectID is the GCP project ID (pubsub only).
	// +optional
	ProjectID string `json:"projectID,omitempty"`

	// Address is the Redis connection address (redis only).
	// Example: "redis-master:6379"
	// +optional
	Address string `json:"address,omitempty"`

	// SecretRef references a Secret containing connection credentials.
	// The Secret must be in the same namespace as the AgentRun.
	// For Kafka: key "sasl-username" and "sasl-password" (or "tls-cert"/"tls-key").
	// For Pub/Sub: key "credentials" containing a service-account JSON blob.
	// For Redis: key "password".
	// +optional
	SecretRef *SecretKeyRef `json:"secretRef,omitempty"`

	// Key is the partition key for the published message (kafka only).
	// If empty, the run ID is used as the key.
	// +optional
	Key string `json:"key,omitempty"`
}

// EgressResult is the payload published to the egress sink on terminal phase.
// It mirrors TaskResponse but is self-contained (no HATEOAS links).
type EgressResult struct {
	// RunID is the AgentRun name.
	RunID string `json:"runId"`
	// Agent is the agent name that was invoked.
	Agent string `json:"agent"`
	// Phase is the terminal phase: "Succeeded" or "Failed".
	Phase string `json:"phase"`
	// Output is the agent's final output (truncated to 10KB).
	Output string `json:"output,omitempty"`
	// SpendUSD is the total cost for this run.
	SpendUSD string `json:"spendUSD,omitempty"`
	// FailureReason is set when the run failed.
	FailureReason string `json:"failureReason,omitempty"`
	// CompletedAt is the RFC3339 timestamp of terminal phase.
	CompletedAt string `json:"completedAt,omitempty"`
	// Metadata is the original task submission metadata.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Tenant is the tenant name (for multi-tenant deployments).
	Tenant string `json:"tenant,omitempty"`
}

// SecretKeyRef references a key in a Kubernetes Secret.
type SecretKeyRef struct {
	// Name of the Secret.
	Name string `json:"name"`
	// Key within the Secret.
	Key string `json:"key"`
	// Namespace of the Secret. Defaults to the same namespace as the referencing resource.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// LocalObjectRef references a resource in the same namespace.
type LocalObjectRef struct {
	// Name of the resource.
	Name string `json:"name"`
}

// ResourceRequirements mirrors corev1.ResourceRequirements.
type ResourceRequirements = corev1.ResourceRequirements

// CloudAuthSpec declares cloud-provider identity bindings for an Agent's ServiceAccount.
// The operator translates this to the appropriate provider-specific SA annotations.
// Only one provider block should be set at a time.
type CloudAuthSpec struct {
	// GCP configures GKE Workload Identity.
	// +optional
	GCP *GCPAuthSpec `json:"gcp,omitempty"`
	// AWS configures EKS IRSA (IAM Roles for Service Accounts).
	// +optional
	AWS *AWSAuthSpec `json:"aws,omitempty"`
	// Azure configures AKS Azure Workload Identity.
	// +optional
	Azure *AzureAuthSpec `json:"azure,omitempty"`
}

// GCPAuthSpec configures GKE Workload Identity.
type GCPAuthSpec struct {
	// ServiceAccount is the GCP Service Account email to impersonate.
	// Example: "my-agent@myproject.iam.gserviceaccount.com"
	ServiceAccount string `json:"serviceAccount"`
}

// AWSAuthSpec configures EKS IRSA.
type AWSAuthSpec struct {
	// RoleARN is the AWS IAM Role ARN to assume.
	// Example: "arn:aws:iam::123456789012:role/my-agent-role"
	RoleARN string `json:"roleArn"`
}

// AzureAuthSpec configures Azure Workload Identity.
type AzureAuthSpec struct {
	// ClientID is the Azure Managed Identity client ID.
	ClientID string `json:"clientId"`
}

// EnvVar represents an environment variable with either a literal value or an indirect
// reference to a Kubernetes Secret key. If ValueFrom is set, Value is ignored.
type EnvVar struct {
	// Name of the environment variable.
	Name string `json:"name"`
	// Value is a literal value. Ignored if ValueFrom is set.
	// +optional
	Value string `json:"value,omitempty"`
	// ValueFrom references an indirect source for the value.
	// +optional
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// EnvVarSource selects the value of an environment variable from an indirect source.
type EnvVarSource struct {
	// SecretKeyRef selects a key from a Kubernetes Secret.
	// +optional
	SecretKeyRef *SecretKeyRef `json:"secretKeyRef,omitempty"`
}

// SecretMount references a Kubernetes Secret to mount into a tool pod or sidecar.
type SecretMount struct {
	// Name of the Secret in the same namespace.
	Name string `json:"name"`
	// MountPath is the filesystem path where the Secret should be mounted.
	// If empty, the Secret's keys are injected as environment variables instead.
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}

// EgressRule defines an allowed outbound network target for a tool or agent pod.
type EgressRule struct {
	// Host is the target hostname. Wildcards like "*.example.com" are supported.
	Host string `json:"host"`
	// Port is the target port.
	Port int32 `json:"port"`
	// Protocol defaults to TCP.
	// +optional
	// +kubebuilder:default=TCP
	Protocol string `json:"protocol,omitempty"`
}

// ConversationMessage represents a single message in a conversation history.
type ConversationMessage struct {
	// Role is "user" or "assistant".
	Role string `json:"role"`
	// Content is the message text.
	Content string `json:"content"`
	// TraceEntries is a JSON-encoded array of trace entries (tool calls, tool results, etc.)
	// captured during this assistant turn. Stored as a string to avoid deep-copy complexity.
	// +optional
	TraceEntries string `json:"traceEntries,omitempty"`
}

// CheckpointMetadata tracks statistics about a conversation checkpoint.
type CheckpointMetadata struct {
	// TotalMessages is the count of messages in the conversation.
	TotalMessages int `json:"totalMessages,omitempty"`
	// TotalTokens is the cumulative token usage across all messages.
	TotalTokens int `json:"totalTokens,omitempty"`
	// TotalCostUSD is the cumulative cost in USD.
	TotalCostUSD string `json:"totalCostUSD,omitempty"`
	// StartTime is when the session began.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// LastMessageTime is when the last message was added.
	// +optional
	LastMessageTime *metav1.Time `json:"lastMessageTime,omitempty"`
}

// Checkpoint represents a compressed conversation state for a session.
// Stored in the checkpoint store (S3, GCS, etc) and referenced by CheckpointRef.
type Checkpoint struct {
	// SessionID is the unique session identifier (e.g., "user-42").
	SessionID string `json:"sessionId"`
	// Version is incremented with each message exchange.
	Version int `json:"version"`
	// ConversationHistory holds the full message transcript.
	ConversationHistory []ConversationMessage `json:"conversationHistory,omitempty"`
	// Metadata tracks stats about the conversation.
	Metadata CheckpointMetadata `json:"metadata,omitempty"`
	// LastRunRef is the name of the most recently created AgentRun in this session.
	// Used to chain conversation context across turns in an AgentDeployment chat session.
	// +optional
	LastRunRef string `json:"lastRunRef,omitempty"`
}

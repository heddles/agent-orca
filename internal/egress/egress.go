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

// Package egress provides durable result delivery from the agent-orc controller
// to external message buses (Kafka, Pub/Sub, Redis Streams).
//
// When an AgentRun reaches a terminal phase (Succeeded or Failed) and its
// spec.egress field is set, the controller publishes the final TaskResponse
// payload to the configured sink. This is independent of webhook callbacks —
// both can be configured simultaneously.
//
// Delivery semantics:
//   - Kafka: at-least-once (producer acks = 1), keyed by run ID for ordering.
//   - Pub/Sub: at-least-once (ack deadline = 10s), keyed by run ID.
//   - Redis: XADD to a stream, at-most-once (fire-and-forget).
//
// All publishers include an "agentorc-run-id" header (or message attribute)
// so consumers can deduplicate.
package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

// Publisher is the interface implemented by all egress sink publishers.
type Publisher interface {
	// Publish delivers the result payload to the external sink.
	Publish(ctx context.Context, result agentorcv1alpha1.EgressResult) error
	// Close releases any underlying connections.
	Close() error
}

// metrics holds Prometheus counters for egress delivery.
var metrics = struct {
	published prometheus.Counter
	failed    prometheus.Counter
}{
	published: promauto.NewCounter(prometheus.CounterOpts{
		Name: "agentorc_egress_published_total",
		Help: "Total number of results successfully published to egress sinks.",
	}),
	failed: promauto.NewCounter(prometheus.CounterOpts{
		Name: "agentorc_egress_failed_total",
		Help: "Total number of results that failed to publish to egress sinks.",
	}),
}

// NewPublisher creates the appropriate Publisher for the given EgressConfig.
// It reads credentials from the referenced Secret (if any) via the K8s client.
func NewPublisher(ctx context.Context, cfg agentorcv1alpha1.EgressConfig, k8sClient client.Client, namespace string) (Publisher, error) {
	switch cfg.Type {
	case agentorcv1alpha1.EgressSinkKafka:
		return newKafkaPublisher(ctx, cfg, k8sClient, namespace)
	case agentorcv1alpha1.EgressSinkPubSub:
		return newPubSubPublisher(ctx, cfg, k8sClient, namespace)
	case agentorcv1alpha1.EgressSinkRedis:
		return newRedisPublisher(ctx, cfg, k8sClient, namespace)
	default:
		return nil, fmt.Errorf("unsupported egress sink type: %s", cfg.Type)
	}
}

// PublishAndRecord publishes the result and updates metrics. It is the
// convenience entry point used by the controller's terminal-phase handler.
func PublishAndRecord(ctx context.Context, p Publisher, result agentorcv1alpha1.EgressResult) {
	payload, _ := json.Marshal(result)
	slog.Info("egress: publishing result",
		"runId", result.RunID,
		"phase", result.Phase,
		"sink", result.RunID, // placeholder; real sink name is in the publisher
	)

	if err := p.Publish(ctx, result); err != nil {
		slog.Error("egress: publish failed", "runId", result.RunID, "err", err)
		metrics.failed.Inc()
		return
	}
	slog.Info("egress: published successfully", "runId", result.RunID, "payloadBytes", len(payload))
	metrics.published.Inc()
}

// readSecretKey reads a single key from a Kubernetes Secret.
func readSecretKey(ctx context.Context, k8sClient client.Client, namespace, secretName, key string) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %s/%s not found", namespace, secretName)
		}
		return nil, fmt.Errorf("reading secret %s/%s: %w", namespace, secretName, err)
	}
	val, ok := secret.Data[key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in secret %s/%s", key, namespace, secretName)
	}
	return val, nil
}

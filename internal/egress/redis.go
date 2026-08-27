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

package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// redisPublisher delivers results to a Redis stream via XADD.
// Delivery is fire-and-forget (at-most-once); if the Redis connection
// drops the message is lost. For at-least-once semantics use Kafka or Pub/Sub.
type redisPublisher struct {
	client *redis.Client
	stream string
}

func newRedisPublisher(ctx context.Context, cfg agentorcav1alpha1.EgressConfig, k8sClient client.Client, namespace string) (*redisPublisher, error) {
	if cfg.Topic == "" {
		return nil, fmt.Errorf("egress.redis: topic (stream name) must be specified")
	}
	if cfg.Address == "" {
		return nil, fmt.Errorf("egress.redis: address must be specified")
	}

	opts := &redis.Options{
		Addr: cfg.Address,
	}

	if cfg.SecretRef != nil {
		pass, err := readSecretKey(ctx, k8sClient, namespace, cfg.SecretRef.Name, "password")
		if err != nil {
			return nil, fmt.Errorf("egress.redis: reading password: %w", err)
		}
		opts.Password = string(pass)
	}

	rdb := redis.NewClient(opts)
	// Verify connectivity.
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("egress.redis: ping failed: %w", err)
	}

	return &redisPublisher{client: rdb, stream: cfg.Topic}, nil
}

func (p *redisPublisher) Publish(ctx context.Context, result agentorcav1alpha1.EgressResult) error {
	if p.client == nil {
		return fmt.Errorf("redis publisher not initialized")
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshaling egress result: %w", err)
	}

	values := map[string]any{
		"run-id":    result.RunID,
		"phase":     result.Phase,
		"tenant":    result.Tenant,
		"payload":   string(payload),
		"timestamp": result.CompletedAt,
	}

	if err := p.client.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		Values: values,
	}).Err(); err != nil {
		return fmt.Errorf("redis XADD: %w", err)
	}

	slog.Debug("egress.redis: published", "stream", p.stream, "runId", result.RunID)
	return nil
}

func (p *redisPublisher) Close() error {
	if p.client != nil {
		return p.client.Close()
	}
	return nil
}

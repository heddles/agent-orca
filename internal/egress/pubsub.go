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

	"cloud.google.com/go/pubsub"
	"google.golang.org/api/option"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// pubsubPublisher delivers results to a Google Cloud Pub/Sub topic.
type pubsubPublisher struct {
	client *pubsub.Client
	topic  *pubsub.Topic
}

func newPubSubPublisher(ctx context.Context, cfg agentorcav1alpha1.EgressConfig, k8sClient client.Client, namespace string) (*pubsubPublisher, error) {
	if cfg.Topic == "" {
		return nil, fmt.Errorf("egress.pubsub: topic must be specified")
	}
	if cfg.ProjectID == "" {
		return nil, fmt.Errorf("egress.pubsub: projectID must be specified")
	}

	var opts []option.ClientOption
	if cfg.SecretRef != nil {
		creds, err := readSecretKey(ctx, k8sClient, namespace, cfg.SecretRef.Name, "credentials")
		if err != nil {
			return nil, fmt.Errorf("egress.pubsub: reading credentials: %w", err)
		}
		opts = append(opts, option.WithCredentialsJSON(creds))
	}

	psClient, err := pubsub.NewClient(ctx, cfg.ProjectID, opts...)
	if err != nil {
		return nil, fmt.Errorf("egress.pubsub: creating client: %w", err)
	}

	topic := psClient.Topic(cfg.Topic)
	if exists, err := topic.Exists(ctx); err != nil {
		return nil, fmt.Errorf("egress.pubsub: checking topic existence: %w", err)
	} else if !exists {
		return nil, fmt.Errorf("egress.pubsub: topic %q does not exist in project %q", cfg.Topic, cfg.ProjectID)
	}

	return &pubsubPublisher{client: psClient, topic: topic}, nil
}

func (p *pubsubPublisher) Publish(ctx context.Context, result agentorcav1alpha1.EgressResult) error {
	if p.topic == nil {
		return fmt.Errorf("pubsub publisher not initialized")
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshaling egress result: %w", err)
	}

	key := result.RunID
	if result.Tenant != "" {
		key = result.Tenant + "/" + result.RunID
	}

	res := p.topic.Publish(ctx, &pubsub.Message{
		Data: payload,
		Attributes: map[string]string{
			"agentorca-run-id": result.RunID,
			"agentorca-phase":  result.Phase,
			"agentorca-tenant": result.Tenant,
			"agentorca-key":    key,
		},
	})

	// Wait for the publish result (at-least-once by default).
	_, err = res.Get(ctx)
	if err != nil {
		return fmt.Errorf("pubsub publish: %w", err)
	}

	slog.Debug("egress.pubsub: published", "topic", p.topic.String(), "runId", result.RunID)
	return nil
}

func (p *pubsubPublisher) Close() error {
	if p.client != nil {
		return p.client.Close()
	}
	return nil
}

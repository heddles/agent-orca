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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// kafkaPublisher delivers results to a Kafka topic.
type kafkaPublisher struct {
	writer *kafka.Writer
	topic  string
}

func newKafkaPublisher(ctx context.Context, cfg agentorcav1alpha1.EgressConfig, k8sClient client.Client, namespace string) (*kafkaPublisher, error) {
	if len(cfg.Brokers) == 0 {
		return nil, fmt.Errorf("egress.kafka: brokers must be specified")
	}
	if cfg.Topic == "" {
		return nil, fmt.Errorf("egress.kafka: topic must be specified")
	}

	dialer := &kafka.Dialer{}
	if cfg.SecretRef != nil {
		saslMechanism, err := buildSASLMechanism(ctx, k8sClient, namespace, cfg.SecretRef.Name)
		if err != nil {
			return nil, fmt.Errorf("egress.kafka: configuring SASL: %w", err)
		}
		if saslMechanism != nil {
			dialer.SASLMechanism = saslMechanism
		}

		// TLS if cert/key are present in the secret.
		if hasKeys(k8sClient, ctx, namespace, cfg.SecretRef.Name, "tls-cert", "tls-key") {
			tlsCfg, err := buildTLSConfig(ctx, k8sClient, namespace, cfg.SecretRef.Name)
			if err != nil {
				return nil, fmt.Errorf("egress.kafka: configuring TLS: %w", err)
			}
			dialer.TLS = tlsCfg
		}
	}

	transport := &kafka.Transport{
		SASL: dialer.SASLMechanism,
		TLS:  dialer.TLS,
	}

	writer := &kafka.Writer{
		Addr:         kafka.TCP(cfg.Brokers...),
		Topic:        cfg.Topic,
		Balancer:     &kafka.LeastBytes{},
		Transport:    transport,
		RequiredAcks: kafka.RequireAll,
		Async:        false,
	}

	return &kafkaPublisher{writer: writer, topic: cfg.Topic}, nil
}

func (p *kafkaPublisher) Publish(ctx context.Context, result agentorcav1alpha1.EgressResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshaling egress result: %w", err)
	}

	key := result.RunID
	if result.Tenant != "" {
		key = result.Tenant + "/" + result.RunID
	}

	msg := kafka.Message{
		Key:   []byte(key),
		Value: payload,
		Headers: []kafka.Header{
			{Key: "agentorca-run-id", Value: []byte(result.RunID)},
			{Key: "agentorca-phase", Value: []byte(result.Phase)},
		},
	}

	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("kafka write: %w", err)
	}
	slog.Debug("egress.kafka: published", "topic", p.topic, "runId", result.RunID)
	return nil
}

func (p *kafkaPublisher) Close() error {
	return p.writer.Close()
}

// buildSASLMechanism creates a SASL mechanism from the credentials in the Secret.
// Supports PLAIN (sasl-username/sasl-password) and SCRAM-SHA-256/512
// (sasl-username/sasl-password/sasl-mechanism).
func buildSASLMechanism(ctx context.Context, k8sClient client.Client, namespace, secretName string) (sasl.Mechanism, error) {
	user, err := readSecretKey(ctx, k8sClient, namespace, secretName, "sasl-username")
	if err != nil {
		return nil, err
	}
	pass, err := readSecretKey(ctx, k8sClient, namespace, secretName, "sasl-password")
	if err != nil {
		return nil, err
	}

	mechBytes, _ := readSecretKey(ctx, k8sClient, namespace, secretName, "sasl-mechanism")
	mechanism := strings.ToLower(string(mechBytes))

	switch mechanism {
	case "", "plain":
		return plain.Mechanism{
			Username: string(user),
			Password: string(pass),
		}, nil
	case "scram-sha-256":
		return scram.Mechanism(
			scram.SHA256,
			string(user),
			string(pass),
		)
	case "scram-sha-512":
		return scram.Mechanism(
			scram.SHA512,
			string(user),
			string(pass),
		)
	default:
		return nil, fmt.Errorf("unsupported SASL mechanism: %s", mechanism)
	}
}

// buildTLSConfig creates a TLS config from cert/key (and optional CA) in the Secret.
func buildTLSConfig(ctx context.Context, k8sClient client.Client, namespace, secretName string) (*tls.Config, error) {
	certPEM, err := readSecretKey(ctx, k8sClient, namespace, secretName, "tls-cert")
	if err != nil {
		return nil, err
	}
	keyPEM, err := readSecretKey(ctx, k8sClient, namespace, secretName, "tls-key")
	if err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading TLS key pair: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// Optional CA cert.
	caBytes, err := readSecretKey(ctx, k8sClient, namespace, secretName, "tls-ca")
	if err == nil {
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("failed to append CA cert from secret %s", secretName)
		}
		tlsCfg.RootCAs = caPool
	}

	return tlsCfg, nil
}

// hasKeys checks whether all the given keys exist in the Secret.
func hasKeys(k8sClient client.Client, ctx context.Context, namespace, secretName string, keys ...string) bool {
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: secretName, Namespace: namespace}, secret); err != nil {
		return false
	}
	for _, k := range keys {
		if _, ok := secret.Data[k]; !ok {
			return false
		}
	}
	return true
}

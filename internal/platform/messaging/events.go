package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/platform/postgres"
)

type EventPublisher struct {
	client   *sqs.Client
	queueURL string
}

func NewEventPublisher(lc fx.Lifecycle, c config.Config) *EventPublisher {
	p := &EventPublisher{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
			defer cancel()
			client := &http.Client{Transport: transport, Timeout: c.DependencyTimeout}
			cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.AWSRegion), awsconfig.WithHTTPClient(client))
			if err != nil {
				transport.CloseIdleConnections()
				return fmt.Errorf("load event publisher AWS configuration failed")
			}
			p.client = sqs.NewFromConfig(cfg, func(o *sqs.Options) {
				// Durable retries belong to the outbox, not an unbounded SDK loop.
				o.RetryMaxAttempts = 1
				if c.SQSEndpoint != "" {
					o.BaseEndpoint = aws.String(c.SQSEndpoint)
				}
			})
			queue, err := p.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.SQSEventsQueueName)})
			if err != nil {
				transport.CloseIdleConnections()
				return fmt.Errorf("event FIFO queue lookup failed")
			}
			p.queueURL = aws.ToString(queue.QueueUrl)
			attributes, err := p.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
				QueueUrl: queue.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameFifoQueue},
			})
			if err != nil || attributes.Attributes["FifoQueue"] != "true" {
				transport.CloseIdleConnections()
				return fmt.Errorf("event queue must be an available FIFO queue")
			}
			return nil
		},
		OnStop: func(context.Context) error { transport.CloseIdleConnections(); return nil },
	})
	return p
}

// BuildEventMessage extracts only routing metadata. The message body is the
// persisted JSONB snapshot, byte for byte; financial values are never decoded.
func BuildEventMessage(queueURL string, event postgres.ClaimedEvent) (*sqs.SendMessageInput, error) {
	var metadata struct {
		EventID string `json:"eventId"`
		Data    struct {
			WalletID string `json:"walletId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(event.Payload, &metadata); err != nil {
		return nil, fmt.Errorf("invalid outbox envelope")
	}
	if metadata.EventID == "" || metadata.EventID != event.EventID || metadata.Data.WalletID == "" || metadata.Data.WalletID != event.WalletID {
		return nil, fmt.Errorf("outbox routing metadata mismatch")
	}
	// Hex SHA-256 supports arbitrary domain identifiers within SQS's 128 bytes.
	group := sha256.Sum256([]byte(metadata.Data.WalletID))
	dedup := sha256.Sum256([]byte(metadata.EventID))
	return &sqs.SendMessageInput{
		QueueUrl: aws.String(queueURL), MessageBody: aws.String(string(event.Payload)),
		MessageGroupId:         aws.String(hex.EncodeToString(group[:])),
		MessageDeduplicationId: aws.String(hex.EncodeToString(dedup[:])),
	}, nil
}

func (p *EventPublisher) Publish(ctx context.Context, event postgres.ClaimedEvent) error {
	input, err := BuildEventMessage(p.queueURL, event)
	if err != nil {
		return err
	}
	_, err = p.client.SendMessage(ctx, input)
	return err
}

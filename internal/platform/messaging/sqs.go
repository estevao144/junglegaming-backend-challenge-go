package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"
	"jungle-gaming/internal/config"
)

var Module = fx.Module("messaging", fx.Provide(New))

type Queue struct {
	client  *sqs.Client
	url     string
	source  string
	dlqName string
	config  config.Config
}

func New(lc fx.Lifecycle, c config.Config) *Queue {
	q := &Queue{config: c}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport, Timeout: time.Duration(c.ConsumerWaitSeconds)*time.Second + c.DependencyTimeout}
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		started := false
		defer func() {
			if !started {
				transport.CloseIdleConnections()
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
		defer cancel()
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.AWSRegion), awsconfig.WithHTTPClient(client))
		if err != nil {
			return fmt.Errorf("load AWS configuration failed")
		}
		q.client = sqs.NewFromConfig(cfg, func(o *sqs.Options) {
			o.RetryMaxAttempts = 1
			if c.SQSEndpoint != "" {
				o.BaseEndpoint = aws.String(c.SQSEndpoint)
			}
		})
		result, err := q.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.SQSQueueName)})
		if err != nil {
			return fmt.Errorf("SQS queue lookup failed")
		}
		q.url = aws.ToString(result.QueueUrl)
		if err := q.Check(ctx); err != nil {
			return fmt.Errorf("SQS startup check failed")
		}
		attributes, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: result.QueueUrl,
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn, types.QueueAttributeNameFifoQueue, types.QueueAttributeNameRedrivePolicy}})
		if err != nil || attributes.Attributes["FifoQueue"] != "true" || attributes.Attributes["RedrivePolicy"] == "" {
			return fmt.Errorf("input queue must be FIFO with redrive policy")
		}
		q.source = attributes.Attributes["QueueArn"]
		var redrive struct {
			DeadLetterTargetARN string `json:"deadLetterTargetArn"`
		}
		if json.Unmarshal([]byte(attributes.Attributes["RedrivePolicy"]), &redrive) != nil {
			return fmt.Errorf("invalid SQS redrive policy")
		}
		arnParts := strings.SplitN(redrive.DeadLetterTargetARN, ":", 6)
		if len(arnParts) != 6 || arnParts[5] == "" {
			return fmt.Errorf("invalid SQS DLQ identity")
		}
		q.dlqName = arnParts[5]
		if q.source == "" {
			return fmt.Errorf("input queue ARN is required for stable inbox identity")
		}
		started = true
		return nil
	}, OnStop: func(context.Context) error {
		transport.CloseIdleConnections()
		return nil
	}})
	return q
}

func (q *Queue) Source() string { return q.source }

func (q *Queue) Receive(ctx context.Context) ([]types.Message, error) {
	result, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(q.url),
		MaxNumberOfMessages: int32(q.config.ConsumerBatchSize), WaitTimeSeconds: int32(q.config.ConsumerWaitSeconds),
		VisibilityTimeout:           int32(q.config.ConsumerVisibilitySeconds),
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount}})
	if err != nil {
		return nil, err
	}
	return result.Messages, nil
}

func (q *Queue) Delete(ctx context.Context, message types.Message) error {
	_, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(q.url), ReceiptHandle: message.ReceiptHandle})
	return err
}

func (q *Queue) Check(ctx context.Context) error {
	_, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(q.url), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	return err
}

func (q *Queue) DLQVisible(ctx context.Context) (int64, error) {
	result, err := q.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(q.dlqName)})
	if err != nil {
		return 0, err
	}
	attributes, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: result.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(attributes.Attributes["ApproximateNumberOfMessages"], 10, 64)
}

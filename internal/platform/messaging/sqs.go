package messaging

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"
	"jungle-gaming/internal/config"
)

var Module = fx.Module("messaging", fx.Provide(New))

type Queue struct {
	client *sqs.Client
	url    string
}

func New(lc fx.Lifecycle, c config.Config) *Queue {
	q := &Queue{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport, Timeout: c.DependencyTimeout}
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
		started = true
		return nil
	}, OnStop: func(context.Context) error {
		transport.CloseIdleConnections()
		return nil
	}})
	return q
}

func (q *Queue) Check(ctx context.Context) error {
	_, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(q.url), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	return err
}

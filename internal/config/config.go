package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/fx"
)

var Module = fx.Module("config", fx.Provide(Load))

type Config struct {
	HTTPAddress               string
	DatabaseURL               string
	AWSRegion                 string
	SQSEndpoint               string
	SQSQueueName              string
	LogLevel                  slog.Level
	DependencyTimeout         time.Duration
	SQSEventsQueueName        string
	OutboxBatchSize           int
	OutboxPollInterval        time.Duration
	OutboxLease               time.Duration
	OutboxRetryBase           time.Duration
	OutboxRetryMax            time.Duration
	ConsumerName              string
	ConsumerConcurrency       int
	ConsumerBatchSize         int
	ConsumerWaitSeconds       int
	ConsumerVisibilitySeconds int
	ConsumerProcessTimeout    time.Duration
	ReferenceBatchSize        int
	ReferencePollInterval     time.Duration
	ReferenceLease            time.Duration
	ReferenceProcessTimeout   time.Duration
	ReferenceRetryBase        time.Duration
	ReferenceRetryMax         time.Duration
	ReferenceMaxAttempts      int
}

// Load reads process environment. It deliberately does not load .env files.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}
		return fallback
	}
	c := Config{
		HTTPAddress:        get("HTTP_ADDR", ":8080"),
		DatabaseURL:        get("DATABASE_URL", ""),
		AWSRegion:          get("AWS_REGION", "us-east-1"),
		SQSEndpoint:        get("SQS_ENDPOINT", ""),
		SQSQueueName:       get("SQS_QUEUE_NAME", "wager-transactions.fifo"),
		SQSEventsQueueName: get("SQS_EVENTS_QUEUE_NAME", "wager-events.fifo"),
	}
	_, port, err := net.SplitHostPort(c.HTTPAddress)
	if err != nil {
		return Config{}, fmt.Errorf("HTTP_ADDR must be host:port")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return Config{}, fmt.Errorf("HTTP_ADDR port must be between 0 and 65535")
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" || u.Path == "" || u.Path == "/" {
		return Config{}, fmt.Errorf("DATABASE_URL must be a PostgreSQL URL with host and database")
	}
	if strings.TrimSpace(c.AWSRegion) == "" {
		return Config{}, fmt.Errorf("AWS_REGION is required")
	}
	if strings.TrimSpace(c.SQSQueueName) == "" || !strings.HasSuffix(c.SQSQueueName, ".fifo") {
		return Config{}, fmt.Errorf("SQS_QUEUE_NAME must name a FIFO queue")
	}
	if c.SQSEndpoint != "" {
		u, err := url.Parse(c.SQSEndpoint)
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("SQS_ENDPOINT must be an HTTP(S) URL")
		}
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "INFO"))); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL must be DEBUG, INFO, WARN or ERROR")
	}
	c.DependencyTimeout, err = time.ParseDuration(get("DEPENDENCY_TIMEOUT", "3s"))
	if err != nil || c.DependencyTimeout <= 0 || c.DependencyTimeout > 10*time.Second {
		return Config{}, fmt.Errorf("DEPENDENCY_TIMEOUT must be positive and at most 10s")
	}
	if !strings.HasSuffix(c.SQSEventsQueueName, ".fifo") || c.SQSEventsQueueName == c.SQSQueueName {
		return Config{}, fmt.Errorf("SQS_EVENTS_QUEUE_NAME must name a separate FIFO queue")
	}
	c.OutboxBatchSize, err = strconv.Atoi(get("OUTBOX_BATCH_SIZE", "10"))
	if err != nil || c.OutboxBatchSize < 1 || c.OutboxBatchSize > 100 {
		return Config{}, fmt.Errorf("OUTBOX_BATCH_SIZE must be between 1 and 100")
	}
	for _, setting := range []struct {
		name     string
		fallback string
		value    *time.Duration
	}{
		{"OUTBOX_POLL_INTERVAL", "1s", &c.OutboxPollInterval},
		{"OUTBOX_LEASE", "30s", &c.OutboxLease},
		{"OUTBOX_RETRY_BASE", "1s", &c.OutboxRetryBase},
		{"OUTBOX_RETRY_MAX", "1m", &c.OutboxRetryMax},
	} {
		*setting.value, err = time.ParseDuration(get(setting.name, setting.fallback))
		if err != nil || *setting.value <= 0 || *setting.value > 24*time.Hour {
			return Config{}, fmt.Errorf("%s must be positive and at most 24h", setting.name)
		}
	}
	if c.OutboxLease < 3*c.DependencyTimeout {
		return Config{}, fmt.Errorf("OUTBOX_LEASE must be at least three times DEPENDENCY_TIMEOUT")
	}
	if c.OutboxRetryMax < c.OutboxRetryBase {
		return Config{}, fmt.Errorf("OUTBOX_RETRY_MAX must be at least OUTBOX_RETRY_BASE")
	}
	c.ConsumerName = get("SQS_CONSUMER_NAME", "financial-operations-v1")
	if c.ConsumerName == "" || strings.TrimSpace(c.ConsumerName) != c.ConsumerName {
		return Config{}, fmt.Errorf("SQS_CONSUMER_NAME must be a nonempty identity")
	}
	for _, setting := range []struct {
		name     string
		fallback string
		value    *int
		maximum  int
	}{
		{"SQS_CONSUMER_CONCURRENCY", "2", &c.ConsumerConcurrency, 10},
		{"SQS_CONSUMER_BATCH_SIZE", "1", &c.ConsumerBatchSize, 10},
		{"SQS_CONSUMER_WAIT_SECONDS", "20", &c.ConsumerWaitSeconds, 20},
		{"SQS_VISIBILITY_SECONDS", "30", &c.ConsumerVisibilitySeconds, 43200},
	} {
		*setting.value, err = strconv.Atoi(get(setting.name, setting.fallback))
		if err != nil || *setting.value < 1 || *setting.value > setting.maximum {
			return Config{}, fmt.Errorf("%s must be between 1 and %d", setting.name, setting.maximum)
		}
	}
	c.ConsumerProcessTimeout, err = time.ParseDuration(get("SQS_PROCESS_TIMEOUT", "5s"))
	if err != nil || c.ConsumerProcessTimeout <= 0 || c.ConsumerProcessTimeout > 10*time.Second {
		return Config{}, fmt.Errorf("SQS_PROCESS_TIMEOUT must be positive and at most 10s")
	}
	budget := time.Duration(c.ConsumerBatchSize)*(c.ConsumerProcessTimeout+c.DependencyTimeout) + c.DependencyTimeout
	if time.Duration(c.ConsumerVisibilitySeconds)*time.Second <= budget {
		return Config{}, fmt.Errorf("SQS_VISIBILITY_SECONDS must exceed the batch processing and acknowledgement budget")
	}
	c.ReferenceBatchSize, err = strconv.Atoi(get("REFERENCE_BATCH_SIZE", "5"))
	if err != nil || c.ReferenceBatchSize < 1 || c.ReferenceBatchSize > 20 {
		return Config{}, fmt.Errorf("REFERENCE_BATCH_SIZE must be between 1 and 20")
	}
	c.ReferenceMaxAttempts, err = strconv.Atoi(get("REFERENCE_MAX_ATTEMPTS", "10"))
	if err != nil || c.ReferenceMaxAttempts < 1 || c.ReferenceMaxAttempts > 1000 {
		return Config{}, fmt.Errorf("REFERENCE_MAX_ATTEMPTS must be between 1 and 1000")
	}
	for _, setting := range []struct {
		name     string
		fallback string
		value    *time.Duration
	}{
		{"REFERENCE_POLL_INTERVAL", "1s", &c.ReferencePollInterval},
		{"REFERENCE_LEASE", "30s", &c.ReferenceLease},
		{"REFERENCE_PROCESS_TIMEOUT", "5s", &c.ReferenceProcessTimeout},
		{"REFERENCE_RETRY_BASE", "5s", &c.ReferenceRetryBase},
		{"REFERENCE_RETRY_MAX", "5m", &c.ReferenceRetryMax},
	} {
		*setting.value, err = time.ParseDuration(get(setting.name, setting.fallback))
		if err != nil || *setting.value < time.Microsecond || *setting.value > 24*time.Hour {
			return Config{}, fmt.Errorf("%s must be at least 1us and at most 24h", setting.name)
		}
	}
	if c.ReferenceProcessTimeout > 10*time.Second {
		return Config{}, fmt.Errorf("REFERENCE_PROCESS_TIMEOUT must be at most 10s")
	}
	if c.ReferenceRetryMax < c.ReferenceRetryBase {
		return Config{}, fmt.Errorf("REFERENCE_RETRY_MAX must be at least REFERENCE_RETRY_BASE")
	}
	if c.ReferenceLease <= time.Duration(c.ReferenceBatchSize)*c.ReferenceProcessTimeout+c.DependencyTimeout {
		return Config{}, fmt.Errorf("REFERENCE_LEASE must exceed the batch processing and claim budget")
	}
	return c, nil
}

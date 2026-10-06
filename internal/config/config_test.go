package config

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, test := range []struct{ name, key, value, wantError string }{
		{name: "defaults"},
		{name: "reference attempts zero", key: "REFERENCE_MAX_ATTEMPTS", value: "0", wantError: "REFERENCE_MAX_ATTEMPTS"},
		{name: "reference batch zero", key: "REFERENCE_BATCH_SIZE", value: "0", wantError: "REFERENCE_BATCH_SIZE"},
		{name: "reference unsafe lease", key: "REFERENCE_LEASE", value: "1s", wantError: "REFERENCE_LEASE"},
		{name: "reference retry cap", key: "REFERENCE_RETRY_MAX", value: "1ms", wantError: "REFERENCE_RETRY_MAX"},
		{name: "reference zero poll", key: "REFERENCE_POLL_INTERVAL", value: "0s", wantError: "REFERENCE_POLL_INTERVAL"},
		{name: "reference resolution timeout", key: "REFERENCE_PROCESS_TIMEOUT", value: "11s", wantError: "REFERENCE_PROCESS_TIMEOUT"},
		{name: "missing database", key: "DATABASE_URL", value: "", wantError: "DATABASE_URL"},
		{name: "invalid database", key: "DATABASE_URL", value: "password=secret", wantError: "DATABASE_URL"},
		{name: "invalid address", key: "HTTP_ADDR", value: "localhost", wantError: "HTTP_ADDR"},
		{name: "invalid port", key: "HTTP_ADDR", value: ":70000", wantError: "HTTP_ADDR"},
		{name: "negative timeout", key: "DEPENDENCY_TIMEOUT", value: "-1s", wantError: "DEPENDENCY_TIMEOUT"},
		{name: "timeout exceeds shutdown budget", key: "DEPENDENCY_TIMEOUT", value: "1m", wantError: "DEPENDENCY_TIMEOUT"},
		{name: "empty region", key: "AWS_REGION", value: "", wantError: "AWS_REGION"},
		{name: "invalid queue", key: "SQS_QUEUE_NAME", value: "standard", wantError: "SQS_QUEUE_NAME"},
		{name: "invalid endpoint", key: "SQS_ENDPOINT", value: "localhost:4566", wantError: "SQS_ENDPOINT"},
		{name: "invalid level", key: "LOG_LEVEL", value: "TRACE", wantError: "LOG_LEVEL"},
		{name: "invalid event queue", key: "SQS_EVENTS_QUEUE_NAME", value: "standard", wantError: "SQS_EVENTS_QUEUE_NAME"},
		{name: "same event queue", key: "SQS_EVENTS_QUEUE_NAME", value: "wager-transactions.fifo", wantError: "SQS_EVENTS_QUEUE_NAME"},
		{name: "zero batch", key: "OUTBOX_BATCH_SIZE", value: "0", wantError: "OUTBOX_BATCH_SIZE"},
		{name: "large batch", key: "OUTBOX_BATCH_SIZE", value: "101", wantError: "OUTBOX_BATCH_SIZE"},
		{name: "invalid poll", key: "OUTBOX_POLL_INTERVAL", value: "0s", wantError: "OUTBOX_POLL_INTERVAL"},
		{name: "short lease", key: "OUTBOX_LEASE", value: "1s", wantError: "OUTBOX_LEASE"},
		{name: "invalid retry", key: "OUTBOX_RETRY_BASE", value: "-1s", wantError: "OUTBOX_RETRY_BASE"},
		{name: "retry cap", key: "OUTBOX_RETRY_MAX", value: "1ms", wantError: "OUTBOX_RETRY_MAX"},
		{name: "empty consumer", key: "SQS_CONSUMER_NAME", value: "", wantError: "SQS_CONSUMER_NAME"},
		{name: "zero concurrency", key: "SQS_CONSUMER_CONCURRENCY", value: "0", wantError: "SQS_CONSUMER_CONCURRENCY"},
		{name: "large consumer batch", key: "SQS_CONSUMER_BATCH_SIZE", value: "11", wantError: "SQS_CONSUMER_BATCH_SIZE"},
		{name: "long polling range", key: "SQS_CONSUMER_WAIT_SECONDS", value: "21", wantError: "SQS_CONSUMER_WAIT_SECONDS"},
		{name: "unsafe visibility", key: "SQS_VISIBILITY_SECONDS", value: "2", wantError: "SQS_VISIBILITY_SECONDS"},
		{name: "invalid process timeout", key: "SQS_PROCESS_TIMEOUT", value: "0s", wantError: "SQS_PROCESS_TIMEOUT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{"DATABASE_URL": "postgres://user:secret@localhost:5432/jungle"}
			if test.key != "" {
				values[test.key] = test.value
			}
			c, err := load(func(key string) (string, bool) { v, ok := values[key]; return v, ok })
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("expected %s validation error, got %v", test.wantError, err)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("configuration error exposed credentials")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.HTTPAddress != ":8080" || c.SQSQueueName != "wager-transactions.fifo" {
				t.Fatalf("unexpected defaults: address=%s queue=%s", c.HTTPAddress, c.SQSQueueName)
			}
		})
	}
}

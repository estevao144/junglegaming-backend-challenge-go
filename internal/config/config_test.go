package config

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, test := range []struct{ name, key, value, wantError string }{
		{name: "defaults"},
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

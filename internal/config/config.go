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
	HTTPAddress       string
	DatabaseURL       string
	AWSRegion         string
	SQSEndpoint       string
	SQSQueueName      string
	LogLevel          slog.Level
	DependencyTimeout time.Duration
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
		HTTPAddress:  get("HTTP_ADDR", ":8080"),
		DatabaseURL:  get("DATABASE_URL", ""),
		AWSRegion:    get("AWS_REGION", "us-east-1"),
		SQSEndpoint:  get("SQS_ENDPOINT", ""),
		SQSQueueName: get("SQS_QUEUE_NAME", "wager-transactions.fifo"),
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
	return c, nil
}

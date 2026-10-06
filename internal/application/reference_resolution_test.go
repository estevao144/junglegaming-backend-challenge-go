package application

import (
	"context"
	"jungle-gaming/internal/platform/postgres"
	"testing"
	"time"
)

func TestReferenceBackoffAndPolicy(t *testing.T) {
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {4, 5 * time.Second}, {1000, 5 * time.Second},
	} {
		if got := ReferenceBackoff(test.attempt, time.Second, 5*time.Second); got != test.want {
			t.Fatalf("attempt %d: %s", test.attempt, got)
		}
	}
	if got := ReferenceBackoff(1000, time.Duration(1<<61), time.Duration(1<<63-1)); got != time.Duration(1<<63-1) {
		t.Fatal("backoff overflow")
	}
	s := &FinancialService{}
	for _, policy := range []ReferenceRetryPolicy{{}, {MaxAttempts: 1, Base: time.Second, Maximum: time.Millisecond}} {
		if err := s.ResolveReference(context.Background(), postgres.ReferenceClaim{}, policy); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}

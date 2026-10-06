package application

import (
	"context"
	"errors"
	"testing"

	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
)

func TestTerminalFailureClassification(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{ErrUnsupportedOperation, "UNSUPPORTED_OPERATION"}, {postgres.ErrConflict, "IDEMPOTENCY_CONFLICT"},
		{postgres.ErrNotFound, "WALLET_NOT_FOUND"}, {ErrWalletIdentity, "WALLET_IDENTITY"},
		{domain.ErrInvalidCurrency, "INVALID_MONEY"}, {domain.ErrInvalidTransaction, "INVALID_INPUT"}, {domain.ErrOverflow, "ARITHMETIC_OVERFLOW"},
		{context.DeadlineExceeded, ""}, {context.Canceled, ""}, {errors.New("database unavailable"), ""}, {postgres.ErrInboxIdentity, ""},
	} {
		if got := TerminalFailure(test.err); got != test.code {
			t.Fatalf("%v classified %q", test.err, got)
		}
	}
}

package domain

import "errors"

var (
	ErrInvalidMoney        = errors.New("invalid money")
	ErrInvalidCurrency     = errors.New("unsupported currency")
	ErrCurrencyMismatch    = errors.New("currencies do not match")
	ErrOverflow            = errors.New("integer overflow")
	ErrInvalidWallet       = errors.New("invalid wallet")
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrInvalidTransaction  = errors.New("invalid wager transaction")
	ErrInvalidTransition   = errors.New("invalid transaction transition")
	ErrInvalidLedgerEntry  = errors.New("invalid ledger entry")
	ErrUnresolvedReference = errors.New("reference must be resolved before determining movement")
)

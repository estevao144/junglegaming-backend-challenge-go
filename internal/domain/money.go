package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Money is immutable. The zero value is invalid; use ZeroMoney for a valid zero.
type Money struct {
	minorUnits int64
	currency   string
}

// NewMoney parses nonnegative BRL amounts with exactly two decimal places (e.g. "25.00").
func NewMoney(amount, currency string) (Money, error) {
	if err := validateCurrency(currency); err != nil {
		return Money{}, err
	}
	parts := strings.Split(amount, ".")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) != 2 || (len(parts[0]) > 1 && parts[0][0] == '0') {
		return Money{}, fmt.Errorf("%w: expected nonnegative decimal with exactly two places", ErrInvalidMoney)
	}
	digits := parts[0] + parts[1]
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return Money{}, fmt.Errorf("%w: expected decimal digits", ErrInvalidMoney)
		}
	}
	units, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("%w: amount exceeds int64 minor units", ErrOverflow)
	}
	return MoneyFromMinorUnits(units, currency)
}

// MoneyFromMinorUnits also supports signed values for internal calculations and rehydration.
// Transport adapters must use NewMoney to validate external financial input.
func MoneyFromMinorUnits(units int64, currency string) (Money, error) {
	if err := validateCurrency(currency); err != nil {
		return Money{}, err
	}
	return Money{minorUnits: units, currency: currency}, nil
}

func ZeroMoney(currency string) (Money, error) { return MoneyFromMinorUnits(0, currency) }
func (m Money) MinorUnits() int64              { return m.minorUnits }
func (m Money) Currency() string               { return m.currency }
func (m Money) Validate() error                { return validateCurrency(m.currency) }

func validateCurrency(currency string) error {
	if currency != "BRL" {
		return fmt.Errorf("%w: only BRL is supported", ErrInvalidCurrency)
	}
	return nil
}

func (m Money) compatible(other Money) error {
	// Diagnose different declared currencies before validating operational support.
	// An uninitialized value (empty currency) still fails normal validation below.
	if m.currency != "" && other.currency != "" && m.currency != other.currency {
		return ErrCurrencyMismatch
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if err := other.Validate(); err != nil {
		return err
	}
	return nil
}

func (m Money) Add(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	if (other.minorUnits > 0 && m.minorUnits > math.MaxInt64-other.minorUnits) ||
		(other.minorUnits < 0 && m.minorUnits < math.MinInt64-other.minorUnits) {
		return Money{}, ErrOverflow
	}
	return MoneyFromMinorUnits(m.minorUnits+other.minorUnits, m.currency)
}

func (m Money) Subtract(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	if (other.minorUnits > 0 && m.minorUnits < math.MinInt64+other.minorUnits) ||
		(other.minorUnits < 0 && m.minorUnits > math.MaxInt64+other.minorUnits) {
		return Money{}, ErrOverflow
	}
	return MoneyFromMinorUnits(m.minorUnits-other.minorUnits, m.currency)
}

func (m Money) Negate() (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	if m.minorUnits == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return MoneyFromMinorUnits(-m.minorUnits, m.currency)
}

func (m Money) Compare(other Money) (int, error) {
	if err := m.compatible(other); err != nil {
		return 0, err
	}
	if m.minorUnits < other.minorUnits {
		return -1, nil
	}
	if m.minorUnits > other.minorUnits {
		return 1, nil
	}
	return 0, nil
}

func (m Money) Decimal() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	whole, fraction := m.minorUnits/100, m.minorUnits%100
	sign := ""
	if m.minorUnits < 0 {
		sign, whole, fraction = "-", -whole, -fraction
	}
	return fmt.Sprintf("%s%d.%02d", sign, whole, fraction), nil
}

func (m Money) MarshalJSON() ([]byte, error) {
	amount, err := m.Decimal()
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}{Amount: amount, Currency: m.currency})
}

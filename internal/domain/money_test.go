package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func mustMoney(t *testing.T, amount, currency string) Money {
	t.Helper()
	m, err := NewMoney(amount, currency)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustUnits(t *testing.T, units int64) Money {
	t.Helper()
	m, err := MoneyFromMinorUnits(units, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// foreignMoneyForTest simulates incompatible data without enabling another currency
// in any public constructor. Tests share package domain to exercise this boundary.
func foreignMoneyForTest(units int64) Money {
	return Money{minorUnits: units, currency: "USD"}
}

func TestMoneyParsing(t *testing.T) {
	for _, test := range []struct {
		amount string
		units  int64
	}{
		{"0.00", 0}, {"0.01", 1}, {"25.00", 2500}, {"123.45", 12345}, {"92233720368547758.07", math.MaxInt64},
	} {
		t.Run(test.amount, func(t *testing.T) {
			m := mustMoney(t, test.amount, "BRL")
			if m.MinorUnits() != test.units || m.Currency() != "BRL" {
				t.Fatalf("unexpected money: %+v", m)
			}
			amount, err := m.Decimal()
			if err != nil || amount != test.amount {
				t.Fatalf("decimal = %q, %v", amount, err)
			}
		})
	}
}

func TestMoneyRejectsInvalidInput(t *testing.T) {
	for _, amount := range []string{"", "NaN", "Infinity", "-Infinity", "1e2", "1E2", "-1.00", "-0.00", "+1.00", "1", "1.0", "1.000", ".00", "1.", "00.00", "01.00", " 1.00", "1.00 ", "1,00", "a.00", "1.a0", "1.00.00", "١.00"} {
		t.Run(amount, func(t *testing.T) {
			if _, err := NewMoney(amount, "BRL"); !errors.Is(err, ErrInvalidMoney) {
				t.Fatalf("expected invalid money, got %v", err)
			}
		})
	}
	for _, amount := range []string{"92233720368547758.08", "999999999999999999999999999.00"} {
		if _, err := NewMoney(amount, "BRL"); !errors.Is(err, ErrOverflow) {
			t.Fatalf("expected overflow for %s, got %v", amount, err)
		}
	}
	for _, currency := range []string{"", "brl", " BRL", "BRL ", "ABC", "JPY", "USD", "BR"} {
		if _, err := NewMoney("1.00", currency); !errors.Is(err, ErrInvalidCurrency) {
			t.Fatalf("accepted currency %q: %v", currency, err)
		}
		if _, err := ZeroMoney(currency); !errors.Is(err, ErrInvalidCurrency) {
			t.Fatalf("accepted zero currency %q", currency)
		}
		if _, err := MoneyFromMinorUnits(1, currency); !errors.Is(err, ErrInvalidCurrency) {
			t.Fatalf("accepted minor units currency %q", currency)
		}
	}
}

func TestMoneyArithmetic(t *testing.T) {
	for _, test := range []struct {
		name     string
		a, b     int64
		subtract bool
		want     int64
		wantErr  error
	}{
		{"add", 2500, 125, false, 2625, nil},
		{"add negative", 100, -200, false, -100, nil},
		{"add positive overflow", math.MaxInt64, 1, false, 0, ErrOverflow},
		{"add negative overflow", math.MinInt64, -1, false, 0, ErrOverflow},
		{"add boundaries", math.MaxInt64, math.MinInt64, false, -1, nil},
		{"subtract", 2500, 125, true, 2375, nil},
		{"negative difference", 100, 200, true, -100, nil},
		{"subtract negative", -100, -200, true, 100, nil},
		{"subtract positive overflow", math.MaxInt64, -1, true, 0, ErrOverflow},
		{"subtract negative overflow", math.MinInt64, 1, true, 0, ErrOverflow},
		{"subtract min from itself", math.MinInt64, math.MinInt64, true, 0, nil},
		{"subtract min from zero", 0, math.MinInt64, true, 0, ErrOverflow},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, b := mustUnits(t, test.a), mustUnits(t, test.b)
			var result Money
			var err error
			if test.subtract {
				result, err = a.Subtract(b)
			} else {
				result, err = a.Add(b)
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if err == nil && result.MinorUnits() != test.want {
				t.Fatalf("got %d, want %d", result.MinorUnits(), test.want)
			}
			if a.MinorUnits() != test.a || b.MinorUnits() != test.b {
				t.Fatal("arithmetic mutated its operands")
			}
		})
	}
}

func TestMoneyComparisonNegationAndSerialization(t *testing.T) {
	for _, test := range []struct {
		units   int64
		decimal string
	}{
		{0, "0.00"}, {-1, "-0.01"}, {-100, "-1.00"}, {math.MaxInt64, "92233720368547758.07"}, {math.MinInt64, "-92233720368547758.08"},
	} {
		m := mustUnits(t, test.units)
		amount, err := m.Decimal()
		if err != nil || amount != test.decimal {
			t.Fatalf("decimal = %s, %v", amount, err)
		}
		encoded, err := json.Marshal(m)
		if err != nil || string(encoded) != `{"amount":"`+test.decimal+`","currency":"BRL"}` {
			t.Fatalf("json = %s, %v", encoded, err)
		}
		negative, err := m.Negate()
		if test.units == math.MinInt64 {
			if !errors.Is(err, ErrOverflow) {
				t.Fatal("negating min must overflow")
			}
		} else if err != nil || negative.MinorUnits() != -test.units {
			t.Fatalf("negation: %+v, %v", negative, err)
		}
	}
	for _, test := range []struct {
		a, b int64
		want int
	}{{1, 2, -1}, {2, 1, 1}, {2, 2, 0}, {math.MinInt64, math.MaxInt64, -1}} {
		got, err := mustUnits(t, test.a).Compare(mustUnits(t, test.b))
		if err != nil || got != test.want {
			t.Fatalf("compare = %d, %v", got, err)
		}
	}
	zero, err := ZeroMoney("BRL")
	if err != nil || zero.MinorUnits() != 0 || zero.Currency() != "BRL" {
		t.Fatalf("zero = %+v, %v", zero, err)
	}
}

func TestMoneyRejectsUninitializedAndMixedCurrency(t *testing.T) {
	a := mustMoney(t, "1.00", "BRL")
	for _, test := range []struct {
		name  string
		other Money
		want  error
	}{
		{"currency", foreignMoneyForTest(100), ErrCurrencyMismatch},
		{"zero value", Money{}, ErrInvalidCurrency},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := a.Add(test.other); !errors.Is(err, test.want) {
				t.Fatal(err)
			}
			if _, err := a.Subtract(test.other); !errors.Is(err, test.want) {
				t.Fatal(err)
			}
			if _, err := a.Compare(test.other); !errors.Is(err, test.want) {
				t.Fatal(err)
			}
			if _, err := test.other.Add(a); !errors.Is(err, test.want) {
				t.Fatal(err)
			}
			if _, err := test.other.Subtract(a); !errors.Is(err, test.want) {
				t.Fatal(err)
			}
			if _, err := test.other.Compare(a); !errors.Is(err, test.want) {
				t.Fatal(err)
			}
		})
	}
	var invalid Money
	if _, err := invalid.Negate(); err == nil {
		t.Fatal("uninitialized negate accepted")
	}
	if _, err := invalid.Decimal(); err == nil {
		t.Fatal("uninitialized decimal accepted")
	}
	if _, err := json.Marshal(invalid); err == nil {
		t.Fatal("uninitialized serialization accepted")
	}
}

func TestMoneyCannotOperateOrSerializeUnsupportedCurrency(t *testing.T) {
	m := foreignMoneyForTest(100)
	if err := m.Validate(); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
	if _, err := m.Add(m); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
	if _, err := m.Subtract(m); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
	if _, err := m.Compare(m); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
	if _, err := m.Negate(); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
	if _, err := m.Decimal(); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
	if _, err := json.Marshal(m); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
}

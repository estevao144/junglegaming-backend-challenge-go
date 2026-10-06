package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFinancialEventContracts(t *testing.T) {
	tx := mustTransaction(t, Bet)
	if _, err := NewWagerTransactionProcessedEvent("event", "correlation", "", tx); err == nil {
		t.Fatal("pending event accepted")
	}
	if err := tx.MarkProcessed(financialResult(t), "", testTime()); err != nil {
		t.Fatal(err)
	}
	event, err := NewWagerTransactionProcessedEvent("event-1", "correlation-1", "message-1", tx)
	if err != nil {
		t.Fatal(err)
	}
	if event.EventType != "WagerTransactionProcessed" || event.Version != 1 || event.AggregateID != tx.Snapshot().Data.ID {
		t.Fatal("wrong event header")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"amount":"25.00"`) || !strings.Contains(string(payload), `"causationId":"message-1"`) {
		t.Fatalf("wrong payload: %s", payload)
	}
	if _, err := NewWagerTransactionProcessedEvent("", "correlation", "", tx); err == nil {
		t.Fatal("missing event identity accepted")
	}
	entry, err := NewWalletLedgerEntry(LedgerEntryState{ID: "ledger", WalletID: "wallet-1", TransactionID: tx.Snapshot().Data.ID, Direction: DebitDirection,
		Money: mustMoney(t, "25.00", "BRL"), BalanceBefore: mustMoney(t, "100.00", "BRL"), BalanceAfter: mustMoney(t, "75.00", "BRL"), CreatedAt: testTime()})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := NewWalletBalanceChangedEvent("event-2", "correlation", "", entry, 2)
	if err != nil {
		t.Fatal(err)
	}
	if changed.EventType != "WalletBalanceChanged" || changed.Data.WalletVersion != 2 || changed.Data.Direction != DebitDirection {
		t.Fatal("wrong balance event")
	}
	if _, err := NewWalletBalanceChangedEvent("event", "correlation", "", entry, 0); err == nil {
		t.Fatal("invalid version accepted")
	}
	rejected := mustTransaction(t, Bet)
	result := financialResult(t)
	if err := rejected.Reject(FailureInsufficientBalance, &result, testTime()); err != nil {
		t.Fatal(err)
	}
	rejection, err := NewWagerTransactionRejectedEvent("event-3", "correlation", "", rejected)
	if err != nil {
		t.Fatal(err)
	}
	if rejection.EventType != "WagerTransactionRejected" || rejection.Data.FailureCode != FailureInsufficientBalance {
		t.Fatal("wrong rejection event")
	}
	opening, err := NewOpeningTransaction("opening", "wallet", "player", mustMoney(t, "100.00", "BRL"), testTime())
	if err != nil {
		t.Fatal(err)
	}
	if err := opening.MarkProcessed(FinancialResult{Balance: mustMoney(t, "100.00", "BRL"), WalletVersion: 1}, "", testTime()); err != nil {
		t.Fatal(err)
	}
	internal, err := NewWagerTransactionProcessedEvent("event-4", "correlation", "", opening)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(internal)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "providerId") || strings.Contains(string(encoded), "externalTransactionId") {
		t.Fatal("internal event contains external metadata")
	}
}

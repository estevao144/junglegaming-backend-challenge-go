package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRefundReferenceRules(t *testing.T) {
	refund := mustTransaction(t, Refund)
	original := transactionData(t, Bet)
	original.ID = "original-internal"
	original.ExternalTransactionID = "external-original"
	result := financialResult(t)
	valid := WagerTransactionState{Data: original, Status: Processed, Result: &result, CreatedAt: testTime(), UpdatedAt: testTime()}
	for _, test := range []struct {
		name   string
		change func(*WagerTransactionState)
		want   FailureCode
	}{
		{"valid", func(*WagerTransactionState) {}, ""},
		{"amount", func(s *WagerTransactionState) { s.Data.Money = mustMoney(t, "24.99", "BRL") }, FailureInvalidReference},
		{"currency", func(s *WagerTransactionState) { s.Data.Money = Money{minorUnits: 2500, currency: "USD"} }, FailureInvalidReference},
		{"provider", func(s *WagerTransactionState) { s.Data.ProviderID = "other" }, FailureInvalidReference},
		{"player", func(s *WagerTransactionState) { s.Data.PlayerID = "other" }, FailureInvalidReference},
		{"wallet", func(s *WagerTransactionState) { s.Data.WalletID = "other" }, FailureInvalidReference},
		{"round", func(s *WagerTransactionState) { s.Data.RoundID = "other" }, FailureInvalidReference},
		{"external identity", func(s *WagerTransactionState) { s.Data.ExternalTransactionID = "other" }, FailureInvalidReference},
		{"WIN", func(s *WagerTransactionState) { s.Data.Kind = Win }, FailureInvalidReference},
		{"LOSS", func(s *WagerTransactionState) { s.Data.Kind = Loss }, FailureInvalidReference},
		{"REFUND", func(s *WagerTransactionState) { s.Data.Kind = Refund }, FailureInvalidReference},
		{"not processed", func(s *WagerTransactionState) { s.Status = Rejected; s.FailureCode = FailureInsufficientBalance }, FailureReferenceNotProcessed},
	} {
		t.Run(test.name, func(t *testing.T) {
			reference := valid
			test.change(&reference)
			if got := refund.RefundReferenceFailure(reference); got != test.want {
				t.Fatalf("got %s, want %s", got, test.want)
			}
		})
	}
}

func TestPendingReferenceEventAndFutureTransition(t *testing.T) {
	refund := mustTransaction(t, Refund)
	if _, err := NewWagerTransactionPendingReferenceEvent("event", "correlation", "cause", refund); err == nil {
		t.Fatal("nonpending event accepted")
	}
	if err := refund.MarkPendingReference(testTime()); err != nil {
		t.Fatal(err)
	}
	event, err := NewWagerTransactionPendingReferenceEvent("event", "correlation", "cause", refund)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"amount":"25.00"`) || event.Data.ReferenceExternalTransactionID != "external-original" || event.EventType != "WagerTransactionPendingReference" || event.Data.WalletID != "wallet-1" {
		t.Fatal("pending snapshot contract changed")
	}
	if refund.Snapshot().Result != nil {
		t.Fatal("pending has financial result")
	}
	if err := refund.MarkProcessed(financialResult(t), "original-internal", testTime()); err != nil {
		t.Fatal(err)
	}
	if event.Data.Kind != Refund || event.Data.ReferenceExternalTransactionID != "external-original" {
		t.Fatal("snapshot mutated")
	}
	if err := refund.RejectReference(FailureReversalConflict, financialResult(t), "original-internal", testTime()); err == nil {
		t.Fatal("terminal transaction changed")
	}
}

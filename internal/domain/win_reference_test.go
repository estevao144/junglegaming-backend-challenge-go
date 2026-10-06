package domain

import "testing"

func TestWinReferenceContextAndIndependentPayout(t *testing.T) {
	data := transactionData(t, Win)
	data.ReferenceExternalTransactionID = "external-original"
	data.Money = mustMoney(t, "10.00", "BRL")
	win, err := NewWagerTransaction(data, testTime())
	if err != nil {
		t.Fatal(err)
	}
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
		{"different payout allowed", func(*WagerTransactionState) {}, ""},
		{"provider", func(s *WagerTransactionState) { s.Data.ProviderID = "other" }, FailureInvalidReference},
		{"wallet", func(s *WagerTransactionState) { s.Data.WalletID = "other" }, FailureInvalidReference},
		{"player", func(s *WagerTransactionState) { s.Data.PlayerID = "other" }, FailureInvalidReference},
		{"round", func(s *WagerTransactionState) { s.Data.RoundID = "other" }, FailureInvalidReference},
		{"kind", func(s *WagerTransactionState) { s.Data.Kind = Win }, FailureInvalidReference},
		{"identity", func(s *WagerTransactionState) { s.Data.ExternalTransactionID = "other" }, FailureInvalidReference},
		{"pending", func(s *WagerTransactionState) { s.Status = Pending; s.Result = nil }, FailureReferenceNotProcessed},
	} {
		t.Run(test.name, func(t *testing.T) {
			reference := valid
			test.change(&reference)
			if got := win.WinReferenceFailure(reference); got != test.want {
				t.Fatal(got, test.want)
			}
		})
	}
}

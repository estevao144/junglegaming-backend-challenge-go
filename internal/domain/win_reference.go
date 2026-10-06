package domain

// A WIN may reference a processed BET in the same provider/wallet/player/round.
// Its payout is independent of the original bet amount.
func (t WagerTransaction) WinReferenceFailure(reference WagerTransactionState) FailureCode {
	if t.state.Data.Kind != Win || validateTransactionState(reference) != nil {
		return FailureInvalidReference
	}
	data, original := t.state.Data, reference.Data
	if original.Kind != Bet || original.ID == data.ID || original.ExternalTransactionID != data.ReferenceExternalTransactionID ||
		original.ProviderID != data.ProviderID || original.WalletID != data.WalletID || original.PlayerID != data.PlayerID ||
		original.RoundID != data.RoundID || original.Money.Currency() != data.Money.Currency() {
		return FailureInvalidReference
	}
	if reference.Status != Processed {
		return FailureReferenceNotProcessed
	}
	return ""
}

package domain

// RefundReferenceFailure checks reference context without moving money.
// Duplicate reversals are coordinated separately by PostgreSQL.
func (t WagerTransaction) RefundReferenceFailure(reference WagerTransactionState) FailureCode {
	if err := validateTransactionState(reference); err != nil {
		return FailureInvalidReference
	}
	data := t.state.Data
	original := reference.Data
	if data.Kind != Refund || !validID(original.ID) || original.ID == data.ID || original.Kind != Bet {
		return FailureInvalidReference
	}
	if original.ExternalTransactionID != data.ReferenceExternalTransactionID || original.ProviderID != data.ProviderID ||
		original.PlayerID != data.PlayerID || original.WalletID != data.WalletID || original.RoundID != data.RoundID ||
		original.Money.Currency() != data.Money.Currency() {
		return FailureInvalidReference
	}
	comparison, err := data.Money.Compare(original.Money)
	if err != nil || comparison != 0 {
		return FailureInvalidReference
	}
	if reference.Status != Processed {
		return FailureReferenceNotProcessed
	}
	return ""
}

package domain

import "fmt"

// RefundReferenceFailure checks reference context without moving money.
// Duplicate reversals are coordinated separately by PostgreSQL.
func (t WagerTransaction) RefundReferenceFailure(reference WagerTransactionState) FailureCode {
	if t.state.Data.Kind != Refund {
		return FailureInvalidReference
	}
	return t.ReversalReferenceFailure(reference)
}

// ReversalReferenceFailure validates both reversal kinds without financial effects.
func (t WagerTransaction) ReversalReferenceFailure(reference WagerTransactionState) FailureCode {
	if err := validateTransactionState(reference); err != nil {
		return FailureInvalidReference
	}
	data := t.state.Data
	original := reference.Data
	if !validID(original.ID) || original.ID == data.ID {
		return FailureInvalidReference
	}
	switch data.Kind {
	case Refund:
		if original.Kind != Bet {
			return FailureInvalidReference
		}
	case Rollback:
		if original.Kind != Bet && original.Kind != Win && original.Kind != Refund {
			return FailureInvalidReference
		}
	default:
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

// ReversalMovement derives direction only after validating the full reference.
func (t WagerTransaction) ReversalMovement(reference WagerTransactionState) (Direction, error) {
	if failure := t.ReversalReferenceFailure(reference); failure != "" {
		return "", fmt.Errorf("%w: %s", ErrUnresolvedReference, failure)
	}
	if t.state.Data.Kind == Refund || reference.Data.Kind == Bet {
		return CreditDirection, nil
	}
	return DebitDirection, nil
}

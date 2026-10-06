package domain

type WagerTransactionPendingReferenceData struct {
	TransactionID                  string          `json:"transactionId"`
	WalletID                       string          `json:"walletId"`
	PlayerID                       string          `json:"playerId"`
	ProviderID                     string          `json:"providerId"`
	ExternalTransactionID          string          `json:"externalTransactionId"`
	Kind                           TransactionKind `json:"kind"`
	RoundID                        string          `json:"roundId"`
	GameID                         string          `json:"gameId"`
	Money                          Money           `json:"money"`
	ReferenceExternalTransactionID string          `json:"referenceExternalTransactionId"`
}
type WagerTransactionPendingReferenceEvent struct {
	EventHeader
	Data WagerTransactionPendingReferenceData `json:"data"`
}

func NewWagerTransactionPendingReferenceEvent(id, correlation, causation string, transaction *WagerTransaction) (WagerTransactionPendingReferenceEvent, error) {
	if transaction == nil {
		return WagerTransactionPendingReferenceEvent{}, ErrInvalidTransaction
	}
	state := transaction.Snapshot()
	if err := validateTransactionState(state); err != nil {
		return WagerTransactionPendingReferenceEvent{}, err
	}
	if state.Status != PendingReference {
		return WagerTransactionPendingReferenceEvent{}, ErrInvalidTransition
	}
	header, err := eventHeader(id, "WagerTransactionPendingReference", state.Data.ID, correlation, causation, state.UpdatedAt)
	if err != nil {
		return WagerTransactionPendingReferenceEvent{}, err
	}
	data := state.Data
	return WagerTransactionPendingReferenceEvent{EventHeader: header, Data: WagerTransactionPendingReferenceData{
		TransactionID: data.ID, WalletID: data.WalletID, PlayerID: data.PlayerID, ProviderID: data.ProviderID, ExternalTransactionID: data.ExternalTransactionID,
		Kind: data.Kind, RoundID: data.RoundID, GameID: data.GameID, Money: data.Money, ReferenceExternalTransactionID: data.ReferenceExternalTransactionID}}, nil
}

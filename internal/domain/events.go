package domain

import (
	"fmt"
	"time"
)

type EventHeader struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
}

type WagerTransactionProcessedData struct {
	TransactionID         string          `json:"transactionId"`
	WalletID              string          `json:"walletId"`
	PlayerID              string          `json:"playerId"`
	ProviderID            string          `json:"providerId,omitempty"`
	ExternalTransactionID string          `json:"externalTransactionId,omitempty"`
	Kind                  TransactionKind `json:"kind"`
	Money                 Money           `json:"money"`
	Balance               Money           `json:"balance"`
	WalletVersion         int64           `json:"walletVersion"`
}

type WagerTransactionProcessedEvent struct {
	EventHeader
	Data WagerTransactionProcessedData `json:"data"`
}

type WagerTransactionRejectedData struct {
	TransactionID         string      `json:"transactionId"`
	WalletID              string      `json:"walletId"`
	ProviderID            string      `json:"providerId"`
	ExternalTransactionID string      `json:"externalTransactionId"`
	FailureCode           FailureCode `json:"failureCode"`
	Balance               Money       `json:"balance"`
	WalletVersion         int64       `json:"walletVersion"`
}

type WagerTransactionRejectedEvent struct {
	EventHeader
	Data WagerTransactionRejectedData `json:"data"`
}

type WalletBalanceChangedData struct {
	WalletID      string    `json:"walletId"`
	TransactionID string    `json:"transactionId"`
	Direction     Direction `json:"direction"`
	Money         Money     `json:"money"`
	BalanceBefore Money     `json:"balanceBefore"`
	BalanceAfter  Money     `json:"balanceAfter"`
	WalletVersion int64     `json:"walletVersion"`
}

type WalletBalanceChangedEvent struct {
	EventHeader
	Data WalletBalanceChangedData `json:"data"`
}

func eventHeader(id, kind, aggregate, correlation, causation string, at time.Time) (EventHeader, error) {
	if !validID(id) || !validID(aggregate) || !validID(correlation) || at.IsZero() || (causation != "" && !validID(causation)) {
		return EventHeader{}, fmt.Errorf("invalid event metadata")
	}
	return EventHeader{EventID: id, EventType: kind, AggregateID: aggregate, CorrelationID: correlation, CausationID: causation, OccurredAt: at.UTC(), Version: 1}, nil
}

func NewWagerTransactionProcessedEvent(id, correlation, causation string, tx *WagerTransaction) (WagerTransactionProcessedEvent, error) {
	if tx == nil {
		return WagerTransactionProcessedEvent{}, ErrInvalidTransaction
	}
	s := tx.Snapshot()
	if err := validateTransactionState(s); err != nil {
		return WagerTransactionProcessedEvent{}, err
	}
	if s.Status != Processed {
		return WagerTransactionProcessedEvent{}, ErrInvalidTransition
	}
	h, err := eventHeader(id, "WagerTransactionProcessed", s.Data.ID, correlation, causation, s.UpdatedAt)
	if err != nil {
		return WagerTransactionProcessedEvent{}, err
	}
	return WagerTransactionProcessedEvent{EventHeader: h, Data: WagerTransactionProcessedData{
		TransactionID: s.Data.ID, WalletID: s.Data.WalletID, PlayerID: s.Data.PlayerID, ProviderID: s.Data.ProviderID,
		ExternalTransactionID: s.Data.ExternalTransactionID, Kind: s.Data.Kind, Money: s.Data.Money, Balance: s.Result.Balance, WalletVersion: s.Result.WalletVersion}}, nil
}

func NewWagerTransactionRejectedEvent(id, correlation, causation string, tx *WagerTransaction) (WagerTransactionRejectedEvent, error) {
	if tx == nil {
		return WagerTransactionRejectedEvent{}, ErrInvalidTransaction
	}
	s := tx.Snapshot()
	if err := validateTransactionState(s); err != nil {
		return WagerTransactionRejectedEvent{}, err
	}
	if s.Status != Rejected || s.Result == nil {
		return WagerTransactionRejectedEvent{}, ErrInvalidTransition
	}
	h, err := eventHeader(id, "WagerTransactionRejected", s.Data.ID, correlation, causation, s.UpdatedAt)
	if err != nil {
		return WagerTransactionRejectedEvent{}, err
	}
	return WagerTransactionRejectedEvent{EventHeader: h, Data: WagerTransactionRejectedData{TransactionID: s.Data.ID, WalletID: s.Data.WalletID, ProviderID: s.Data.ProviderID,
		ExternalTransactionID: s.Data.ExternalTransactionID, FailureCode: s.FailureCode, Balance: s.Result.Balance, WalletVersion: s.Result.WalletVersion}}, nil
}

func NewWalletBalanceChangedEvent(id, correlation, causation string, entry WalletLedgerEntry, version int64) (WalletBalanceChangedEvent, error) {
	s := entry.Snapshot()
	if _, err := RehydrateWalletLedgerEntry(s); err != nil {
		return WalletBalanceChangedEvent{}, err
	}
	if version < 1 {
		return WalletBalanceChangedEvent{}, ErrInvalidWallet
	}
	h, err := eventHeader(id, "WalletBalanceChanged", s.WalletID, correlation, causation, s.CreatedAt)
	if err != nil {
		return WalletBalanceChangedEvent{}, err
	}
	return WalletBalanceChangedEvent{EventHeader: h, Data: WalletBalanceChangedData{WalletID: s.WalletID, TransactionID: s.TransactionID,
		Direction: s.Direction, Money: s.Money, BalanceBefore: s.BalanceBefore, BalanceAfter: s.BalanceAfter, WalletVersion: version}}, nil
}

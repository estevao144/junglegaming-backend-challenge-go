// Package sqstransport maps incoming JSON to the shared financial use case.
package sqstransport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"jungle-gaming/internal/application"
	"jungle-gaming/internal/domain"
)

var ErrPoisonMessage = errors.New("malformed or incomplete operation envelope")

type OperationEnvelope struct {
	MessageID     string        `json:"messageId"`
	Type          string        `json:"type"`
	OccurredAt    time.Time     `json:"occurredAt"`
	CorrelationID string        `json:"correlationId,omitempty"`
	Data          OperationData `json:"data"`
}

type OperationData struct {
	ProviderID            string                 `json:"providerId"`
	ExternalTransactionID string                 `json:"externalTransactionId"`
	IdempotencyKey        string                 `json:"idempotencyKey"`
	PlayerID              string                 `json:"playerId"`
	WalletID              string                 `json:"walletId"`
	RoundID               string                 `json:"roundId"`
	GameID                string                 `json:"gameId"`
	Kind                  domain.TransactionKind `json:"kind"`
	Money                 struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"money"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
}

func ParseOperation(body, consumer, source string) (application.IncomingOperation, error) {
	var envelope OperationEnvelope
	decoder := json.NewDecoder(bytes.NewBufferString(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return application.IncomingOperation{}, ErrPoisonMessage
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return application.IncomingOperation{}, ErrPoisonMessage
	}
	d := envelope.Data
	for _, value := range []string{envelope.MessageID, envelope.Type, d.ProviderID, d.ExternalTransactionID, d.IdempotencyKey, d.PlayerID, d.WalletID, d.RoundID, d.GameID, string(d.Kind), d.Money.Amount, d.Money.Currency} {
		if strings.TrimSpace(value) == "" {
			return application.IncomingOperation{}, ErrPoisonMessage
		}
	}
	if envelope.OccurredAt.IsZero() || strings.TrimSpace(envelope.MessageID) != envelope.MessageID {
		return application.IncomingOperation{}, ErrPoisonMessage
	}
	correlation := envelope.CorrelationID
	if correlation == "" {
		correlation = envelope.MessageID
	}
	hash := sha256.Sum256([]byte(body))
	incoming := application.IncomingOperation{ConsumerName: consumer, Source: source, MessageID: envelope.MessageID, PayloadHash: hex.EncodeToString(hash[:]), CorrelationID: correlation}
	if envelope.Type != "WagerTransactionRequested" || strings.TrimSpace(correlation) != correlation {
		incoming.RejectionCode = "INVALID_INPUT"
		incoming.CorrelationID = envelope.MessageID
		correlation = envelope.MessageID
	}
	money, err := domain.NewMoney(d.Money.Amount, d.Money.Currency)
	if err != nil {
		incoming.RejectionCode = "INVALID_MONEY"
	}
	incoming.Command = application.ProcessCommand{ProviderID: d.ProviderID, ExternalTransactionID: d.ExternalTransactionID, IdempotencyKey: d.IdempotencyKey,
		PlayerID: d.PlayerID, WalletID: d.WalletID, RoundID: d.RoundID, GameID: d.GameID, Kind: d.Kind, Money: money, ReferenceExternalTransactionID: d.ReferenceExternalTransactionID,
		CorrelationID: correlation, CausationID: envelope.MessageID}
	return incoming, nil
}

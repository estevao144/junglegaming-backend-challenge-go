package sqstransport

import (
	"errors"
	"strings"
	"testing"

	"jungle-gaming/internal/application"
	"jungle-gaming/internal/domain"
)

const validOperation = `{"messageId":"envelope-1","type":"WagerTransactionRequested","occurredAt":"2026-10-06T12:00:00Z","data":{"providerId":"provider","externalTransactionId":"external","idempotencyKey":"original-custom-key","playerId":"player","walletId":"wallet","roundId":"round","gameId":"game","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`

func TestOperationMappingAndTransportHashEquivalence(t *testing.T) {
	incoming, err := ParseOperation(validOperation, "consumer", "source")
	if err != nil {
		t.Fatal(err)
	}
	if incoming.Command.IdempotencyKey != "original-custom-key" || incoming.Command.CausationID != "envelope-1" || incoming.Command.CorrelationID != "envelope-1" {
		t.Fatal("transport mapping changed identity")
	}
	money, err := domain.NewMoney("25.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	direct := application.ProcessCommand{ProviderID: "provider", ExternalTransactionID: "external", IdempotencyKey: "original-custom-key", PlayerID: "player", WalletID: "wallet", RoundID: "round", GameID: "game", Kind: domain.Bet, Money: money, CorrelationID: "http-correlation"}
	a, err := application.PayloadHash(direct)
	if err != nil {
		t.Fatal(err)
	}
	b, err := application.PayloadHash(incoming.Command)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("transport metadata affected financial hash")
	}
	other, err := ParseOperation(strings.ReplaceAll(validOperation, "envelope-1", "envelope-2"), "consumer", "source")
	if err != nil {
		t.Fatal(err)
	}
	if other.PayloadHash == incoming.PayloadHash {
		t.Fatal("inbox must hash the full envelope separately")
	}
}

func TestMalformedAndSemanticMessages(t *testing.T) {
	for _, body := range []string{`{`, `null`, `{}`, validOperation + `{}`, strings.Replace(validOperation, `"25.00"`, `25.00`, 1), strings.Replace(validOperation, `"wallet"`, `""`, 1)} {
		if _, err := ParseOperation(body, "consumer", "source"); !errors.Is(err, ErrPoisonMessage) {
			t.Fatalf("expected poison: %s", body)
		}
	}
	for _, amount := range []string{"25.0", "-1.00", "92233720368547758.08"} {
		incoming, err := ParseOperation(strings.Replace(validOperation, "25.00", amount, 1), "consumer", "source")
		if err != nil || incoming.RejectionCode != "INVALID_MONEY" {
			t.Fatal("semantic input should get a durable rejection")
		}
	}
}

func TestReversalReferenceMappingAndHash(t *testing.T) {
	for _, kind := range []domain.TransactionKind{domain.Refund, domain.Rollback} {
		t.Run(string(kind), func(t *testing.T) {
			body := strings.Replace(validOperation, `"kind":"BET"`, `"kind":"`+string(kind)+`","referenceExternalTransactionId":"original-bet"`, 1)
			incoming, err := ParseOperation(body, "consumer", "source")
			if err != nil {
				t.Fatal(err)
			}
			if incoming.Command.Kind != kind || incoming.Command.ReferenceExternalTransactionID != "original-bet" || incoming.Command.IdempotencyKey != "original-custom-key" {
				t.Fatal("reference input lost")
			}
			hash, err := application.PayloadHash(incoming.Command)
			if err != nil {
				t.Fatal(err)
			}
			changed := incoming.Command
			changed.ReferenceExternalTransactionID = "other-bet"
			other, err := application.PayloadHash(changed)
			if err != nil {
				t.Fatal(err)
			}
			if hash == other {
				t.Fatal("reference must participate in financial hash")
			}
		})
	}
}

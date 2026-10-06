package application

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"jungle-gaming/internal/domain"
)

func TestPayloadHashCanonicalBusinessFields(t *testing.T) {
	money, err := domain.NewMoney("25.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	c := ProcessCommand{ProviderID: "provider-a", ExternalTransactionID: "ext-1", IdempotencyKey: "key-1", WalletID: "wallet-1", PlayerID: "player-1", RoundID: "round-1", GameID: "game-1", Kind: domain.Bet, Money: money, CorrelationID: "transport-1"}
	hash, err := PayloadHash(c)
	if err != nil {
		t.Fatal(err)
	}
	canonical := `{"externalTransactionId":"ext-1","gameId":"game-1","kind":"BET","money":{"amount":"25.00","currency":"BRL"},"playerId":"player-1","providerId":"provider-a","referenceExternalTransactionId":"","roundId":"round-1","walletId":"wallet-1"}`
	want := sha256.Sum256([]byte(canonical))
	if hash != hex.EncodeToString(want[:]) {
		t.Fatalf("hash is not the specified canonical JSON: %s", hash)
	}
	c.IdempotencyKey = "other-key"
	c.CorrelationID = "http-or-sqs"
	c.CausationID = "msg-1"
	again, err := PayloadHash(c)
	if err != nil || again != hash {
		t.Fatal("transport metadata or key changed business hash")
	}
	for _, change := range []func(*ProcessCommand){
		func(c *ProcessCommand) { c.ExternalTransactionID = "ext-2" }, func(c *ProcessCommand) { c.ProviderID = "provider-b" },
		func(c *ProcessCommand) { c.WalletID = "wallet-2" }, func(c *ProcessCommand) { c.PlayerID = "player-2" },
		func(c *ProcessCommand) { c.RoundID = "round-2" }, func(c *ProcessCommand) { c.GameID = "game-2" },
		func(c *ProcessCommand) { c.Kind = domain.Win }, func(c *ProcessCommand) { c.ReferenceExternalTransactionID = "original" },
		func(c *ProcessCommand) { c.Money, _ = domain.NewMoney("25.01", "BRL") },
	} {
		changed := c
		change(&changed)
		other, err := PayloadHash(changed)
		if err != nil {
			t.Fatal(err)
		}
		if other == hash {
			t.Fatal("business field was excluded from hash")
		}
	}
	if _, err := PayloadHash(ProcessCommand{}); err == nil {
		t.Fatal("uninitialized money accepted")
	}
}

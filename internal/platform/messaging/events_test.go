package messaging

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"jungle-gaming/internal/platform/postgres"
)

func TestEventMessageSnapshotAndStableRouting(t *testing.T) {
	event := postgres.ClaimedEvent{OutboxRecord: postgres.OutboxRecord{EventID: "event-1",
		Payload: []byte(`{"eventId":"event-1","aggregateId":"transaction-1","correlationId":"corr","causationId":"cause","occurredAt":"2026-01-01T00:00:00Z","version":1,"data":{"walletId":"wallet-1","money":{"amount":"92233720368547758.07","currency":"BRL"}}}`)}, WalletID: "wallet-1"}
	first, err := BuildEventMessage("queue", event)
	if err != nil {
		t.Fatal(err)
	}
	event.ClaimToken = "new-owner"
	event.Attempts = 4
	second, err := BuildEventMessage("queue", event)
	if err != nil {
		t.Fatal(err)
	}
	group := sha256.Sum256([]byte("wallet-1"))
	dedup := sha256.Sum256([]byte("event-1"))
	if aws.ToString(first.MessageBody) != string(event.Payload) || aws.ToString(second.MessageBody) != string(event.Payload) {
		t.Fatal("persisted snapshot changed")
	}
	if aws.ToString(first.MessageGroupId) != hex.EncodeToString(group[:]) {
		t.Fatal("must group by wallet, not transaction aggregate")
	}
	if aws.ToString(first.MessageDeduplicationId) != hex.EncodeToString(dedup[:]) || aws.ToString(first.MessageDeduplicationId) != aws.ToString(second.MessageDeduplicationId) {
		t.Fatal("dedup identity must remain stable")
	}
	event.WalletID = "other"
	event.Payload = []byte(`{"eventId":"event-1","data":{"walletId":"other"}}`)
	other, err := BuildEventMessage("queue", event)
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(other.MessageGroupId) == aws.ToString(first.MessageGroupId) {
		t.Fatal("independent wallets share a global group")
	}
}

func TestEventMessageRejectsInvalidMetadata(t *testing.T) {
	for _, payload := range []string{`{`, `{}`, `{"eventId":"other","data":{"walletId":"wallet"}}`, `{"eventId":"event","data":{"walletId":"other"}}`} {
		event := postgres.ClaimedEvent{OutboxRecord: postgres.OutboxRecord{EventID: "event", Payload: []byte(payload)}, WalletID: "wallet"}
		if _, err := BuildEventMessage("queue", event); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}

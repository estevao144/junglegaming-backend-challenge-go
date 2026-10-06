//go:build integration

package application_test

import (
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/workers"
	"testing"
)

func TestExternalOpeningRejectedByAuthenticatedSQSConsumer(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	_, identity := testAuthenticator(t, authConfig(q.config))
	wallet := open(t, f, "external-opening-player", "0.00")
	external := command(t, wallet, domain.Opening, "100.00", "external-opening")
	external.ProviderID = "provider-alpha"
	consumer := workers.NewOperationConsumer(q.queue, application.NewAuthorizedIncomingService(f.service, identity), q.config, outboxLogger())
	sendOperation(t, f, q, operationBody(t, external, "opening-message"), "opening", "opening")
	message := receiveOperation(t, f, q)
	result, err := consumer.Resolve(f.ctx, message)
	if err != nil || result.Status != "REJECTED" || result.FailureCode != "UNSUPPORTED_OPERATION" {
		t.Fatal(result, err)
	}
	if err := consumer.Handle(f.ctx, message); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, wallet.Snapshot().ID, 0, 1)
	for _, table := range []string{"wager_transactions", "wallet_ledger_entries", "outbox_events"} {
		if count(t, f, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("external opening moved money", table)
		}
	}
	assertQueueDeleted(t, f, q)
}

//go:build integration

package application_test

import (
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
	"testing"
	"time"
)

func TestReferencedWinUsesDurableReferenceRecovery(t *testing.T) {
	f := newFixture(t)
	wallet := open(t, f, "referenced-win-player", "100.00")
	win := command(t, wallet, domain.Win, "10.00", "early-win")
	win.ReferenceExternalTransactionID = "late-bet"
	pending, err := f.service.Process(f.ctx, win)
	if err != nil || pending.Transaction.Status != domain.PendingReference {
		t.Fatal(pending, err)
	}
	assertBalance(t, f, wallet.Snapshot().ID, 10000, 1)
	bet := command(t, wallet, domain.Bet, "25.00", "late-bet")
	if _, err := f.service.Process(f.ctx, bet); err != nil {
		t.Fatal(err)
	}
	restarted := startInstance(t, f.url)
	claims, err := postgres.NewReferenceQueue(restarted.database).Claim(f.ctx, 10, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatal(claims, err)
	}
	if err := restarted.service.ResolveReference(f.ctx, claims[0], application.ReferenceRetryPolicy{MaxAttempts: 3, Base: time.Millisecond, Maximum: time.Second}); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, wallet.Snapshot().ID, 8500, 3)
	replay, err := restarted.service.Process(f.ctx, win)
	if err != nil || !replay.IdempotentReplay || replay.Transaction.ReferenceTransactionID == "" || replay.Transaction.Result.Balance.MinorUnits() != 8500 {
		t.Fatal(replay, err)
	}
	assertBalance(t, f, wallet.Snapshot().ID, 8500, 3)
}

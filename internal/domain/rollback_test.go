package domain

import "testing"

func TestRollbackReferenceAndDirection(t *testing.T) {
	for _, kind := range []TransactionKind{Bet, Win, Refund, Loss, Rollback, Opening} {
		t.Run(string(kind), func(t *testing.T) {
			rollback := mustTransaction(t, Rollback)
			original := transactionData(t, kind)
			original.ID = "original-internal"
			original.ExternalTransactionID = "external-original"
			result := financialResult(t)
			state := WagerTransactionState{Data: original, Status: Processed, Result: &result, CreatedAt: testTime(), UpdatedAt: testTime()}
			if kind == Refund || kind == Rollback {
				state.ReferenceTransactionID = "earlier-internal"
				state.Data.ReferenceExternalTransactionID = "earlier-external"
			}
			if kind == Loss {
				state.Data.Money = mustMoney(t, "0.00", "BRL")
			}
			allowed := kind == Bet || kind == Win || kind == Refund
			failure := rollback.ReversalReferenceFailure(state)
			direction, err := rollback.ReversalMovement(state)
			if !allowed {
				if failure != FailureInvalidReference || err == nil {
					t.Fatal("invalid rollback reference accepted")
				}
				return
			}
			want := DebitDirection
			if kind == Bet {
				want = CreditDirection
			}
			if failure != "" || err != nil || direction != want {
				t.Fatalf("direction=%s failure=%s err=%v", direction, failure, err)
			}
			state.Data.Money = mustMoney(t, "24.99", "BRL")
			if rollback.ReversalReferenceFailure(state) != FailureInvalidReference {
				t.Fatal("partial rollback accepted")
			}
			state.Data.Money = original.Money
			state.Status = Rejected
			state.FailureCode = FailureInvalidReference
			if rollback.ReversalReferenceFailure(state) != FailureReferenceNotProcessed {
				t.Fatal("nonprocessed rollback accepted")
			}
		})
	}
}

func TestRollbackPendingTransitions(t *testing.T) {
	for _, terminal := range []TransactionStatus{Processed, Rejected} {
		t.Run(string(terminal), func(t *testing.T) {
			tx := mustTransaction(t, Rollback)
			if err := tx.MarkPendingReference(testTime()); err != nil {
				t.Fatal(err)
			}
			var err error
			if terminal == Processed {
				err = tx.MarkProcessed(financialResult(t), "internal-original", testTime())
			} else {
				err = tx.Reject(FailureReferenceNotFound, nil, testTime())
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.MarkPendingReference(testTime()); err == nil {
				t.Fatal("terminal state reopened")
			}
		})
	}
}

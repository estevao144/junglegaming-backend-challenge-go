//go:build integration

package application_test

import (
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jungle-gaming/internal/application"
	"jungle-gaming/internal/platform/observability"
	httptransport "jungle-gaming/internal/transport/http"
)

func TestCompleteHTTPWithRealInfrastructure(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	c := authConfig(q.config)
	auth, _ := testAuthenticator(t, c)
	metrics := observability.NewMetrics()
	financial := application.NewAuthorizedFinancialService(application.NewObservedFinancialService(f.store, metrics))
	handler := httptransport.NewObservedOperationHandler(financial, metrics, outboxLogger())
	server := httptest.NewServer(httptransport.NewAuthenticatedMux(httptransport.NewHealth(c, f.database, q.queue, outboxLogger()), handler, auth))
	defer server.Close()
	internal := realToken(t, c, "wallet-internal")
	alpha := realToken(t, c, "provider-alpha")
	beta := realToken(t, c, "provider-beta")
	call := func(method, path, token, body, key string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(f.ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		req.Header.Set("X-Correlation-ID", "http-6b-test")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s: status %d want %d body=%s", method, path, response.StatusCode, want, data)
		}
		if strings.Contains(path, "health") || path == "/metrics" {
			return data
		}
		if response.Header.Get("X-Correlation-ID") != "http-6b-test" {
			t.Fatal("correlation ID not propagated")
		}
		return data
	}
	walletBody := func(player, amount string) string {
		return fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, player, amount)
	}
	var created struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
		Balance struct {
			Amount string `json:"amount"`
		} `json:"balance"`
	}
	// README restricts all wallet endpoints to the internal service.
	call("POST", "/wallets", "", walletBody("p", "0.00"), "", 401)
	call("POST", "/wallets", alpha, walletBody("p", "0.00"), "", 403)
	if count(t, f, `SELECT count(*) FROM wallets`) != 0 {
		t.Fatal("unauthorized creation wrote wallet")
	}
	data := call("POST", "/wallets", internal, walletBody("zero-player", "0.00"), "", 201)
	if json.Unmarshal(data, &created) != nil || created.Version != 1 || created.Balance.Amount != "0.00" {
		t.Fatal(string(data))
	}
	zeroID := created.ID
	if count(t, f, `SELECT count(*) FROM wager_transactions`)+count(t, f, `SELECT count(*) FROM wallet_ledger_entries`)+count(t, f, `SELECT count(*) FROM outbox_events`) != 0 {
		t.Fatal("zero opening created financial effects")
	}
	zeroReconcile := call("POST", "/wallets/"+zeroID+"/reconciliation", internal, "", "", 200)
	if !strings.Contains(string(zeroReconcile), `"consistent":true`) || !strings.Contains(string(zeroReconcile), `"checkedEntries":0`) {
		t.Fatal(string(zeroReconcile))
	}
	data = call("POST", "/wallets", internal, walletBody("positive-player", "100.00"), "", 201)
	if json.Unmarshal(data, &created) != nil || created.Version != 1 || created.Balance.Amount != "100.00" {
		t.Fatal(string(data))
	}
	walletID := created.ID
	if count(t, f, `SELECT count(*) FROM wager_transactions WHERE kind='OPENING' AND status='PROCESSED'`) != 1 || count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE direction='CREDIT'`) != 1 || count(t, f, `SELECT count(*) FROM outbox_events`) != 2 {
		t.Fatal("positive opening incomplete")
	}
	call("POST", "/wallets", internal, walletBody("positive-player", "100.00"), "", 409)
	if count(t, f, `SELECT count(*) FROM wager_transactions WHERE kind='OPENING'`) != 1 || count(t, f, `SELECT count(*) FROM outbox_events`) != 2 {
		t.Fatal("duplicate opening")
	}
	for _, amount := range []string{"-1.00", "1.0", "1e2", "NaN", "Infinity"} {
		call("POST", "/wallets", internal, walletBody("invalid", amount), "", 400)
	}
	call("GET", "/wallets/"+walletID, alpha, "", "", 403)
	call("GET", "/wallets/"+walletID, beta, "", "", 403)
	call("GET", "/wallets/missing", internal, "", "", 404)
	call("GET", "/wallets/"+walletID, internal, "", "", 200)
	operation := func(kind, external, amount, reference string) string {
		ref := ""
		if reference != "" {
			ref = fmt.Sprintf(`,"referenceExternalTransactionId":%q`, reference)
		}
		return fmt.Sprintf(`{"providerId":"provider-alpha","externalTransactionId":%q,"playerId":"positive-player","walletId":%q,"roundId":"round","gameId":"game","kind":%q,"money":{"amount":%q,"currency":"BRL"}%s}`, external, walletID, kind, amount, ref)
	}
	call("POST", "/wagering/transactions", alpha, operation("BET", "missing-key", "25.00", ""), "", 400)
	call("POST", "/wagering/transactions", alpha, operation("OPENING", "external-opening", "25.00", ""), "opening", 400)
	data = call("POST", "/wagering/transactions", alpha, operation("BET", "bet", "25.00", ""), "bet", 200)
	var tx struct {
		TransactionID string `json:"transactionId"`
	}
	if json.Unmarshal(data, &tx) != nil {
		t.Fatal(string(data))
	}
	betID := tx.TransactionID
	call("POST", "/wagering/transactions", alpha, operation("WIN", "win", "10.00", "bet"), "win", 200)
	// A malformed reference is durably rejected without crediting the wallet.
	invalidWin := strings.Replace(operation("WIN", "wrong-round-win", "10.00", "bet"), `"roundId":"round"`, `"roundId":"other"`, 1)
	call("POST", "/wagering/transactions", alpha, invalidWin, "wrong-round-win", 422)
	replay := call("POST", "/wagering/transactions", alpha, operation("BET", "bet", "25.00", ""), "bet", 200)
	if !strings.Contains(string(replay), `"amount":"75.00"`) || !strings.Contains(string(replay), `"idempotentReplay":true`) {
		t.Fatal("historical replay", string(replay))
	}
	call("POST", "/wagering/transactions", alpha, operation("BET", "bet", "26.00", ""), "bet", 409)
	historical := call("GET", "/wagering/transactions/"+betID, alpha, "", "", 200)
	if !strings.Contains(string(historical), `"amount":"75.00"`) || !strings.Contains(string(historical), `"externalTransactionId":"bet"`) {
		t.Fatal(string(historical))
	}
	call("GET", "/wagering/transactions/"+betID, beta, "", "", 404)
	call("GET", "/providers/provider-alpha/wagering/transactions/bet", beta, "", "", 403)
	call("GET", "/providers/provider-alpha/wagering/transactions/bet", alpha, "", "", 200)
	call("POST", "/wagering/transactions", alpha, operation("REFUND", "refund", "25.00", "bet"), "refund", 200)
	call("POST", "/wagering/transactions", alpha, operation("ROLLBACK", "rollback", "10.00", "win"), "rollback", 200)
	call("POST", "/wagering/transactions", alpha, operation("LOSS", "loss", "0.00", ""), "loss", 200)
	call("POST", "/wagering/transactions", alpha, operation("BET", "insufficient", "999.00", ""), "insufficient", 422)
	pending := call("POST", "/wagering/transactions", alpha, operation("REFUND", "pending", "1.00", "absent"), "pending", 202)
	if json.Unmarshal(pending, &tx) != nil {
		t.Fatal(string(pending))
	}
	call("GET", "/wagering/transactions/"+tx.TransactionID, alpha, "", "", 200)
	call("GET", "/wallets/"+walletID+"/ledger", alpha, "", "", 403)
	call("POST", "/wallets/"+walletID+"/reconciliation", beta, "", "", 403)
	call("GET", "/wallets/"+walletID+"/ledger?limit=201", internal, "", "", 400)
	call("GET", "/wallets/"+walletID+"/ledger?cursor=broken", internal, "", "", 400)
	seen := map[string]bool{}
	cursor := ""
	for {
		path := "/wallets/" + walletID + "/ledger?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		data := call("GET", path, internal, "", "", 200)
		var page struct {
			Items []struct {
				ID    string `json:"id"`
				Money struct {
					Amount string `json:"amount"`
				} `json:"money"`
			} `json:"items"`
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(data, &page) != nil || len(page.Items) > 2 {
			t.Fatal(string(data))
		}
		for _, item := range page.Items {
			if seen[item.ID] || item.Money.Amount == "" {
				t.Fatal("invalid page")
			}
			seen[item.ID] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		call("GET", "/wallets/"+zeroID+"/ledger?cursor="+cursor, internal, "", "", 400)
	}
	if len(seen) != 5 {
		t.Fatalf("ledger entries %d want 5", len(seen))
	}
	beforeTransactions := count(t, f, `SELECT count(*) FROM wager_transactions`)
	beforeOutbox := count(t, f, `SELECT count(*) FROM outbox_events`)
	consistent := call("POST", "/wallets/"+walletID+"/reconciliation", internal, "", "", 200)
	if !strings.Contains(string(consistent), `"consistent":true`) || !strings.Contains(string(consistent), `"checkedEntries":5`) {
		t.Fatal(string(consistent))
	}
	// Deliberate administrative corruption in this isolated test schema only.
	// Disable the constraint in the same SQL transaction and restore it before COMMIT.
	corruption, err := f.database.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer corruption.Rollback(f.ctx)
	if _, err := corruption.Exec(f.ctx, `ALTER TABLE wallets DISABLE TRIGGER wallet_movement_consistency`); err != nil {
		t.Fatal(err)
	}
	if _, err := corruption.Exec(f.ctx, `UPDATE wallets SET balance_minor_units=balance_minor_units+1 WHERE id=$1`, walletID); err != nil {
		t.Fatal(err)
	}
	if _, err := corruption.Exec(f.ctx, `ALTER TABLE wallets ENABLE TRIGGER wallet_movement_consistency`); err != nil {
		t.Fatal(err)
	}
	if err := corruption.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	divergent := call("POST", "/wallets/"+walletID+"/reconciliation", internal, "", "", 200)
	if !strings.Contains(string(divergent), `"consistent":false`) || !strings.Contains(string(divergent), `"amount":"0.01"`) {
		t.Fatal(string(divergent))
	}
	corruptedWallet, err := f.store.GetWallet(f.ctx, walletID)
	if err != nil || corruptedWallet.Snapshot().Balance.MinorUnits() != 10001 || corruptedWallet.Snapshot().Version != 5 {
		t.Fatal("reconciliation changed wallet", err)
	}
	if count(t, f, `SELECT count(*) FROM wager_transactions`) != beforeTransactions || count(t, f, `SELECT count(*) FROM outbox_events`) != beforeOutbox || count(t, f, `SELECT count(*) FROM wallet_ledger_entries`) != 5 {
		t.Fatal("reconciliation changed state")
	}
	oldest, err := f.store.OldestPendingEvent(f.ctx)
	if err != nil || oldest == nil {
		t.Fatal(oldest, err)
	}
	metrics.Outbox(oldest, true)
	if _, err := q.client.SendMessage(f.ctx, &sqs.SendMessageInput{QueueUrl: aws.String(q.dlq), MessageBody: aws.String("metric fixture"), MessageGroupId: aws.String("metric"), MessageDeduplicationId: aws.String("metric-fixture")}); err != nil {
		t.Fatal(err)
	}
	dlq, err := q.queue.DLQVisible(f.ctx)
	if err != nil || dlq != 1 {
		t.Fatal(err)
	}
	metrics.DLQ(dlq, true)
	exposition := string(call("GET", "/metrics", "", "", "", 200))
	for _, want := range []string{`jungle_operations_total{transport="http",kind="BET",result="PROCESSED"} 2`, `jungle_replays_total{transport="http"} 1`, `jungle_reconciliation_divergences_total 1`, `jungle_dlq_visible_messages 1`, `jungle_metrics_dependency_available{dependency="postgres"} 1`, `jungle_metrics_dependency_available{dependency="sqs"} 1`} {
		if !strings.Contains(exposition, want) {
			t.Fatal("metric missing", want, exposition)
		}
	}
	for _, forbidden := range []string{walletID, betID, "http-6b-test", "walletId=", "providerId="} {
		if strings.Contains(exposition, forbidden) {
			t.Fatal("high cardinality metric")
		}
	}
	call("GET", "/health/live", "", "", "", 200)
	call("GET", "/health/ready", "", "", "", 200)
}

func TestPositiveHTTPOpeningRollsBackOnOutboxFailure(t *testing.T) {
	f := newFixture(t)
	_, err := f.database.Pool().Exec(f.ctx, `CREATE FUNCTION fail_opening_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled failure'; END; $$;
 CREATE TRIGGER fail_opening_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_opening_outbox();`)
	if err != nil {
		t.Fatal(err)
	}
	q := newOperationQueue(t, f)
	c := authConfig(q.config)
	a, _ := testAuthenticator(t, c)
	handler := httptransport.NewOperationHandler(application.NewAuthorizedFinancialService(f.service))
	server := httptest.NewServer(httptransport.NewAuthenticatedMux(httptransport.NewHealth(c, f.database, q.queue, outboxLogger()), handler, a))
	defer server.Close()
	req, err := http.NewRequestWithContext(f.ctx, "POST", server.URL+"/wallets", strings.NewReader(`{"playerId":"atomic-player","initialBalance":{"amount":"100.00","currency":"BRL"}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+realToken(t, c, "wallet-internal"))
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 500 || strings.Contains(string(data), "controlled failure") {
		t.Fatal(response.StatusCode, string(data), err)
	}
	for _, table := range []string{"wallets", "wager_transactions", "wallet_ledger_entries", "outbox_events"} {
		if count(t, f, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("partial opening", table)
		}
	}
}

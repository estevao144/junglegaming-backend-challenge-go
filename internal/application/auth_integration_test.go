//go:build integration

package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/auth"
	"jungle-gaming/internal/security"
	httptransport "jungle-gaming/internal/transport/http"
	"jungle-gaming/internal/workers"
)

func authConfig(c config.Config) config.Config {
	c.OIDCIssuerURL = os.Getenv("OIDC_ISSUER_URL")
	c.OIDCInternalURL = os.Getenv("OIDC_INTERNAL_URL")
	c.OIDCAudience = os.Getenv("OIDC_AUDIENCE")
	c.MessagingClientID = os.Getenv("MESSAGING_CLIENT_ID")
	c.MessagingClientSecret = os.Getenv("MESSAGING_CLIENT_SECRET")
	return c
}
func testAuthenticator(t *testing.T, c config.Config) (*auth.Authenticator, *auth.MessagingIdentity) {
	t.Helper()
	var a *auth.Authenticator
	var m *auth.MessagingIdentity
	graph := fx.New(fx.NopLogger, fx.Supply(c), auth.Module, fx.Populate(&a, &m))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := graph.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := graph.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return a, m
}
func realToken(t *testing.T, c config.Config, clientID string) string {
	t.Helper()
	issuer := c.OIDCIssuerURL
	if c.OIDCInternalURL != "" {
		original, _ := url.Parse(issuer)
		internal, _ := url.Parse(c.OIDCInternalURL)
		original.Scheme, original.Host = internal.Scheme, internal.Host
		issuer = original.String()
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientID + "-local"}}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.PostForm(issuer+"/protocol/openid-connect/token", form)
	if err != nil {
		t.Fatal("real Keycloak token endpoint unavailable")
	}
	defer response.Body.Close()
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&result) != nil || result.AccessToken == "" {
		t.Fatal("real client_credentials failed")
	}
	return result.AccessToken
}
func authenticatedBody(t *testing.T, c application.ProcessCommand) string {
	t.Helper()
	amount, _ := c.Money.Decimal()
	body := map[string]any{"providerId": c.ProviderID, "externalTransactionId": c.ExternalTransactionID, "playerId": c.PlayerID, "walletId": c.WalletID, "roundId": c.RoundID, "gameId": c.GameID, "kind": c.Kind, "money": map[string]string{"amount": amount, "currency": c.Money.Currency()}}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestRealAuthenticatedFinancialHTTPAndProviderIsolation(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	c := authConfig(q.config)
	a, _ := testAuthenticator(t, c)
	financial := application.NewAuthorizedFinancialService(f.service)
	health := httptransport.NewHealth(c, f.database, q.queue, outboxLogger())
	server := httptest.NewServer(httptransport.NewAuthenticatedMux(health, httptransport.NewOperationHandler(financial), a))
	defer server.Close()
	w := open(t, f, "auth-player", "100.00")
	alpha := realToken(t, c, "provider-alpha")
	beta := realToken(t, c, "provider-beta")
	cmd := command(t, w, domain.Bet, "25.00", "same-external")
	cmd.ProviderID = "provider-alpha"
	cmd.IdempotencyKey = "same-key"
	post := func(token string, command application.ProcessCommand, want int) {
		t.Helper()
		req, err := http.NewRequestWithContext(f.ctx, "POST", server.URL+"/wagering/transactions", strings.NewReader(authenticatedBody(t, command)))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Idempotency-Key", command.IdempotencyKey)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("authenticated HTTP status %d, want %d", response.StatusCode, want)
		}
	}
	post(alpha, cmd, 200)
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	other := cmd
	other.ProviderID = "provider-beta"
	beforeLedger := count(t, f, `SELECT count(*) FROM wallet_ledger_entries`)
	beforeOutbox := count(t, f, `SELECT count(*) FROM outbox_events`)
	post(alpha, other, 403)
	post("", other, 401)
	post("not-a-jwt", other, 401)
	post(realToken(t, c, "auth-test-missing-provider"), cmd, 401)
	post(realToken(t, c, "auth-test-wrong-audience"), cmd, 401)
	post(realToken(t, c, "auth-test-no-role"), cmd, 403)
	post(realToken(t, c, "wagering-messaging"), cmd, 403)
	post(realToken(t, c, "wallet-internal"), cmd, 403)
	if count(t, f, `SELECT count(*) FROM wager_transactions WHERE provider_id='provider-beta'`) != 0 || count(t, f, `SELECT count(*) FROM wallet_ledger_entries`) != beforeLedger || count(t, f, `SELECT count(*) FROM outbox_events`) != beforeOutbox {
		t.Fatal("unauthorized access left financial effects")
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	post(beta, other, 200) // Same external identity/key, separate provider namespace.
	assertBalance(t, f, w.Snapshot().ID, 5000, 3)
	post(alpha, cmd, 200)
	post(beta, other, 200)
	assertBalance(t, f, w.Snapshot().ID, 5000, 3)
	betaTx, err := f.store.GetExternalTransaction(f.ctx, "provider-beta", other.ExternalTransactionID)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := a.Authenticate(f.ctx, alpha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := financial.GetTransaction(security.WithPrincipal(f.ctx, principal), betaTx.Snapshot().Data.ID); err == nil {
		t.Fatal("alpha read beta transaction")
	}
	for _, path := range []string{"/health/live", "/health/ready"} {
		response, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("health became private")
		}
	}
}

func TestRealMessagingAuthorizationBeforeInboxAndFinance(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	c := authConfig(q.config)
	_, identity := testAuthenticator(t, c)
	service := application.NewAuthorizedIncomingService(f.service, identity)
	w := open(t, f, "messaging-auth-player", "100.00")
	cmd := command(t, w, domain.Bet, "25.00", "trusted-message")
	cmd.ProviderID = "provider-alpha"
	consumer := workers.NewOperationConsumer(q.queue, service, q.config, outboxLogger())
	body := operationBody(t, cmd, "authorized-message")
	sendOperation(t, f, q, body, "wallet", "authorized-dedup")
	if err := consumer.Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
		t.Fatal(err)
	}
	assertQueueDeleted(t, f, q)
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	cmd.ProviderID = "untrusted-provider"
	cmd.ExternalTransactionID = "untrusted-message"
	cmd.IdempotencyKey = "untrusted-key"
	sendOperation(t, f, q, operationBody(t, cmd, "untrusted-message"), "wallet", "untrusted-dedup")
	if err := consumer.Handle(f.ctx, receiveOperation(t, f, q)); !errors.Is(err, security.ErrForbidden) {
		t.Fatal("untrusted provider reached inbox")
	}
	if count(t, f, `SELECT count(*) FROM inbox_messages`) != 1 || count(t, f, `SELECT count(*) FROM wager_transactions WHERE provider_id='untrusted-provider'`) != 0 {
		t.Fatal("unauthorized message persisted")
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
}

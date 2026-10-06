package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/observability"
	"jungle-gaming/internal/platform/postgres"
	"jungle-gaming/internal/security"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWalletParsingBeforeIO(t *testing.T) {
	h := NewOperationHandler(nil)
	principal := security.Principal{Subject: "internal", ClientID: "internal", Roles: []string{security.WalletRole}}
	for _, body := range []string{
		`{"playerId":"p","initialBalance":{"amount":"-1.00","currency":"BRL"}}`,
		`{"playerId":"p","initialBalance":{"amount":"1.0","currency":"BRL"}}`,
		`{"playerId":"p","initialBalance":{"amount":1,"currency":"BRL"}}`,
		`{"playerId":"p","initialBalance":{"amount":"1e2","currency":"BRL"}}`,
		`{"playerId":"p","initialBalance":{"amount":"NaN","currency":"BRL"}}`,
		`{"playerId":"p","initialBalance":{"amount":"Infinity","currency":"BRL"}}`,
		`{"playerId":"p","initialBalance":{"amount":"1.00","currency":"USD"}}`,
		`{"playerId":"p","initialBalance":{"amount":"0.00","currency":"BRL"},"extra":true}`,
		`{`, `{} {}`,
	} {
		r := httptest.NewRequest("POST", "/wallets", strings.NewReader(body)).WithContext(security.WithPrincipal(context.Background(), principal))
		w := httptest.NewRecorder()
		h.OpenWallet(w, r)
		if w.Code != 400 {
			t.Fatalf("status=%d body=%s", w.Code, body)
		}
	}
}

func TestWalletPermissionPrecedesParsing(t *testing.T) {
	h := NewOperationHandler(nil)
	for _, role := range []string{security.ProviderRole, security.MessagingRole, ""} {
		p := security.Principal{Subject: "s", ClientID: "c", ProviderID: "alpha", Roles: []string{role}}
		for _, handle := range []func(*httptest.ResponseRecorder){
			func(w *httptest.ResponseRecorder) {
				h.OpenWallet(w, httptest.NewRequest("POST", "/wallets", strings.NewReader("broken")).WithContext(security.WithPrincipal(context.Background(), p)))
			},
			func(w *httptest.ResponseRecorder) {
				h.Ledger(w, httptest.NewRequest("GET", "/wallets/w/ledger?limit=broken", nil).WithContext(security.WithPrincipal(context.Background(), p)))
			},
		} {
			w := httptest.NewRecorder()
			handle(w)
			if w.Code != 403 {
				t.Fatal(w.Code)
			}
		}
	}
}

func TestLedgerLimit(t *testing.T) {
	for value, want := range map[string]int{"": 50, "1": 1, "200": 200, "0": 0, "201": 0, "-1": 0, "x": 0} {
		got, err := ledgerLimit(value)
		if got != want || (err != nil) != (want == 0) {
			t.Fatalf("%q: %d %v", value, got, err)
		}
	}
}

func TestWalletDTOMoneyAndVersion(t *testing.T) {
	for _, amount := range []string{"0.00", "100.00"} {
		money, err := domain.NewMoney(amount, "BRL")
		if err != nil {
			t.Fatal(err)
		}
		wallet, err := domain.NewWallet("w", "p", money, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(walletDTO(wallet))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"amount":"`+amount+`"`) || !strings.Contains(string(data), `"version":1`) {
			t.Fatal(string(data))
		}
	}
}

func TestErrorMappingDoesNotLeakDetails(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{
		{security.ErrUnauthenticated, 401}, {security.ErrForbidden, 403}, {postgres.ErrNotFound, 404}, {postgres.ErrConflict, 409}, {postgres.ErrConcurrentChange, 409}, {postgres.ErrInvalidCursor, 400}, {context.DeadlineExceeded, 503}, {errors.New("SQL secret stack trace"), 500},
	} {
		w := httptest.NewRecorder()
		respondError(w, test.err)
		if w.Code != test.status || strings.Contains(w.Body.String(), "SQL") || strings.Contains(w.Body.String(), "secret") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestCorrelationLogsExcludeCredentialsAndPropagate(t *testing.T) {
	var output bytes.Buffer
	h := &OperationHandler{metrics: observability.NewMetrics(), log: slog.New(slog.NewJSONHandler(&output, nil))}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if correlationID(r) == "" {
			t.Fatal("missing correlation")
		}
		traceResource(r, "alpha", "wallet", "transaction", "external", "PROCESSED")
		respond(w, 200, map[string]string{"status": "ok"})
	})
	r := httptest.NewRequest("POST", "/wagering/transactions", strings.NewReader("private financial payload"))
	r.Header.Set("Authorization", "Bearer private-token")
	w := httptest.NewRecorder()
	h.Observe(next).ServeHTTP(w, r)
	if w.Header().Get("X-Correlation-ID") == "" {
		t.Fatal("missing correlation header")
	}
	text := output.String()
	for _, required := range []string{"correlationId", "providerId", "walletId", "transactionId", "externalTransactionId", "latencyMicros"} {
		if !strings.Contains(text, required) {
			t.Fatal(required)
		}
	}
	for _, forbidden := range []string{"private-token", "Authorization", "financial payload"} {
		if strings.Contains(text, forbidden) {
			t.Fatal("log leaked", forbidden)
		}
	}
}

func TestAuthenticationPrecedesInvalidCorrelation(t *testing.T) {
	h := NewOperationHandler(nil)
	mux := NewAuthenticatedMux(&Health{}, h, nil)
	request := httptest.NewRequest("POST", "/wallets", strings.NewReader("{}"))
	request.Header.Set("X-Correlation-ID", strings.Repeat("x", 129))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatalf("missing token must return 401, got %d", response.Code)
	}
}

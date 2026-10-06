package httptransport

import (
	"errors"
	"log/slog"
	"net/http"

	"jungle-gaming/internal/application"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/auth"
	"jungle-gaming/internal/platform/observability"
	"jungle-gaming/internal/platform/postgres"
	"jungle-gaming/internal/security"
)

type OperationHandler struct {
	financial *application.AuthorizedFinancialService
	metrics   *observability.Metrics
	log       *slog.Logger
}

func NewOperationHandler(financial *application.AuthorizedFinancialService) *OperationHandler {
	return &OperationHandler{financial: financial, metrics: observability.NewMetrics(), log: defaultHTTPLogger()}
}

func NewAuthenticatedMux(h *Health, handler *OperationHandler, a *auth.Authenticator) *http.ServeMux {
	mux := NewMux(h)
	for _, route := range []struct {
		pattern string
		serve   http.HandlerFunc
	}{
		{"POST /wagering/transactions", handler.Process},
		{"POST /wallets", handler.OpenWallet},
		{"GET /wallets/{walletId}", handler.GetWallet},
		{"GET /wallets/{walletId}/ledger", handler.Ledger},
		{"POST /wallets/{walletId}/reconciliation", handler.Reconciliation},
		{"GET /wagering/transactions/{transactionId}", handler.Transaction},
		{"GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", handler.ExternalTransaction},
	} {
		mux.Handle(route.pattern, handler.Observe(RequireAuthentication(a, handler.authenticatedTrace(route.serve))))
	}
	// Operational metrics are public locally; deploy behind a private network.
	mux.Handle("GET /metrics", handler.metrics)
	return mux
}

type operationRequest struct {
	ProviderID            string                 `json:"providerId"`
	ExternalTransactionID string                 `json:"externalTransactionId"`
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

func (h *OperationHandler) Process(w http.ResponseWriter, r *http.Request) {
	p, err := security.PrincipalFromContext(r.Context())
	if err != nil {
		unauthorized(w)
		return
	}
	if !p.HasRole(security.ProviderRole) {
		respond(w, 403, map[string]string{"error": "forbidden"})
		return
	}
	var body operationRequest
	if err := decodeBody(w, r, &body); err != nil {
		respond(w, 400, map[string]string{"error": "invalid_input"})
		return
	}
	if err := p.AuthorizeProvider(body.ProviderID); err != nil {
		respond(w, 403, map[string]string{"error": "forbidden"})
		return
	}
	money, err := domain.NewMoney(body.Money.Amount, body.Money.Currency)
	if err != nil {
		respond(w, 400, map[string]string{"error": "invalid_money"})
		return
	}
	correlation := r.Header.Get("X-Correlation-ID")
	if correlation == "" {
		correlation = correlationID(r)
	}
	command := application.ProcessCommand{ProviderID: body.ProviderID, ExternalTransactionID: body.ExternalTransactionID, IdempotencyKey: r.Header.Get("Idempotency-Key"), PlayerID: body.PlayerID, WalletID: body.WalletID, RoundID: body.RoundID, GameID: body.GameID, Kind: body.Kind, Money: money, ReferenceExternalTransactionID: body.ReferenceExternalTransactionID, CorrelationID: correlation}
	result, err := h.financial.Process(r.Context(), command)
	if err != nil {
		status, code := http.StatusInternalServerError, "internal_error"
		switch {
		case errors.Is(err, security.ErrForbidden):
			status, code = 403, "forbidden"
		case errors.Is(err, security.ErrUnauthenticated):
			unauthorized(w)
			return
		case errors.Is(err, postgres.ErrConflict):
			status, code = 409, "idempotency_conflict"
		case errors.Is(err, postgres.ErrNotFound):
			status, code = 404, "not_found"
		case errors.Is(err, postgres.ErrConcurrentChange):
			status, code = 409, "concurrent_change"
		case application.TerminalFailure(err) != "":
			status, code = 400, "invalid_input"
		case infrastructureUnavailable(err):
			status, code = 503, "unavailable"
		}
		respond(w, status, map[string]string{"error": code})
		return
	}
	state := result.Transaction
	traceResource(r, state.Data.ProviderID, state.Data.WalletID, state.Data.ID, state.Data.ExternalTransactionID, string(state.Status))
	response := struct {
		TransactionID    string                   `json:"transactionId"`
		Status           domain.TransactionStatus `json:"status"`
		FailureCode      domain.FailureCode       `json:"failureCode,omitempty"`
		IdempotentReplay bool                     `json:"idempotentReplay"`
		Balance          *domain.Money            `json:"balance,omitempty"`
		WalletVersion    *int64                   `json:"walletVersion,omitempty"`
	}{TransactionID: state.Data.ID, Status: state.Status, FailureCode: state.FailureCode, IdempotentReplay: result.IdempotentReplay}
	if state.Result != nil {
		response.Balance = &state.Result.Balance
		response.WalletVersion = &state.Result.WalletVersion
	}
	status := http.StatusOK
	if state.Status == domain.PendingReference {
		status = http.StatusAccepted
	}
	if state.Status == domain.Rejected {
		status = http.StatusUnprocessableEntity
	}
	respond(w, status, response)
}

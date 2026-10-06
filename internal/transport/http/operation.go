package httptransport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"jungle-gaming/internal/application"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/auth"
	"jungle-gaming/internal/platform/postgres"
	"jungle-gaming/internal/security"
)

type OperationHandler struct {
	financial *application.AuthorizedFinancialService
}

func NewOperationHandler(financial *application.AuthorizedFinancialService) *OperationHandler {
	return &OperationHandler{financial}
}

// Only the submission route is added in 6A to prove authorization with real
// financial effects. Wallet, reconciliation and the complete API belong to 6B.
func NewAuthenticatedMux(h *Health, handler *OperationHandler, a *auth.Authenticator) *http.ServeMux {
	mux := NewMux(h)
	mux.Handle("POST /wagering/transactions", RequireAuthentication(a, http.HandlerFunc(handler.Process)))
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
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body operationRequest
	if err := decoder.Decode(&body); err != nil {
		respond(w, 400, map[string]string{"error": "invalid_input"})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
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
		correlation = p.Subject
	}
	command := application.ProcessCommand{ProviderID: body.ProviderID, ExternalTransactionID: body.ExternalTransactionID, IdempotencyKey: r.Header.Get("Idempotency-Key"), PlayerID: body.PlayerID, WalletID: body.WalletID, RoundID: body.RoundID, GameID: body.GameID, Kind: body.Kind, Money: money, ReferenceExternalTransactionID: body.ReferenceExternalTransactionID, CorrelationID: correlation}
	result, err := h.financial.Process(r.Context(), command)
	if err != nil {
		status, code := http.StatusServiceUnavailable, "unavailable"
		switch {
		case errors.Is(err, security.ErrForbidden):
			status, code = 403, "forbidden"
		case errors.Is(err, security.ErrUnauthenticated):
			unauthorized(w)
			return
		case errors.Is(err, postgres.ErrConflict):
			status, code = 409, "idempotency_conflict"
		case application.TerminalFailure(err) != "":
			status, code = 400, "invalid_input"
		}
		respond(w, status, map[string]string{"error": code})
		return
	}
	state := result.Transaction
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

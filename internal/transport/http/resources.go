package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
	"jungle-gaming/internal/security"
)

func decodeBody(w http.ResponseWriter, r *http.Request, body any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("expected a single JSON object")
	}
	return nil
}

func respondError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal_error"
	switch {
	case errors.Is(err, security.ErrUnauthenticated):
		unauthorized(w)
		return
	case errors.Is(err, security.ErrForbidden):
		status, code = 403, "forbidden"
	case errors.Is(err, postgres.ErrNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, postgres.ErrConflict), errors.Is(err, postgres.ErrConcurrentChange):
		status, code = 409, "conflict"
	case errors.Is(err, postgres.ErrInvalidCursor), errors.Is(err, domain.ErrInvalidWallet):
		status, code = 400, "invalid_input"
	case infrastructureUnavailable(err):
		status, code = 503, "unavailable"
	}
	respond(w, status, map[string]string{"error": code})
}

type walletResponse struct {
	ID        string       `json:"id"`
	PlayerID  string       `json:"playerId"`
	Balance   domain.Money `json:"balance"`
	Version   int64        `json:"version"`
	CreatedAt time.Time    `json:"createdAt"`
	UpdatedAt time.Time    `json:"updatedAt"`
}

func walletDTO(wallet *domain.Wallet) walletResponse {
	s := wallet.Snapshot()
	return walletResponse{s.ID, s.PlayerID, s.Balance, s.Version, s.CreatedAt, s.UpdatedAt}
}

func infrastructureUnavailable(err error) bool {
	var networkError net.Error
	var pgError *pgconn.PgError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) {
		return true
	}
	if errors.As(err, &pgError) {
		return len(pgError.Code) >= 2 && pgError.Code[:2] == "08" || pgError.Code == "57P01" || pgError.Code == "57P02" || pgError.Code == "57P03"
	}
	return false
}

func (h *OperationHandler) OpenWallet(w http.ResponseWriter, r *http.Request) {
	if err := authorizeWalletRequest(r); err != nil {
		respondError(w, err)
		return
	}
	var body struct {
		PlayerID       string `json:"playerId"`
		InitialBalance struct {
			Amount   string `json:"amount"`
			Currency string `json:"currency"`
		} `json:"initialBalance"`
	}
	if decodeBody(w, r, &body) != nil {
		respond(w, 400, map[string]string{"error": "invalid_input"})
		return
	}
	balance, err := domain.NewMoney(body.InitialBalance.Amount, body.InitialBalance.Currency)
	if err != nil || balance.MinorUnits() < 0 {
		respond(w, 400, map[string]string{"error": "invalid_money"})
		return
	}
	wallet, err := h.financial.OpenWallet(r.Context(), body.PlayerID, balance, correlationID(r))
	if err != nil {
		respondError(w, err)
		return
	}
	traceResource(r, "", wallet.Snapshot().ID, "", "", "created")
	w.Header().Set("Location", "/wallets/"+wallet.Snapshot().ID)
	respond(w, 201, walletDTO(wallet))
}

func authorizeWalletRequest(r *http.Request) error {
	p, err := security.PrincipalFromContext(r.Context())
	if err != nil {
		return err
	}
	return p.AuthorizeWallet()
}
func (h *OperationHandler) GetWallet(w http.ResponseWriter, r *http.Request) {
	wallet, err := h.financial.GetWallet(r.Context(), r.PathValue("walletId"))
	if err != nil {
		respondError(w, err)
		return
	}
	traceResource(r, "", wallet.Snapshot().ID, "", "", "read")
	respond(w, 200, walletDTO(wallet))
}

type ledgerResponse struct {
	ID            string           `json:"id"`
	WalletID      string           `json:"walletId"`
	TransactionID string           `json:"transactionId"`
	Direction     domain.Direction `json:"direction"`
	Money         domain.Money     `json:"money"`
	BalanceBefore domain.Money     `json:"balanceBefore"`
	BalanceAfter  domain.Money     `json:"balanceAfter"`
	CreatedAt     time.Time        `json:"createdAt"`
}

func ledgerLimit(value string) (int, error) {
	if value == "" {
		return 50, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > 200 {
		return 0, postgres.ErrInvalidCursor
	}
	return limit, nil
}
func (h *OperationHandler) Ledger(w http.ResponseWriter, r *http.Request) {
	// Permission precedes validation and reads, including malformed pagination.
	if err := authorizeWalletRequest(r); err != nil {
		respondError(w, err)
		return
	}
	limit, err := ledgerLimit(r.URL.Query().Get("limit"))
	if err != nil {
		respondError(w, err)
		return
	}
	id := r.PathValue("walletId")
	page, err := h.financial.LedgerPage(r.Context(), id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		respondError(w, err)
		return
	}
	items := make([]ledgerResponse, 0, len(page.Entries))
	for _, e := range page.Entries {
		items = append(items, ledgerResponse{e.ID, e.WalletID, e.TransactionID, e.Direction, e.Money, e.BalanceBefore, e.BalanceAfter, e.CreatedAt})
	}
	traceResource(r, "", id, "", "", "read")
	respond(w, 200, struct {
		Items      []ledgerResponse `json:"items"`
		NextCursor string           `json:"nextCursor,omitempty"`
	}{items, page.NextCursor})
}

type transactionResponse struct {
	TransactionID                  string                   `json:"transactionId"`
	ExternalTransactionID          string                   `json:"externalTransactionId,omitempty"`
	ProviderID                     string                   `json:"providerId,omitempty"`
	WalletID                       string                   `json:"walletId"`
	PlayerID                       string                   `json:"playerId"`
	RoundID                        string                   `json:"roundId,omitempty"`
	GameID                         string                   `json:"gameId,omitempty"`
	Kind                           domain.TransactionKind   `json:"kind"`
	Money                          domain.Money             `json:"money"`
	ReferenceExternalTransactionID string                   `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string                   `json:"referenceTransactionId,omitempty"`
	Status                         domain.TransactionStatus `json:"status"`
	FailureCode                    domain.FailureCode       `json:"failureCode,omitempty"`
	Balance                        *domain.Money            `json:"balance,omitempty"`
	WalletVersion                  *int64                   `json:"walletVersion,omitempty"`
	CreatedAt                      time.Time                `json:"createdAt"`
	UpdatedAt                      time.Time                `json:"updatedAt"`
}

func transactionDTO(tx *domain.WagerTransaction) transactionResponse {
	s := tx.Snapshot()
	d := s.Data
	result := transactionResponse{TransactionID: d.ID, ExternalTransactionID: d.ExternalTransactionID, ProviderID: d.ProviderID, WalletID: d.WalletID, PlayerID: d.PlayerID, RoundID: d.RoundID, GameID: d.GameID, Kind: d.Kind, Money: d.Money, ReferenceExternalTransactionID: d.ReferenceExternalTransactionID, ReferenceTransactionID: s.ReferenceTransactionID, Status: s.Status, FailureCode: s.FailureCode, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
	if s.Result != nil {
		result.Balance = &s.Result.Balance
		result.WalletVersion = &s.Result.WalletVersion
	}
	return result
}
func (h *OperationHandler) Transaction(w http.ResponseWriter, r *http.Request) {
	tx, err := h.financial.GetTransaction(r.Context(), r.PathValue("transactionId"))
	h.respondTransaction(w, r, tx, err)
}
func (h *OperationHandler) ExternalTransaction(w http.ResponseWriter, r *http.Request) {
	tx, err := h.financial.GetExternalTransaction(r.Context(), r.PathValue("providerId"), r.PathValue("externalTransactionId"))
	h.respondTransaction(w, r, tx, err)
}
func (h *OperationHandler) respondTransaction(w http.ResponseWriter, r *http.Request, tx *domain.WagerTransaction, err error) {
	if err != nil {
		respondError(w, err)
		return
	}
	s := tx.Snapshot()
	traceResource(r, s.Data.ProviderID, s.Data.WalletID, s.Data.ID, s.Data.ExternalTransactionID, string(s.Status))
	respond(w, 200, transactionDTO(tx))
}
func (h *OperationHandler) Reconciliation(w http.ResponseWriter, r *http.Request) {
	result, err := h.financial.Reconcile(r.Context(), r.PathValue("walletId"))
	if err != nil {
		respondError(w, err)
		return
	}
	status := "consistent"
	if !result.Consistent {
		h.metrics.Divergence()
		status = "divergent"
	}
	traceResource(r, "", result.WalletID, "", "", status)
	respond(w, 200, result)
}

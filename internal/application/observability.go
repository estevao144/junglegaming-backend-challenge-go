package application

import (
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"jungle-gaming/internal/platform/observability"
	"jungle-gaming/internal/platform/postgres"
)

func NewObservedFinancialService(store *postgres.Store, metrics *observability.Metrics) *FinancialService {
	return &FinancialService{store: store, metrics: metrics}
}
func (s *FinancialService) observeOperation(transport, kind, status string, replay bool, err error) {
	if err != nil {
		status = "ERROR"
	}
	s.metrics.Operation(transport, kind, status, replay)
	var pgErr *pgconn.PgError
	if errors.Is(err, postgres.ErrConcurrentChange) || (errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")) {
		s.metrics.Conflict()
	}
}

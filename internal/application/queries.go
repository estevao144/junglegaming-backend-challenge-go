package application

import (
	"context"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
	"jungle-gaming/internal/security"
)

func authorizeWallet(ctx context.Context) error {
	p, err := security.PrincipalFromContext(ctx)
	if err != nil {
		return err
	}
	return p.AuthorizeWallet()
}

func (s *AuthorizedFinancialService) OpenWallet(ctx context.Context, player string, balance domain.Money, correlation string) (*domain.Wallet, error) {
	if err := authorizeWallet(ctx); err != nil {
		return nil, err
	}
	return s.financial.OpenWallet(ctx, player, balance, correlation)
}
func (s *AuthorizedFinancialService) GetWallet(ctx context.Context, id string) (*domain.Wallet, error) {
	if err := authorizeWallet(ctx); err != nil {
		return nil, err
	}
	return s.financial.store.GetWallet(ctx, id)
}
func (s *AuthorizedFinancialService) LedgerPage(ctx context.Context, id, cursor string, limit int) (postgres.LedgerPage, error) {
	if err := authorizeWallet(ctx); err != nil {
		return postgres.LedgerPage{}, err
	}
	if _, err := s.financial.store.GetWallet(ctx, id); err != nil {
		return postgres.LedgerPage{}, err
	}
	return s.financial.store.LedgerPage(ctx, id, cursor, limit)
}
func (s *AuthorizedFinancialService) Reconcile(ctx context.Context, id string) (postgres.Reconciliation, error) {
	if err := authorizeWallet(ctx); err != nil {
		return postgres.Reconciliation{}, err
	}
	return s.financial.store.Reconcile(ctx, id)
}
func (s *AuthorizedFinancialService) GetExternalTransaction(ctx context.Context, provider, external string) (*domain.WagerTransaction, error) {
	p, err := security.PrincipalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := p.AuthorizeProvider(provider); err != nil {
		return nil, err
	}
	return s.financial.store.GetExternalTransaction(ctx, p.ProviderID, external)
}

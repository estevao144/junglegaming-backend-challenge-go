package application

import (
	"context"

	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/auth"
	"jungle-gaming/internal/security"
)

// AuthorizedFinancialService is the public boundary. FinancialService remains
// an internal transactional use case, shared by trusted workers and adapters.
type AuthorizedFinancialService struct{ financial *FinancialService }

func NewAuthorizedFinancialService(financial *FinancialService) *AuthorizedFinancialService {
	return &AuthorizedFinancialService{financial}
}

func (s *AuthorizedFinancialService) Process(ctx context.Context, c ProcessCommand) (ProcessResult, error) {
	p, err := security.PrincipalFromContext(ctx)
	if err != nil {
		return ProcessResult{}, err
	}
	if err := p.AuthorizeProvider(c.ProviderID); err != nil {
		return ProcessResult{}, err
	}
	c.ProviderID = p.ProviderID
	return s.financial.Process(ctx, c)
}
func (s *AuthorizedFinancialService) GetTransaction(ctx context.Context, id string) (*domain.WagerTransaction, error) {
	p, err := security.PrincipalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := p.AuthorizeProvider(p.ProviderID); err != nil {
		return nil, err
	}
	return s.financial.store.GetTransaction(ctx, p.ProviderID, id)
}

type AuthorizedIncomingService struct {
	financial *FinancialService
	identity  *auth.MessagingIdentity
}

func NewAuthorizedIncomingService(financial *FinancialService, identity *auth.MessagingIdentity) *AuthorizedIncomingService {
	return &AuthorizedIncomingService{financial, identity}
}
func (s *AuthorizedIncomingService) CheckInbox(ctx context.Context) error {
	return s.financial.CheckInbox(ctx)
}
func (s *AuthorizedIncomingService) ProcessIncoming(ctx context.Context, in IncomingOperation) (IncomingResult, error) {
	p, err := s.identity.Principal(ctx)
	if err != nil {
		return IncomingResult{}, err
	}
	if err := p.AuthorizeMessage(in.Command.ProviderID); err != nil {
		return IncomingResult{}, err
	}
	return s.financial.ProcessIncoming(ctx, in)
}

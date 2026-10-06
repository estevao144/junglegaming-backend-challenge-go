package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/fx"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/security"
)

type MessagingIdentity struct {
	auth      *Authenticator
	config    config.Config
	mutex     sync.Mutex // Only the local token cache; never a financial lock.
	token     *oauth2.Token
	principal security.Principal
}

func NewMessagingIdentity(lc fx.Lifecycle, a *Authenticator, c config.Config) *MessagingIdentity {
	m := &MessagingIdentity{auth: a, config: c}
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		if _, err := m.Principal(ctx); err != nil {
			return fmt.Errorf("messaging service authentication failed")
		}
		return nil
	}})
	return m
}

// Cache token and verified identity until shortly before expiry. No token is
// persisted, logged, or put into the operation payload/financial domain.
func (m *MessagingIdentity) Principal(ctx context.Context) (security.Principal, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return security.Principal{}, err
	}
	if m.token != nil && time.Until(m.token.Expiry) > 10*time.Second {
		return m.principal.Clone(), nil
	}
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, oauth2.HTTPClient, m.auth.client), m.config.DependencyTimeout)
	defer cancel()
	c := clientcredentials.Config{ClientID: m.config.MessagingClientID, ClientSecret: m.config.MessagingClientSecret, TokenURL: m.auth.tokenEndpoint, AuthStyle: oauth2.AuthStyleInParams}
	token, err := c.Token(ctx)
	if err != nil {
		return security.Principal{}, security.ErrUnauthenticated
	}
	p, err := m.auth.Authenticate(ctx, token.AccessToken)
	if err != nil {
		return security.Principal{}, err
	}
	if token.Expiry.IsZero() || p.ExpiresAt.Before(token.Expiry) {
		token.Expiry = p.ExpiresAt
	}
	if p.ClientID != m.config.MessagingClientID || !p.HasRole(security.MessagingRole) || p.HasRole(security.ProviderRole) || len(p.AllowedProviders) == 0 {
		return security.Principal{}, security.ErrForbidden
	}
	m.token, m.principal = token, p.Clone()
	return p.Clone(), nil
}

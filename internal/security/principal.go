// Package security defines identities and authorization without JWT/IdP dependencies.
package security

import (
	"context"
	"errors"
	"time"
)

var ErrUnauthenticated = errors.New("unauthenticated")
var ErrForbidden = errors.New("forbidden")

const ProviderRole = "wagering-provider"
const MessagingRole = "wagering-messaging"

type Principal struct {
	ExpiresAt        time.Time
	Subject          string
	ClientID         string
	ProviderID       string
	Roles            []string
	AllowedProviders []string
}

func (p Principal) HasRole(role string) bool {
	for _, value := range p.Roles {
		if value == role {
			return true
		}
	}
	return false
}
func (p Principal) AuthorizeProvider(provider string) error {
	if !p.HasRole(ProviderRole) || p.ProviderID == "" || provider != p.ProviderID {
		return ErrForbidden
	}
	return nil
}
func (p Principal) AuthorizeMessage(provider string) error {
	if !p.HasRole(MessagingRole) || p.HasRole(ProviderRole) {
		return ErrForbidden
	}
	for _, allowed := range p.AllowedProviders {
		if provider != "" && provider == allowed {
			return nil
		}
	}
	return ErrForbidden
}

type principalKey struct{}

func (p Principal) Clone() Principal {
	p.Roles = append([]string(nil), p.Roles...)
	p.AllowedProviders = append([]string(nil), p.AllowedProviders...)
	return p
}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p.Clone())
}
func PrincipalFromContext(ctx context.Context) (Principal, error) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok || p.Subject == "" || p.ClientID == "" {
		return Principal{}, ErrUnauthenticated
	}
	return p.Clone(), nil
}

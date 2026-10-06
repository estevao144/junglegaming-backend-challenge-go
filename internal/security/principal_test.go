package security

import (
	"context"
	"errors"
	"testing"
)

func TestPrincipalContextAndProviderAuthorization(t *testing.T) {
	if _, err := PrincipalFromContext(context.Background()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("missing identity accepted")
	}
	p := Principal{Subject: "service-account", ClientID: "provider-alpha", ProviderID: "provider-alpha", Roles: []string{ProviderRole}}
	ctx := WithPrincipal(context.Background(), p)
	p.Roles[0] = "other"
	stored, err := PrincipalFromContext(ctx)
	if err != nil || stored.AuthorizeProvider("provider-alpha") != nil {
		t.Fatal("principal context lost identity")
	}
	if !errors.Is(stored.AuthorizeProvider("provider-beta"), ErrForbidden) {
		t.Fatal("provider isolation broken")
	}
	stored.Roles[0] = "other"
	again, _ := PrincipalFromContext(ctx)
	if !again.HasRole(ProviderRole) {
		t.Fatal("principal slices mutable through context")
	}
}
func TestMessagingAuthorization(t *testing.T) {
	p := Principal{Roles: []string{MessagingRole}, AllowedProviders: []string{"provider-alpha"}}
	if p.AuthorizeMessage("provider-alpha") != nil || !errors.Is(p.AuthorizeMessage("provider-beta"), ErrForbidden) {
		t.Fatal("messaging allowlist broken")
	}
	if !errors.Is(p.AuthorizeProvider("provider-alpha"), ErrForbidden) {
		t.Fatal("service impersonated HTTP provider")
	}
	p.Roles = append(p.Roles, ProviderRole)
	if p.AuthorizeMessage("provider-alpha") == nil {
		t.Fatal("mixed identity accepted for messaging")
	}
}

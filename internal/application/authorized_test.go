package application

import (
	"context"
	"errors"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/security"
	"testing"
)

func TestAuthorizationPrecedesFinancialAccess(t *testing.T) {
	s := NewAuthorizedFinancialService(nil) // Any financial call would panic.
	if _, err := s.Process(context.Background(), ProcessCommand{}); !errors.Is(err, security.ErrUnauthenticated) {
		t.Fatal("missing principal reached finance")
	}
	ctx := security.WithPrincipal(context.Background(), security.Principal{Subject: "subject", ClientID: "provider-alpha", ProviderID: "provider-alpha", Roles: []string{security.ProviderRole}})
	if _, err := s.Process(ctx, ProcessCommand{ProviderID: "provider-beta"}); !errors.Is(err, security.ErrForbidden) {
		t.Fatal("provider mismatch reached finance")
	}
	ctx = security.WithPrincipal(context.Background(), security.Principal{Subject: "subject", ClientID: "messaging", Roles: []string{security.MessagingRole}})
	if _, err := s.Process(ctx, ProcessCommand{ProviderID: "provider-alpha"}); !errors.Is(err, security.ErrForbidden) {
		t.Fatal("messaging credential reached HTTP finance")
	}
}

func TestWalletServiceRejectsProviderBeforeAnyIO(t *testing.T) {
	s := NewAuthorizedFinancialService(nil)
	ctx := security.WithPrincipal(context.Background(), security.Principal{Subject: "s", ClientID: "alpha", ProviderID: "alpha", Roles: []string{security.ProviderRole}})
	if _, err := s.OpenWallet(ctx, "player", domain.Money{}, "correlation"); !errors.Is(err, security.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.GetWallet(ctx, "wallet"); !errors.Is(err, security.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.LedgerPage(ctx, "wallet", "", 50); !errors.Is(err, security.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx, "wallet"); !errors.Is(err, security.ErrForbidden) {
		t.Fatal(err)
	}
}

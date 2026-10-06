//go:build integration

package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/fx"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/security"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	c := config.Config{OIDCIssuerURL: os.Getenv("OIDC_ISSUER_URL"), OIDCInternalURL: os.Getenv("OIDC_INTERNAL_URL"), OIDCAudience: os.Getenv("OIDC_AUDIENCE"), MessagingClientID: os.Getenv("MESSAGING_CLIENT_ID"), MessagingClientSecret: os.Getenv("MESSAGING_CLIENT_SECRET"), DependencyTimeout: 5 * time.Second}
	if c.OIDCIssuerURL == "" || c.OIDCAudience == "" {
		t.Fatal("real Keycloak OIDC environment is required")
	}
	return c
}
func realAuth(t *testing.T, c config.Config) (*Authenticator, *MessagingIdentity) {
	t.Helper()
	var a *Authenticator
	var m *MessagingIdentity
	app := fx.New(fx.NopLogger, fx.Supply(c), Module, fx.Populate(&a, &m))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return a, m
}
func clientToken(t *testing.T, a *Authenticator, issuer, client string) *oauth2.Token {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), oauth2.HTTPClient, a.client), 5*time.Second)
	defer cancel()
	c := clientcredentials.Config{ClientID: client, ClientSecret: client + "-local", TokenURL: issuer + "/protocol/openid-connect/token", AuthStyle: oauth2.AuthStyleInParams}
	token, err := c.Token(ctx)
	if err != nil {
		t.Fatalf("real client_credentials failed for %s", client)
	}
	return token
}

func TestRealKeycloakProviderClaimsAndInvalidTokens(t *testing.T) {
	c := testConfig(t)
	a, _ := realAuth(t, c)
	for _, client := range []string{"provider-alpha", "provider-beta"} {
		token := clientToken(t, a, c.OIDCIssuerURL, client)
		p, err := a.Authenticate(context.Background(), token.AccessToken)
		if err != nil || p.ProviderID != client || p.ClientID != client || p.Subject == "" || p.AuthorizeProvider(client) != nil {
			t.Fatalf("provider principal incorrect for %s: %v", client, err)
		}
	}
	for _, client := range []string{"auth-test-missing-provider", "auth-test-wrong-audience"} {
		token := clientToken(t, a, c.OIDCIssuerURL, client)
		if _, err := a.Authenticate(context.Background(), token.AccessToken); !errors.Is(err, security.ErrUnauthenticated) {
			t.Fatalf("invalid claims accepted for %s", client)
		}
	}
	other := clientToken(t, a, strings.TrimSuffix(c.OIDCIssuerURL, "/jungle")+"/jungle-other", "provider-alpha")
	if _, err := a.Authenticate(context.Background(), other.AccessToken); !errors.Is(err, security.ErrUnauthenticated) {
		t.Fatal("other real realm accepted")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fabricated"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: c.OIDCIssuerURL, Subject: "fabricated-service", Audience: jwt.Audience{c.OIDCAudience}, Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}).Claims(map[string]any{"azp": "provider-alpha", "provider_id": "provider-alpha", "typ": "Bearer", "realm_access": map[string]any{"roles": []string{security.ProviderRole}}}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(context.Background(), raw); !errors.Is(err, security.ErrUnauthenticated) {
		t.Fatal("fabricated JWT with matching claims accepted")
	}
	if _, err := a.Authenticate(context.Background(), "eyJhbGciOiJub25lIn0.e30."); !errors.Is(err, security.ErrUnauthenticated) {
		t.Fatal("alg=none accepted")
	}
}

type countTransport struct {
	base  http.RoundTripper
	calls atomic.Int64
}

func (t *countTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return t.base.RoundTrip(r)
}

func TestRealMessagingIdentityCacheAndRenewal(t *testing.T) {
	c := testConfig(t)
	a, m := realAuth(t, c)
	counter := &countTransport{base: a.client.Transport}
	a.client.Transport = counter
	first, err := m.Principal(context.Background())
	if err != nil || first.ClientID != "wagering-messaging" || first.AuthorizeMessage("provider-alpha") != nil || first.AuthorizeProvider("provider-alpha") == nil {
		t.Fatal("messaging identity/permissions incorrect")
	}
	for i := 0; i < 10; i++ {
		if _, err := m.Principal(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	first.AllowedProviders[0] = "untrusted-provider"
	unchanged, err := m.Principal(context.Background())
	if err != nil || unchanged.AuthorizeMessage("untrusted-provider") == nil {
		t.Fatal("caller mutated cached permissions")
	}
	if counter.calls.Load() != 0 {
		t.Fatal("cached messaging identity fetched a token per operation")
	}
	m.token.Expiry = time.Now().Add(time.Second)
	if _, err := m.Principal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if counter.calls.Load() != 1 {
		t.Fatal("token renewal missing or JWKS fetched despite cache")
	}
}

func TestRealKeycloakTokenExpiredAtVerification(t *testing.T) {
	c := testConfig(t)
	a, _ := realAuth(t, c)
	token := clientToken(t, a, c.OIDCIssuerURL, "provider-alpha")
	p, err := a.Authenticate(context.Background(), token.AccessToken)
	if err != nil {
		t.Fatal("real token was not initially valid")
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(context.Background(), a.client), 5*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, c.OIDCIssuerURL)
	if err != nil {
		t.Fatal("real discovery failed")
	}
	// Move the verifier clock past the genuine JWT expiry, without waiting five
	// minutes or modifying signed claims. Runtime continues to use time.Now.
	expired := &Authenticator{timeout: 5 * time.Second, verifier: provider.VerifierContext(ctx, &oidc.Config{ClientID: c.OIDCAudience, SupportedSigningAlgs: []string{oidc.RS256}, Now: func() time.Time { return p.ExpiresAt.Add(time.Second) }})}
	if _, err := expired.Authenticate(ctx, token.AccessToken); !errors.Is(err, security.ErrUnauthenticated) {
		t.Fatal("expired real Keycloak JWT accepted")
	}
}

func TestRealMessagingInvalidCredentialsAndProviderRole(t *testing.T) {
	for _, scenario := range []string{"invalid secret", "provider credential"} {
		t.Run(scenario, func(t *testing.T) {
			c := testConfig(t)
			if scenario == "invalid secret" {
				c.MessagingClientSecret = "invalid-local-secret"
			} else {
				c.MessagingClientID = "provider-alpha"
				c.MessagingClientSecret = "provider-alpha-local"
			}
			var identity *MessagingIdentity
			graph := fx.New(fx.NopLogger, fx.Supply(c), Module, fx.Populate(&identity))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := graph.Start(ctx); err == nil {
				_ = graph.Stop(ctx)
				t.Fatal("invalid messaging authentication permitted startup")
			}
		})
	}
}

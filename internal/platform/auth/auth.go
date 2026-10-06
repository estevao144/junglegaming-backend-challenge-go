package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/fx"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/security"
)

type Authenticator struct {
	verifier      *oidc.IDTokenVerifier
	client        *http.Client
	tokenEndpoint string
	timeout       time.Duration
}

// internalTransport changes only the configured IdP origin, retaining paths and
// the public issuer checked by go-oidc. It permits Docker backchannel networking.
type internalTransport struct {
	base             http.RoundTripper
	issuer, internal *url.URL
}

func (t internalTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.internal == nil || r.URL.Scheme != t.issuer.Scheme || r.URL.Host != t.issuer.Host {
		return t.base.RoundTrip(r)
	}
	copy := r.Clone(r.Context())
	address := *r.URL
	address.Scheme, address.Host = t.internal.Scheme, t.internal.Host
	copy.URL = &address
	copy.Host = ""
	return t.base.RoundTrip(copy)
}

func NewAuthenticator(lc fx.Lifecycle, c config.Config) *Authenticator {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	issuer, _ := url.Parse(c.OIDCIssuerURL)
	var internal *url.URL
	if c.OIDCInternalURL != "" {
		internal, _ = url.Parse(c.OIDCInternalURL)
	}
	client := &http.Client{Transport: internalTransport{transport, issuer, internal}, Timeout: c.DependencyTimeout}
	a := &Authenticator{client: client, timeout: c.DependencyTimeout}
	// The key cache must outlive OnStart's context, but all HTTP calls are bounded.
	keyContext, cancelKeys := context.WithCancel(oidc.ClientContext(context.Background(), client))
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, client), c.DependencyTimeout)
		defer cancel()
		provider, err := oidc.NewProvider(ctx, c.OIDCIssuerURL)
		if err != nil {
			cancelKeys()
			transport.CloseIdleConnections()
			return fmt.Errorf("OIDC discovery failed")
		}
		a.verifier = provider.VerifierContext(keyContext, &oidc.Config{ClientID: c.OIDCAudience, SupportedSigningAlgs: []string{oidc.RS256}})
		a.tokenEndpoint = provider.Endpoint().TokenURL
		return nil
	}, OnStop: func(context.Context) error { cancelKeys(); transport.CloseIdleConnections(); return nil }})
	return a
}

type tokenClaims struct {
	Subject     string `json:"sub"`
	ClientID    string `json:"azp"`
	ProviderID  string `json:"provider_id"`
	Type        string `json:"typ"`
	NotBefore   int64  `json:"nbf"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	AllowedProviders []string `json:"provider_ids"`
}

func principalFromClaims(c tokenClaims) (security.Principal, error) {
	for _, value := range []string{c.Subject, c.ClientID} {
		if value == "" || strings.TrimSpace(value) != value {
			return security.Principal{}, security.ErrUnauthenticated
		}
	}
	if c.Type != "Bearer" || c.NotBefore > time.Now().Unix() {
		return security.Principal{}, security.ErrUnauthenticated
	}
	p := security.Principal{Subject: c.Subject, ClientID: c.ClientID, ProviderID: c.ProviderID, Roles: c.RealmAccess.Roles, AllowedProviders: c.AllowedProviders}
	if p.HasRole(security.ProviderRole) && (p.ProviderID == "" || strings.TrimSpace(p.ProviderID) != p.ProviderID) {
		return security.Principal{}, security.ErrUnauthenticated
	}
	return p, nil
}

func (a *Authenticator) Authenticate(ctx context.Context, raw string) (security.Principal, error) {
	if a.verifier == nil || raw == "" {
		return security.Principal{}, security.ErrUnauthenticated
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	token, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return security.Principal{}, security.ErrUnauthenticated
	}
	var claims tokenClaims
	if err := token.Claims(&claims); err != nil {
		return security.Principal{}, security.ErrUnauthenticated
	}
	p, err := principalFromClaims(claims)
	p.ExpiresAt = token.Expiry
	return p, err
}

var Module = fx.Module("authentication", fx.Provide(NewAuthenticator, NewMessagingIdentity))

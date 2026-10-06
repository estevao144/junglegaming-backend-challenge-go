package auth

import (
	"jungle-gaming/internal/security"
	"testing"
	"time"
)

func TestRequiredVerifiedClaims(t *testing.T) {
	valid := tokenClaims{Subject: "subject", ClientID: "provider-alpha", ProviderID: "provider-alpha", Type: "Bearer"}
	valid.RealmAccess.Roles = []string{security.ProviderRole}
	for _, test := range []struct {
		name    string
		change  func(*tokenClaims)
		invalid bool
	}{
		{"valid", func(*tokenClaims) {}, false},
		{"subject missing", func(c *tokenClaims) { c.Subject = "" }, true},
		{"client missing", func(c *tokenClaims) { c.ClientID = "" }, true},
		{"provider missing", func(c *tokenClaims) { c.ProviderID = "" }, true},
		{"ID token", func(c *tokenClaims) { c.Type = "ID" }, true},
		{"not active yet", func(c *tokenClaims) { c.NotBefore = time.Now().Add(time.Hour).Unix() }, true},
		{"no role", func(c *tokenClaims) { c.RealmAccess.Roles = nil }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := valid
			test.change(&c)
			p, err := principalFromClaims(c)
			if (err != nil) != test.invalid {
				t.Fatalf("claims error %v", err)
			}
			if test.name == "no role" && p.AuthorizeProvider("provider-alpha") == nil {
				t.Fatal("missing role authorized")
			}
		})
	}
}

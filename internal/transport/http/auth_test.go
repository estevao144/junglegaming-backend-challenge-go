package httptransport

import (
	"jungle-gaming/internal/platform/auth"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerAndUnauthorizedResponses(t *testing.T) {
	for _, test := range []struct {
		header string
		valid  bool
	}{
		{"", false}, {"Basic abc", false}, {"Bearer", false}, {"Bearer one two", false}, {"bearer opaque", true}, {"Bearer opaque", true},
	} {
		_, ok := bearerToken(test.header)
		if ok != test.valid {
			t.Fatalf("Bearer extraction for %q", test.header)
		}
		called := false
		h := RequireAuthentication(&auth.Authenticator{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		r := httptest.NewRequest("POST", "/wagering/transactions", nil)
		if test.header != "" {
			r.Header.Set("Authorization", test.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 || called || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("invalid authentication reached handler")
		}
	}
}

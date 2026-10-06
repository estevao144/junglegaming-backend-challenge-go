package httptransport

import (
	"net/http"
	"strings"

	"jungle-gaming/internal/platform/auth"
	"jungle-gaming/internal/security"
)

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

func RequireAuthentication(a *auth.Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			unauthorized(w)
			return
		}
		raw, ok := bearerToken(headers[0])
		if !ok {
			unauthorized(w)
			return
		}
		p, err := a.Authenticate(r.Context(), raw)
		if err != nil {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), p)))
	})
}
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="jungle-api"`)
	respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

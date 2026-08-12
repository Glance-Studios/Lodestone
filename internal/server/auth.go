package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Wraps next so a request passes only if it carries the expected token.
// An empty expected token denies every request, so a bad server fails closed.
func RequireToken(expected string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !tokenValid(expected, bearerToken(r)) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// tokenValid reports whether got equals expected, compared in constant time so
// the check leaks no timing information about the token.
// An empty expected token is never valid.
func tokenValid(expected, got string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}

// bearerToken returns the token from an "Authorization: Bearer <token>" header,
// or "" if the header is absent or malformed.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}
